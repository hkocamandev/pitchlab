package api_test

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/hkocamandev/pitchlab/internal/api"
	"github.com/hkocamandev/pitchlab/internal/auth"
	"github.com/hkocamandev/pitchlab/internal/config"
)

// These tests exercise the authorisation boundary, which is the part of the
// request path that runs before any handler and therefore before the database
// is touched. They deliberately use no store: a test that needs a database to
// prove a request was refused is testing the wrong thing, and would let a
// rejection that quietly ran a query still pass.

// Deliberately free of the words NewSigner refuses -- it rejects anything
// containing "secret", "test" or "changeme" as a placeholder, which is a guard
// worth keeping and not worth working around.
const testSecret = "9f3c1a7e5b2d84c60ae1f7b39d52c8e04a6b1f7c3e9d2a58"

func newAuthServer(t *testing.T) (*httptest.Server, *auth.Signer) {
	t.Helper()

	signer, err := auth.NewSigner(testSecret)
	if err != nil {
		t.Fatalf("signer: %v", err)
	}

	cfg := config.API{
		Addr:            ":0",
		DatabaseURL:     "postgres://localhost/unused",
		AllowedOrigins:  []string{"http://localhost:5173"},
		DefaultPageSize: 50,
		MaxPageSize:     500,
		ReadTimeout:     10 * time.Second,
		LogLevel:        "error",
		LogFormat:       "json",
		Version:         "test",
		JWTSecret:       testSecret,
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("test config invalid: %v", err)
	}

	log := slog.New(slog.NewJSONHandler(io.Discard, nil))
	srv := httptest.NewServer(
		api.New(cfg, nil, log).
			WithAuth(signer, auth.NewLimiter(nil)).
			Routes(),
	)
	t.Cleanup(srv.Close)

	return srv, signer
}

// do sends a request without following redirects or attaching cookies.
func do(t *testing.T, req *http.Request) *http.Response {
	t.Helper()
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	t.Cleanup(func() { _ = resp.Body.Close() })
	return resp
}

func request(t *testing.T, method, url string, body string) *http.Request {
	t.Helper()
	var r io.Reader
	if body != "" {
		r = strings.NewReader(body)
	}
	req, err := http.NewRequest(method, url, r)
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	return req
}

func tokenFor(t *testing.T, signer *auth.Signer, role string) string {
	t.Helper()
	token, _, err := signer.Issue(uuid.New(), "tester", role)
	if err != nil {
		t.Fatalf("issue: %v", err)
	}
	return token
}

// --- the boundary itself ---------------------------------------------------

func TestProtectedRoutesRefuseAnAnonymousCaller(t *testing.T) {
	srv, _ := newAuthServer(t)

	// Every route that returns data, not a sample of them. A list like this is
	// the only way a route added later without s.secure shows up as a failure
	// rather than as an endpoint nobody noticed was open.
	paths := []string{
		"/api/v1/athletes",
		"/api/v1/athletes/" + uuid.New().String(),
		"/api/v1/athletes/" + uuid.New().String() + "/analytics",
		"/api/v1/sessions",
		"/api/v1/sessions/" + uuid.New().String(),
		"/api/v1/sessions/" + uuid.New().String() + "/pitches",
		"/api/v1/pitches/" + uuid.New().String(),
	}

	for _, path := range paths {
		resp := do(t, request(t, http.MethodGet, srv.URL+path, ""))
		if resp.StatusCode != http.StatusUnauthorized {
			t.Errorf("GET %s answered %d for an anonymous caller, want 401",
				path, resp.StatusCode)
		}
	}
}

func TestHealthEndpointsStayOpen(t *testing.T) {
	srv, _ := newAuthServer(t)

	// Liveness and readiness are consumed by the orchestrator, which has no
	// credential. Putting them behind authentication makes a container that is
	// perfectly healthy look dead.
	for _, path := range []string{"/healthz", "/readyz"} {
		resp := do(t, request(t, http.MethodGet, srv.URL+path, ""))
		if resp.StatusCode == http.StatusUnauthorized {
			t.Errorf("GET %s requires authentication", path)
		}
	}
}

func TestAValidTokenPassesTheBoundary(t *testing.T) {
	srv, signer := newAuthServer(t)

	req := request(t, http.MethodGet, srv.URL+"/api/v1/athletes", "")
	req.AddCookie(&http.Cookie{
		Name: api.SessionCookie, Value: tokenFor(t, signer, api.RoleViewer),
	})

	// The handler behind it has no store and will fail; what matters is that
	// it was reached at all, which a 401 would prove it was not.
	if resp := do(t, req); resp.StatusCode == http.StatusUnauthorized {
		t.Fatal("a valid token was refused")
	}
}

