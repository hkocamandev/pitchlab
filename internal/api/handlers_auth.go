package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/hkocamandev/pitchlab/internal/auth"
	"github.com/hkocamandev/pitchlab/internal/db"
	"github.com/hkocamandev/pitchlab/internal/db/dbgen"
	"github.com/hkocamandev/pitchlab/internal/httpx"
)

// SessionCookie is where the token lives.
//
// An httpOnly cookie rather than a bearer token in JavaScript, which is a
// deliberate departure from the original design -- and worth explaining.
//
// The design chose a query parameter because a browser's WebSocket API cannot
// send custom headers, so a token held in JavaScript has nowhere else to go.
// But a token in a query parameter ends up in server access logs, browser
// history and Referer headers, which is a well-known anti-pattern. And the
// premise falls away entirely once the token is httpOnly: the browser attaches
// cookies to the WebSocket handshake automatically, so the socket is
// authenticated without JavaScript ever seeing the credential.
//
// That leaves CSRF, which cookies reintroduce and bearer tokens do not. It is
// handled below by SameSite plus an origin check on every state-changing
// request.
const SessionCookie = "pitchlab_session"

// LoginRequest is the credential payload.
type LoginRequest struct {
	Username string `json:"username"`
	Password string `json:"password"`
}

// UserDTO is the authenticated identity as the dashboard sees it.
//
// No password hash, no internal flags. A DTO that mirrors the row would
// eventually leak one of them.
type UserDTO struct {
	ID          string `json:"id"`
	Username    string `json:"username"`
	DisplayName string `json:"display_name"`
	Role        string `json:"role"`
}

// LoginResponse is what a successful login returns.
//
// The token is deliberately absent. It is in the cookie, and putting it in the
// body as well would invite a client to store a copy somewhere JavaScript can
// reach -- which would undo the reason for the cookie.
type LoginResponse struct {
	User      UserDTO   `json:"user"`
	ExpiresAt time.Time `json:"expires_at"`
}

// login authenticates a user and sets the session cookie.
func (s *Server) login(w http.ResponseWriter, r *http.Request) error {
	if s.signer == nil {
		return httpx.ErrUnavailable("authentication is not configured", nil)
	}

	var req LoginRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		return httpx.ErrValidation("body must be a JSON object with a username and a password")
	}

	req.Username = strings.TrimSpace(req.Username)
	if req.Username == "" || req.Password == "" {
		return httpx.ErrValidation("username and password are required")
	}

	// Two keys with two limits. Throttling only by address lets a botnet
	// spread guesses for one account across many addresses; throttling only by
	// username lets one address work through a list of accounts. The username
	// key is the one that actually protects a password, which is why it is the
	// tighter of the two -- an address is shared by everyone behind a NAT, and
	// a limit of five there would lock out a whole office.
	ip := httpx.ClientIP(r)
	for _, throttle := range []struct {
		key   string
		limit int
	}{
		{"user:" + strings.ToLower(req.Username), auth.AccountAttemptLimit},
		{"addr:" + ip, auth.AddressAttemptLimit},
	} {
		allowed, _ := s.limiter.Allow(r.Context(), throttle.key, throttle.limit)
		if !allowed {
			s.log.Warn("login throttled",
				"username", req.Username, "remote", ip,
				"key_kind", strings.Split(throttle.key, ":")[0],
				"limit", throttle.limit)
			return httpx.ErrTooManyRequests(
				"too many login attempts; try again in a few minutes")
		}
	}

	user, err := s.store.GetUserByUsername(r.Context(), req.Username)
	if err != nil {
		if db.IsNoRows(err) {
			// Burn the same work a real verification costs. Without this a
			// missing user answers in microseconds and a real one in fifty
			// milliseconds, and the difference enumerates accounts -- which
			// would make the identical error message below pointless.
			auth.DummyVerify(req.Password)
			return s.rejectLogin(r, req.Username, ip, "no such user")
		}
		return httpx.ErrInternal(err)
	}

	if !user.IsActive {
		auth.DummyVerify(req.Password)
		return s.rejectLogin(r, req.Username, ip, "account disabled")
	}

	ok, err := auth.VerifyPassword(req.Password, user.PasswordHash)
	if err != nil {
		// An unparseable stored hash is an operational fault, not a wrong
		// password, and it must not be reported to the client as one.
		s.log.Error("stored password hash could not be parsed",
			"user_id", user.ID, "error", err)
		return httpx.ErrInternal(err)
	}
	if !ok {
		return s.rejectLogin(r, req.Username, ip, "wrong password")
	}

	token, claims, err := s.signer.Issue(user.ID, user.Username, user.Role)
	if err != nil {
		return httpx.ErrInternal(err)
	}

	// Only now is the throttle cleared, and only for the username: somebody
	// who eventually remembers their password should not stay locked out, but
	// an address that guessed its way to one success has not earned a reset.
	s.limiter.Reset(r.Context(), "user:"+strings.ToLower(req.Username))

	if err := s.store.RecordLogin(r.Context(), user.ID); err != nil {
		// A failed bookkeeping write must not fail a valid login.
		s.log.Warn("could not record login time", "user_id", user.ID, "error", err)
	}

	http.SetCookie(w, s.sessionCookie(r, token, claims.Expiry()))

	s.log.Info("login succeeded",
		"user_id", user.ID, "username", user.Username,
		"role", user.Role, "remote", ip, "token_id", claims.TokenID)

	httpx.WriteJSON(w, r, http.StatusOK, LoginResponse{
		User:      toUserDTO(user),
		ExpiresAt: claims.Expiry(),
	})
	return nil
}

