package auth

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
)

// TokenLifetime is how long an issued token stays valid.
//
// One day, and no refresh token. A refresh flow exists to make access tokens
// short -- minutes -- so that a stolen one expires before it is useful, at the
// cost of a second credential to store, rotate and revoke. Twenty-four hours
// with no refresh is the honest middle for a single-user dashboard: long
// enough that nobody logs in twice a day, short enough that a leaked token is
// not permanent, and simple enough that there is one thing to reason about
// instead of two.
const TokenLifetime = 24 * time.Hour

// Issuer identifies the minting service inside the token.
const Issuer = "pitchlab"

// Token errors. All of them are deliberately indistinguishable to the caller
// that renders a response: the client is told "not authenticated", never which
// part failed.
var (
	ErrMalformedToken = errors.New("token is malformed")
	ErrBadSignature   = errors.New("token signature does not verify")
	ErrTokenExpired   = errors.New("token has expired")
	ErrWrongIssuer    = errors.New("token was not issued by this service")
	ErrWeakSecret     = errors.New("signing secret is too weak")
)

// MinSecretLength is the shortest signing key this build accepts.
//
// HS256 keys shorter than the hash output add no security beyond their own
// length, and a short key is guessable offline from a single captured token.
// Thirty-two bytes matches the HMAC-SHA256 block the key feeds.
const MinSecretLength = 32

// Claims is the payload this service issues and accepts.
//
// Hand-rolled rather than a JWT library, and that is a deliberate trade worth
// defending. The argument for a library is that JWT has real footguns; the
// argument against is that most of them come from the library's flexibility --
// the `alg` header being attacker-controlled, `alg: none` being accepted,
// asymmetric keys being verified as symmetric. This implementation supports
// exactly one algorithm and never reads the algorithm from the token, so those
// classes of bug cannot occur. What is left is about eighty lines that can be
// read in full.
type Claims struct {
	Subject   uuid.UUID `json:"sub"`
	Username  string    `json:"preferred_username"`
	Role      string    `json:"role"`
	Issuer    string    `json:"iss"`
	IssuedAt  int64     `json:"iat"`
	ExpiresAt int64     `json:"exp"`
	// TokenID makes an individual token identifiable in logs without the
	// token itself ever being logged.
	TokenID string `json:"jti"`
}

// Signer issues and verifies tokens.
type Signer struct {
	secret []byte
	now    func() time.Time
}

// NewSigner builds a signer, refusing a secret that is too short.
//
// Refusing at startup rather than warning: a service that boots with a
// four-character signing key is a service whose tokens can be forged, and it
// will not be noticed by anything else.
func NewSigner(secret string) (*Signer, error) {
	if len(secret) < MinSecretLength {
		return nil, fmt.Errorf(
			"%w: %d bytes, need at least %d", ErrWeakSecret,
			len(secret), MinSecretLength)
	}
	for _, weak := range []string{
		"changeme", "secret", "password", "pitchlab", "development", "test",
	} {
		if strings.Contains(strings.ToLower(secret), weak) {
			return nil, fmt.Errorf(
				"%w: it contains %q, which means it is a placeholder",
				ErrWeakSecret, weak)
		}
	}
	return &Signer{secret: []byte(secret), now: time.Now}, nil
}

// GenerateSecret returns a random secret suitable for signing.
func GenerateSecret() (string, error) {
	buf := make([]byte, MinSecretLength)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return hex.EncodeToString(buf), nil
}

// Issue mints a token for a user.
func (s *Signer) Issue(userID uuid.UUID, username, role string) (string, Claims, error) {
	now := s.now()

	jti, err := uuid.NewV7()
	if err != nil {
		return "", Claims{}, fmt.Errorf("generate token id: %w", err)
	}

	claims := Claims{
		Subject:   userID,
		Username:  username,
		Role:      role,
		Issuer:    Issuer,
		IssuedAt:  now.Unix(),
		ExpiresAt: now.Add(TokenLifetime).Unix(),
		TokenID:   jti.String(),
	}

	payload, err := json.Marshal(claims)
	if err != nil {
		return "", Claims{}, fmt.Errorf("encode claims: %w", err)
	}

	// The header is a constant, not built from the claims. There is one
	// algorithm and it is never negotiable.
	header := base64.RawURLEncoding.EncodeToString(
		[]byte(`{"alg":"HS256","typ":"JWT"}`))
	body := base64.RawURLEncoding.EncodeToString(payload)

	signing := header + "." + body
	signature := base64.RawURLEncoding.EncodeToString(s.sign(signing))

	return signing + "." + signature, claims, nil
}

// Verify checks a token and returns its claims.
//
// The order matters: signature first, then expiry. Reading the claims of a
// token whose signature has not been checked means acting on attacker-supplied
// data, even if only to decide it is expired.
func (s *Signer) Verify(token string) (Claims, error) {
	var claims Claims

	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return claims, ErrMalformedToken
	}

	signing := parts[0] + "." + parts[1]

	want, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		return claims, ErrMalformedToken
	}

	// Constant time, for the same reason the password comparison is.
	if subtle.ConstantTimeCompare(s.sign(signing), want) != 1 {
		return claims, ErrBadSignature
	}

	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return claims, ErrMalformedToken
	}
	if err := json.Unmarshal(payload, &claims); err != nil {
		return claims, ErrMalformedToken
	}

	if claims.Issuer != Issuer {
		return claims, ErrWrongIssuer
	}

	now := s.now()
	if now.Unix() >= claims.ExpiresAt {
		return claims, ErrTokenExpired
	}
	// A token issued in the future is either a clock problem or a forgery
	// attempt; sixty seconds of slack covers the former.
	if claims.IssuedAt > now.Add(time.Minute).Unix() {
		return claims, ErrMalformedToken
	}

	return claims, nil
}

func (s *Signer) sign(signing string) []byte {
	mac := hmac.New(sha256.New, s.secret)
	mac.Write([]byte(signing))
	return mac.Sum(nil)
}

// ExpiresAt returns the expiry of a set of claims.
func (c Claims) Expiry() time.Time { return time.Unix(c.ExpiresAt, 0) }