func TestABearerTokenIsAlsoAccepted(t *testing.T) {
	srv, signer := newAuthServer(t)

	// The dashboard uses the cookie; this is for curl and for anything that is
	// not a browser and so has nowhere to keep one.
	req := request(t, http.MethodGet, srv.URL+"/api/v1/athletes", "")
	req.Header.Set("Authorization", "Bearer "+tokenFor(t, signer, api.RoleViewer))

	if resp := do(t, req); resp.StatusCode == http.StatusUnauthorized {
		t.Fatal("a valid bearer token was refused")
	}
}

func TestATokenInTheQueryStringIsIgnored(t *testing.T) {
	srv, signer := newAuthServer(t)

	// Accepting one would be convenient and would put the credential into
	// access logs and Referer headers. The reader deliberately does not look
	// here, and this test is what keeps somebody from adding it back.
	url := srv.URL + "/api/v1/athletes?token=" + tokenFor(t, signer, api.RoleViewer)
	if resp := do(t, request(t, http.MethodGet, url, "")); resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("a token in the query string was accepted (%d)", resp.StatusCode)
	}
}

func TestATokenSignedWithAnotherSecretIsRefused(t *testing.T) {
	srv, _ := newAuthServer(t)

	other, err := auth.NewSigner("1c8b47e2f90a6d35b7e14c82f60d9a3e5b28c71f4d69e0a3")
	if err != nil {
		t.Fatalf("signer: %v", err)
	}

	req := request(t, http.MethodGet, srv.URL+"/api/v1/athletes", "")
	req.AddCookie(&http.Cookie{
		Name: api.SessionCookie, Value: tokenFor(t, other, api.RoleAdmin),
	})

	if resp := do(t, req); resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("a foreign signature was accepted (%d)", resp.StatusCode)
	}
}

// --- roles -----------------------------------------------------------------

func TestWriteRoutesRequireTheAdminRole(t *testing.T) {
	srv, signer := newAuthServer(t)

	req := request(t, http.MethodPost,
		srv.URL+"/api/v1/sessions/"+uuid.New().String()+"/close", "")
	req.Header.Set("Origin", "http://localhost:5173")
	req.AddCookie(&http.Cookie{
		Name: api.SessionCookie, Value: tokenFor(t, signer, api.RoleViewer),
	})

	// 403, not 401. The caller is authenticated and re-authenticating will not
	// help; telling them to log in again would send them round a loop.
	if resp := do(t, req); resp.StatusCode != http.StatusForbidden {
		t.Fatalf("a viewer could reach a write route (%d)", resp.StatusCode)
	}
}

func TestAnAdminReachesAWriteRoute(t *testing.T) {
	srv, signer := newAuthServer(t)

	req := request(t, http.MethodPost,
		srv.URL+"/api/v1/sessions/"+uuid.New().String()+"/close", "")
	req.Header.Set("Origin", "http://localhost:5173")
	req.AddCookie(&http.Cookie{
		Name: api.SessionCookie, Value: tokenFor(t, signer, api.RoleAdmin),
	})

	resp := do(t, req)
	if resp.StatusCode == http.StatusForbidden || resp.StatusCode == http.StatusUnauthorized {
		t.Fatalf("an admin was refused a write route (%d)", resp.StatusCode)
	}
}

// --- cross-site requests ---------------------------------------------------

func TestACrossSiteWriteIsRefused(t *testing.T) {
	srv, signer := newAuthServer(t)

	// The cookie is SameSite=Strict, so a browser would not send it here at
	// all. This is the second fence: it holds even if the cookie policy is
	// weakened, and it is the reason a cookie-based session is defensible.
	req := request(t, http.MethodPost,
		srv.URL+"/api/v1/sessions/"+uuid.New().String()+"/close", "")
	req.Header.Set("Origin", "https://evil.example.com")
	req.AddCookie(&http.Cookie{
		Name: api.SessionCookie, Value: tokenFor(t, signer, api.RoleAdmin),
	})

	if resp := do(t, req); resp.StatusCode != http.StatusForbidden {
		t.Fatalf("a cross-site write was allowed (%d)", resp.StatusCode)
	}
}

func TestACrossSiteReadIsNotBlockedByTheOriginCheck(t *testing.T) {
	srv, signer := newAuthServer(t)

	// A GET changes nothing, and CORS already stops the response being read.
	// Refusing it here would break tooling for no gain.
	req := request(t, http.MethodGet, srv.URL+"/api/v1/athletes", "")
	req.Header.Set("Origin", "https://evil.example.com")
	req.AddCookie(&http.Cookie{
		Name: api.SessionCookie, Value: tokenFor(t, signer, api.RoleViewer),
	})

	if resp := do(t, req); resp.StatusCode == http.StatusForbidden {
		t.Fatal("a cross-origin read was refused by the same-origin check")
	}
}