// rejectLogin returns the one failure message every login failure gets.
//
// "No such user", "wrong password" and "account disabled" are the same
// response. Telling them apart is exactly what an attacker wants: the first
// distinction enumerates accounts, and the third confirms one exists.
func (s *Server) rejectLogin(r *http.Request, username, ip, reason string) error {
	s.log.Warn("login failed",
		"username", username, "remote", ip, "reason", reason)
	return httpx.ErrUnauthorized("invalid username or password")
}

// logout clears the session cookie.
func (s *Server) logout(w http.ResponseWriter, r *http.Request) error {
	// Expired in the past with an empty value, which is how a cookie is
	// deleted. The token itself stays valid until it expires -- see the note
	// on revocation in the phase document.
	cookie := s.sessionCookie(r, "", time.Unix(0, 0))
	cookie.MaxAge = -1
	http.SetCookie(w, cookie)

	if claims, ok := ClaimsFrom(r.Context()); ok {
		s.log.Info("logout", "user_id", claims.Subject, "token_id", claims.TokenID)
	}

	httpx.WriteJSON(w, r, http.StatusOK, map[string]string{"status": "signed out"})
	return nil
}

// currentUser reports who the caller is.
//
// The dashboard calls this on load to decide between the login page and the
// application, which is why it answers 401 rather than redirecting.
func (s *Server) currentUser(w http.ResponseWriter, r *http.Request) error {
	claims, ok := ClaimsFrom(r.Context())
	if !ok {
		return httpx.ErrUnauthorized("not signed in")
	}

	user, err := s.store.GetUser(r.Context(), claims.Subject)
	if err != nil {
		if db.IsNoRows(err) {
			// The token is valid but the account is gone. Treating this as
			// unauthenticated rather than as an error is what makes a deleted
			// account stop working immediately.
			return httpx.ErrUnauthorized("account no longer exists")
		}
		return httpx.ErrInternal(err)
	}
	if !user.IsActive {
		return httpx.ErrUnauthorized("account is disabled")
	}

	httpx.WriteJSON(w, r, http.StatusOK, map[string]any{
		"user":       toUserDTO(user),
		"expires_at": claims.Expiry(),
	})
	return nil
}

// sessionCookie builds the cookie carrying the token.
func (s *Server) sessionCookie(r *http.Request, value string, expires time.Time) *http.Cookie {
	return &http.Cookie{
		Name:  SessionCookie,
		Value: value,
		Path:  "/",
		// Not readable from JavaScript. This is the whole point: a cross-site
		// scripting bug can act as the user while the page is open, but it
		// cannot read the credential and send it somewhere that outlives the
		// page.
		HttpOnly: true,
		// Only sent over TLS when the request arrived over TLS. Setting it
		// unconditionally would break local development by making the browser
		// withhold the cookie it just received.
		Secure: httpx.IsSecureRequest(r),
		// Strict, not Lax: no cross-site request carries this cookie at all,
		// which removes the ordinary CSRF vector outright. The cost is that
		// following a link from another site lands on the login page, and for
		// a dashboard nobody deep-links into, that cost is nothing.
		SameSite: http.SameSiteStrictMode,
		Expires:  expires,
		MaxAge:   int(time.Until(expires).Seconds()),
	}
}

func toUserDTO(u dbgen.User) UserDTO {
	return UserDTO{
		ID:          u.ID.String(),
		Username:    u.Username,
		DisplayName: u.DisplayName,
		Role:        u.Role,
	}
}

var _ = context.Background
var _ = errors.Is
