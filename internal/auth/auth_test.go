package auth

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

const testSecret = "f1a9c0e7b4d2836a5f0e1c7d9b3a48625e0f7c1d8b6a4932e5c0d7f1a8b3e6c4"

// --- password hashing -----------------------------------------------------

func TestHashAndVerify(t *testing.T) {
	hash, err := HashPassword("correct horse battery staple")
	if err != nil {
		t.Fatalf("hash: %v", err)
	}

	ok, err := VerifyPassword("correct horse battery staple", hash)
	if err != nil || !ok {
		t.Fatalf("the right password did not verify: ok=%v err=%v", ok, err)
	}

	ok, err = VerifyPassword("Correct horse battery staple", hash)
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	if ok {
		t.Fatal("a one-character difference verified")
	}
}

func TestHashIsSaltedPerPassword(t *testing.T) {
	a, _ := HashPassword("same password")
	b, _ := HashPassword("same password")

	// Without a per-password salt, identical passwords produce identical
	// hashes -- and a single rainbow table breaks every account at once.
	if a == b {
		t.Fatal("the same password produced the same hash twice")
	}
	// Both still verify, which is what the salt travelling inside the hash is
	// for.
	for _, h := range []string{a, b} {
		if ok, _ := VerifyPassword("same password", h); !ok {
			t.Fatal("a salted hash did not verify")
		}
	}
}

func TestHashCarriesItsOwnParameters(t *testing.T) {
	hash, _ := HashPassword("x")

	// The parameters are in the string, not in a constant the verifier reads.
	// That is what lets the cost be raised later without invalidating every
	// existing password.
	if !strings.HasPrefix(hash, "$argon2id$v=19$m=65536,t=3,p=4$") {
		t.Fatalf("hash does not carry its parameters: %s", hash)
	}
	// Six fields: "", algorithm, version, params, salt, hash.
	if n := len(strings.Split(hash, "$")); n != 6 {
		t.Fatalf("hash has %d fields, want 6", n)
	}
}

func TestOldParametersStillVerify(t *testing.T) {
	// A hash produced with a lower cost must keep verifying after the cost is
	// raised, because the parameters come from the stored string rather than
	// from a constant the verifier reads.
	//
	// Generated here rather than pasted as a literal, so the test does not
	// depend on a value somebody would have to regenerate by hand.
	legacy := hashWith("legacy password", 32*1024, 2, 2)

	ok, err := VerifyPassword("legacy password", legacy)
	if err != nil {
		t.Fatalf("a hash with older parameters failed to parse: %v", err)
	}
	if !ok {
		t.Fatal("a hash with older parameters did not verify")
	}
}

func TestMalformedHashIsRejectedNotAccepted(t *testing.T) {
	for _, bad := range []string{
		"",
		"plaintext",
		"$2a$10$abcdefghijklmnopqrstuv", // bcrypt
		"$argon2i$v=19$m=65536,t=3,p=4$c2FsdA$aGFzaA", // wrong variant
		"$argon2id$v=19$m=65536,t=3,p=4$not-base64$aGFzaA",
	} {
		ok, err := VerifyPassword("anything", bad)
		if ok {
			t.Errorf("a malformed hash accepted a password: %q", bad)
		}
		if err == nil {
			t.Errorf("a malformed hash reported no error: %q", bad)
		}
	}
}

func TestDummyVerifyCostsTheSameAsARealOne(t *testing.T) {
	hash, _ := HashPassword("real")

	start := time.Now()
	_, _ = VerifyPassword("guess", hash)
	real := time.Since(start)

	start = time.Now()
	DummyVerify("guess")
	dummy := time.Since(start)

	// A missing user must not answer faster than a wrong password, or the
	// timing enumerates accounts. Loose bounds: this is about orders of
	// magnitude, not microseconds.
	ratio := float64(dummy) / float64(real)
	if ratio < 0.3 || ratio > 3.0 {
		t.Fatalf("dummy verify took %v against a real %v (ratio %.2f)",
			dummy, real, ratio)
	}
}

// --- tokens ---------------------------------------------------------------

func TestIssueAndVerify(t *testing.T) {
	signer := mustSigner(t)
	id := uuid.New()

	token, claims, err := signer.Issue(id, "ada", "admin")
	if err != nil {
		t.Fatalf("issue: %v", err)
	}

	got, err := signer.Verify(token)
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	if got.Subject != id || got.Username != "ada" || got.Role != "admin" {
		t.Fatalf("claims round-tripped wrong: %+v", got)
	}
	if got.TokenID != claims.TokenID {
		t.Fatal("token id changed between issue and verify")
	}
}

func TestTokenLivesForOneDay(t *testing.T) {
	signer := mustSigner(t)
	_, claims, _ := signer.Issue(uuid.New(), "ada", "viewer")

	life := time.Unix(claims.ExpiresAt, 0).Sub(time.Unix(claims.IssuedAt, 0))
	if life != 24*time.Hour {
		t.Fatalf("token lifetime is %v, want 24h", life)
	}
}