// --- login surface ---------------------------------------------------------

func TestLoginRejectsAMalformedBody(t *testing.T) {
	srv, _ := newAuthServer(t)

	for _, body := range []string{``, `not json`, `{}`, `{"username":"a"}`} {
		req := request(t, http.MethodPost, srv.URL+"/api/v1/auth/login", body)
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Origin", "http://localhost:5173")

		resp := do(t, req)
		if resp.StatusCode != http.StatusBadRequest &&
			resp.StatusCode != http.StatusUnprocessableEntity {
			t.Errorf("login accepted %q with status %d", body, resp.StatusCode)
		}
	}
}

func TestLoginRefusesAnOversizedBody(t *testing.T) {
	srv, _ := newAuthServer(t)

	// Unbounded, this is free memory amplification: a megabyte of "password"
	// would be read, decoded and hashed.
	body := `{"username":"a","password":"` + strings.Repeat("x", 1<<20) + `"}`
	req := request(t, http.MethodPost, srv.URL+"/api/v1/auth/login", body)
	req.Header.Set("Origin", "http://localhost:5173")

	resp := do(t, req)
	if resp.StatusCode == http.StatusOK {
		t.Fatal("an oversized login body was accepted")
	}
}

// --- response headers ------------------------------------------------------

func TestSecurityHeadersArePresentOnEveryResponse(t *testing.T) {
	srv, _ := newAuthServer(t)

	// Checked on a 401 on purpose. Headers applied only on the success path
	// are missing from exactly the responses an attacker sees most of.
	resp := do(t, request(t, http.MethodGet, srv.URL+"/api/v1/athletes", ""))

	for header, want := range map[string]string{
		"X-Content-Type-Options": "nosniff",
		"X-Frame-Options":        "DENY",
		"Referrer-Policy":        "no-referrer",
	} {
		if got := resp.Header.Get(header); got != want {
			t.Errorf("%s = %q, want %q", header, got, want)
		}
	}
	if resp.Header.Get("Content-Security-Policy") == "" {
		t.Error("no Content-Security-Policy header")
	}
}

func TestHSTSIsAbsentOverPlainHTTP(t *testing.T) {
	srv, _ := newAuthServer(t)

	// Sending it over http:// is at best ignored and at worst pins a
	// development host into https-only in the browser's preload state, which
	// is remarkably annoying to undo.
	resp := do(t, request(t, http.MethodGet, srv.URL+"/healthz", ""))
	if got := resp.Header.Get("Strict-Transport-Security"); got != "" {
		t.Fatalf("HSTS sent over plain HTTP: %q", got)
	}
}

func TestHSTSIsSentWhenTheProxyTerminatedTLS(t *testing.T) {
	srv, _ := newAuthServer(t)

	req := request(t, http.MethodGet, srv.URL+"/healthz", "")
	req.Header.Set("X-Forwarded-Proto", "https")

	if got := do(t, req).Header.Get("Strict-Transport-Security"); got == "" {
		t.Fatal("no HSTS behind a TLS-terminating proxy")
	}
}

// --- failing closed --------------------------------------------------------

func TestWithoutASignerProtectedRoutesAreClosedNotOpen(t *testing.T) {
	cfg := config.API{
		Addr:            ":0",
		DatabaseURL:     "postgres://localhost/unused",
		AllowedOrigins:  []string{"http://localhost:5173"},
		DefaultPageSize: 50,
		MaxPageSize:     500,
		ReadTimeout:     10 * time.Second,
		LogLevel:        "error",
		LogFormat:       "json",
		Version:         "test",
		JWTSecret:       testSecret,
	}

	log := slog.New(slog.NewJSONHandler(io.Discard, nil))
	// Note: no WithAuth.
	srv := httptest.NewServer(api.New(cfg, nil, log).Routes())
	t.Cleanup(srv.Close)

	// The single most valuable assertion in this file. A misconfiguration that
	// leaves the signer unset must not serve data to anonymous callers; the
	// only acceptable answers are "unauthorised" and "unavailable".
	resp := do(t, request(t, http.MethodGet, srv.URL+"/api/v1/athletes", ""))
	if resp.StatusCode != http.StatusServiceUnavailable &&
		resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("an unconfigured server answered %d; it must fail closed",
			resp.StatusCode)
	}
}