func TestExpiredTokenIsRejected(t *testing.T) {
	signer := mustSigner(t)

	// Issue in the past by moving the signer's clock, rather than sleeping.
	signer.now = func() time.Time { return time.Now().Add(-25 * time.Hour) }
	token, _, _ := signer.Issue(uuid.New(), "ada", "viewer")

	signer.now = time.Now
	if _, err := signer.Verify(token); !errors.Is(err, ErrTokenExpired) {
		t.Fatalf("expired token returned %v, want ErrTokenExpired", err)
	}
}

func TestTamperedPayloadIsRejected(t *testing.T) {
	signer := mustSigner(t)
	token, _, _ := signer.Issue(uuid.New(), "ada", "viewer")

	parts := strings.Split(token, ".")
	// Re-encode the payload with an escalated role, keeping the original
	// signature. This is the attack the signature exists to stop.
	escalated := strings.Replace(
		decodeSegment(t, parts[1]), `"role":"viewer"`, `"role":"admin"`, 1)
	parts[1] = encodeSegment(escalated)

	if _, err := signer.Verify(strings.Join(parts, ".")); !errors.Is(err, ErrBadSignature) {
		t.Fatalf("a tampered payload returned %v, want ErrBadSignature", err)
	}
}

func TestAlgNoneIsNotAcceptable(t *testing.T) {
	signer := mustSigner(t)

	// The classic JWT attack: rewrite the header to claim no algorithm and
	// drop the signature. This implementation never reads the algorithm from
	// the token, so the forged token simply fails the signature check.
	header := encodeSegment(`{"alg":"none","typ":"JWT"}`)
	payload := encodeSegment(
		`{"sub":"` + uuid.New().String() + `","role":"admin","iss":"pitchlab",` +
			`"iat":1,"exp":99999999999,"jti":"x","preferred_username":"mallory"}`)

	for _, forged := range []string{
		header + "." + payload + ".",
		header + "." + payload + "." + encodeSegment(""),
		header + "." + payload,
	} {
		if _, err := signer.Verify(forged); err == nil {
			t.Fatalf("an alg:none token was accepted: %q", forged)
		}
	}
}

func TestATokenFromAnotherSecretIsRejected(t *testing.T) {
	mine := mustSigner(t)
	theirs, err := NewSigner(
		"9f8e7d6c5b4a39281706f5e4d3c2b1a0998877665544332211aabbccddeeff00")
	if err != nil {
		t.Fatal(err)
	}

	token, _, _ := theirs.Issue(uuid.New(), "mallory", "admin")
	if _, err := mine.Verify(token); !errors.Is(err, ErrBadSignature) {
		t.Fatalf("a token signed with another key returned %v", err)
	}
}

func TestMalformedTokensAreRejected(t *testing.T) {
	signer := mustSigner(t)

	for _, bad := range []string{
		"", "a", "a.b", "a.b.c.d",
		"not.base64.at-all!",
		strings.Repeat("a", 5000),
	} {
		if _, err := signer.Verify(bad); err == nil {
			t.Errorf("a malformed token was accepted: %q", truncate(bad))
		}
	}
}

// --- signing key ----------------------------------------------------------

func TestWeakSecretsAreRefusedAtStartup(t *testing.T) {
	// A service that boots with a guessable signing key issues forgeable
	// tokens, and nothing else in the system will notice.
	for _, weak := range []string{
		"",
		"short",
		strings.Repeat("a", MinSecretLength-1),
		"changeme-changeme-changeme-changeme-changeme",
		"pitchlab-development-secret-key-for-testing-1234",
		"this-is-a-long-enough-secret-but-says-password",
	} {
		if _, err := NewSigner(weak); !errors.Is(err, ErrWeakSecret) {
			t.Errorf("a weak secret was accepted: %q (err=%v)", weak, err)
		}
	}
}

func TestGeneratedSecretIsAccepted(t *testing.T) {
	secret, err := GenerateSecret()
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	if _, err := NewSigner(secret); err != nil {
		t.Fatalf("a generated secret was refused: %v", err)
	}
	if len(secret) < MinSecretLength {
		t.Fatalf("generated secret is %d characters", len(secret))
	}
}

// --- helpers --------------------------------------------------------------

func mustSigner(t *testing.T) *Signer {
	t.Helper()
	s, err := NewSigner(testSecret)
	if err != nil {
		t.Fatalf("signer: %v", err)
	}
	return s
}

func truncate(s string) string {
	if len(s) > 40 {
		return s[:40] + "..."
	}
	return s
}

// --- login throttling ------------------------------------------------------

// flakyCounter is a remote counter that can be told to stop answering, which
// is the failure the two-layer design exists to survive.
type flakyCounter struct {
	counts map[string]int64
	down   bool
}

func newFlakyCounter() *flakyCounter {
	return &flakyCounter{counts: make(map[string]int64)}
}

func (c *flakyCounter) Increment(_ context.Context, key string, _ time.Duration) (int64, bool) {
	if c.down {
		return 0, false
	}
	c.counts[key]++
	return c.counts[key], true
}

func (c *flakyCounter) Forget(_ context.Context, key string) {
	if c.down {
		return
	}
	delete(c.counts, key)
}

func TestAttemptsAreAllowedUpToTheLimitAndThenRefused(t *testing.T) {
	limiter := NewLimiter(newFlakyCounter())
	ctx := context.Background()

	for i := 1; i <= AccountAttemptLimit; i++ {
		allowed, left := limiter.Allow(ctx, "user:ada", AccountAttemptLimit)
		if !allowed {
			t.Fatalf("attempt %d was refused below the limit", i)
		}
		if want := AccountAttemptLimit - i; left != want {
			t.Errorf("attempt %d reported %d remaining, want %d", i, left, want)
		}
	}

	if allowed, left := limiter.Allow(ctx, "user:ada", AccountAttemptLimit); allowed {
		t.Fatal("an attempt past the limit was allowed")
	} else if left != 0 {
		t.Errorf("a refused attempt reported %d remaining, want 0", left)
	}
}

func TestOneAccountsThrottleDoesNotAffectAnother(t *testing.T) {
	limiter := NewLimiter(newFlakyCounter())
	ctx := context.Background()

	// Otherwise anybody could lock out any account they can name, which turns
	// the protection into the attack.
	for i := 0; i < AccountAttemptLimit+5; i++ {
		limiter.Allow(ctx, "user:ada", AccountAttemptLimit)
	}

	if allowed, _ := limiter.Allow(ctx, "user:grace", AccountAttemptLimit); !allowed {
		t.Fatal("throttling one account throttled another")
	}
}

func TestTheAddressLimitIsLooserThanTheAccountLimit(t *testing.T) {
	// A shared address is not an identity. If these were equal, the sixth
	// person behind one NAT to sign in would be refused.
	if AddressAttemptLimit <= AccountAttemptLimit {
		t.Fatalf("address limit %d does not exceed account limit %d",
			AddressAttemptLimit, AccountAttemptLimit)
	}

	limiter := NewLimiter(newFlakyCounter())
	ctx := context.Background()

	for i := 0; i < AccountAttemptLimit*2; i++ {
		if allowed, _ := limiter.Allow(ctx, "addr:10.0.0.1", AddressAttemptLimit); !allowed {
			t.Fatalf("an address was throttled after %d attempts", i+1)
		}
	}
}

func TestASuccessfulLoginClearsTheThrottle(t *testing.T) {
	limiter := NewLimiter(newFlakyCounter())
	ctx := context.Background()

	for i := 0; i < AccountAttemptLimit; i++ {
		limiter.Allow(ctx, "user:ada", AccountAttemptLimit)
	}
	limiter.Reset(ctx, "user:ada")

	// Reset has to clear the counter, not increment a differently named one --
	// which is what it did when this test was written, and why it exists.
	if allowed, left := limiter.Allow(ctx, "user:ada", AccountAttemptLimit); !allowed {
		t.Fatal("a reset key was still throttled")
	} else if want := AccountAttemptLimit - 1; left != want {
		t.Errorf("after a reset, %d attempts remained; want %d", left, want)
	}
}

func TestThrottlingSurvivesTheRemoteCounterGoingAway(t *testing.T) {
	counter := newFlakyCounter()
	limiter := NewLimiter(counter)
	ctx := context.Background()

	// Redis disappears before any attempt is made. The in-process counter has
	// to take over -- failing open here would mean anyone who can disturb the
	// cache also switches off brute-force protection.
	counter.down = true

	for i := 1; i <= AccountAttemptLimit; i++ {
		if allowed, _ := limiter.Allow(ctx, "user:ada", AccountAttemptLimit); !allowed {
			t.Fatalf("attempt %d was refused below the limit", i)
		}
	}
	if allowed, _ := limiter.Allow(ctx, "user:ada", AccountAttemptLimit); allowed {
		t.Fatal("with the remote counter down, guessing was unlimited")
	}
}

func TestALimiterWithNoRemoteCounterStillThrottles(t *testing.T) {
	limiter := NewLimiter(nil)
	ctx := context.Background()

	for i := 0; i < AccountAttemptLimit; i++ {
		limiter.Allow(ctx, "user:ada", AccountAttemptLimit)
	}
	if allowed, _ := limiter.Allow(ctx, "user:ada", AccountAttemptLimit); allowed {
		t.Fatal("a limiter without Redis allowed unlimited attempts")
	}
}
