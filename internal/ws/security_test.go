package ws

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/gorilla/websocket"

	"github.com/hkocamandev/pitchlab/internal/httpx"
)

// stubAuth accepts or refuses on command.
type stubAuth struct {
	identity Identity
	err      error
	calls    int
}

func (s *stubAuth) Authenticate(*http.Request) (Identity, error) {
	s.calls++
	if s.err != nil {
		return Identity{}, s.err
	}
	return s.identity, nil
}

func newSecuredHarness(
	t *testing.T, auth Authenticator, limit int, timeouts Timeouts,
) *harness {
	t.Helper()

	log := discardLogger()
	hub := NewHub(log).WithTimeouts(timeouts)

	ctx, cancel := context.WithCancel(context.Background())
	stopped := make(chan struct{})
	go func() { defer close(stopped); hub.Run(ctx) }()

	lookup := &fakeLookup{snapshot: Snapshot{
		Data: SnapshotData{LastPitchIndex: 4, RecentPitches: []string{}},
	}}

	handler := NewHandler(hub, lookup, func(o string) bool {
		return o == "" || o == allowedOrigin
	}, log).WithConnectionLimit(limit)
	if auth != nil {
		handler = handler.WithAuthenticator(auth)
	}

	mux := http.NewServeMux()
	mux.Handle("GET /ws/sessions/{sessionId}", handler)

	srv := httptest.NewServer(mux)
	t.Cleanup(func() { srv.Close(); cancel(); <-stopped })

	return &harness{server: srv, hub: hub, lookup: lookup}
}

// --- authentication -------------------------------------------------------

func TestUnauthenticatedConnectionIsRefusedBeforeUpgrade(t *testing.T) {
	auth := &stubAuth{err: httpx.ErrUnauthorized("no session")}
	h := newSecuredHarness(t, auth, 100, Timeouts{})

	conn, resp, err := h.tryDial(uuid.New(), allowedOrigin)
	if err == nil {
		_ = conn.Close()
		t.Fatal("an unauthenticated connection was accepted")
	}
	if resp == nil || resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %v", resp)
	}

	// Refused before the upgrade, so no socket, no goroutines, no buffer -- and
	// the client gets a status it can act on rather than a close code it has to
	// interpret.
	if h.hub.Clients() != 0 {
		t.Fatal("a refused connection registered a client")
	}
	// And before the snapshot query: a rejection should not cost a database
	// round trip.
	if h.lookup.calls.get() != 0 {
		t.Fatal("the snapshot was queried for a rejected connection")
	}
}

func TestAuthenticatedConnectionIsAccepted(t *testing.T) {
	auth := &stubAuth{identity: Identity{
		UserID: "u1", Username: "ada", Role: "viewer",
		ExpiresAt: time.Now().Add(time.Hour),
	}}
	h := newSecuredHarness(t, auth, 100, Timeouts{})

	conn := h.dial(t, uuid.New(), allowedOrigin)
	if msg := readMessage(t, conn, time.Second); msg.Type != TypeSnapshot {
		t.Fatalf("got %q, want a snapshot", msg.Type)
	}
	if auth.calls != 1 {
		t.Fatalf("authenticator was called %d times", auth.calls)
	}
}

func TestOriginIsCheckedBeforeAuthentication(t *testing.T) {
	auth := &stubAuth{identity: Identity{UserID: "u1"}}
	h := newSecuredHarness(t, auth, 100, Timeouts{})

	conn, resp, err := h.tryDial(uuid.New(), "http://evil.example.com")
	if err == nil {
		_ = conn.Close()
		t.Fatal("a disallowed origin was accepted")
	}
	if resp == nil || resp.StatusCode != http.StatusForbidden {
		t.Fatalf("expected 403, got %v", resp)
	}
	// The cheaper check runs first. Authenticating a request that is going to
	// be refused anyway is work done for an attacker.
	if auth.calls != 0 {
		t.Fatal("a disallowed origin reached the authenticator")
	}
}

// --- credential expiry ----------------------------------------------------

func TestConnectionClosesWhenTheCredentialExpires(t *testing.T) {
	// A socket opened a minute before expiry would otherwise live for days.
	// This is the one place where checking only on entry is not enough.
	auth := &stubAuth{identity: Identity{
		UserID: "u1", Username: "ada", Role: "viewer",
		ExpiresAt: time.Now().Add(150 * time.Millisecond),
	}}
	h := newSecuredHarness(t, auth, 100, Timeouts{})

	conn := h.dial(t, uuid.New(), allowedOrigin)
	readMessage(t, conn, time.Second) // snapshot

	_ = conn.SetReadDeadline(time.Now().Add(3 * time.Second))
	_, _, err := conn.ReadMessage()

	// 4401, not 1000 or 1013: the client has to know that reconnecting
	// unchanged will fail and that it needs a fresh credential.
	var closeErr *websocket.CloseError
	if !errors.As(err, &closeErr) {
		t.Fatalf("expected a close error, got %v", err)
	}
	if closeErr.Code != CloseTokenExpired {
		t.Fatalf("closed with %d, want %d", closeErr.Code, CloseTokenExpired)
	}
}

// --- connection limit -----------------------------------------------------

func TestConnectionsPerAddressAreCapped(t *testing.T) {
	const limit = 3
	h := newSecuredHarness(t, nil, limit, Timeouts{})

	sessionID := uuid.New()
	var open []*websocket.Conn

	for i := 0; i < limit; i++ {
		conn, _, err := h.tryDial(sessionID, allowedOrigin)
		if err != nil {
			t.Fatalf("connection %d was refused below the limit: %v", i, err)
		}
		open = append(open, conn)
		readMessage(t, conn, time.Second)
	}

	// One over.
	conn, resp, err := h.tryDial(sessionID, allowedOrigin)
	if err == nil {
		_ = conn.Close()
		t.Fatal("a connection above the limit was accepted")
	}
	if resp == nil || resp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("expected 429, got %v", resp)
	}

	// Closing one frees a slot. Without that, the cap would be a one-way
	// ratchet and a busy dashboard would lock itself out.
	_ = open[0].Close()
	waitFor(t, 3*time.Second, "the slot to be released", func() bool {
		c, _, err := h.tryDial(sessionID, allowedOrigin)
		if err != nil {
			return false
		}
		_ = c.Close()
		return true
	})

	for _, c := range open[1:] {
		_ = c.Close()
	}
}

func TestRefusedConnectionsDoNotConsumeSlots(t *testing.T) {
	// A rejected handshake must give its reservation back, or repeated
	// failures exhaust the cap and lock out the legitimate client -- turning a
	// protection into the denial of service it was meant to prevent.
	auth := &stubAuth{err: httpx.ErrUnauthorized("no session")}
	h := newSecuredHarness(t, auth, 2, Timeouts{})

	for i := 0; i < 10; i++ {
		conn, _, err := h.tryDial(uuid.New(), allowedOrigin)
		if err == nil {
			_ = conn.Close()
			t.Fatal("an unauthenticated connection was accepted")
		}
	}

	auth.err = nil
	auth.identity = Identity{UserID: "u1", ExpiresAt: time.Now().Add(time.Hour)}

	conn, _, err := h.tryDial(uuid.New(), allowedOrigin)
	if err != nil {
		t.Fatalf("a valid connection was refused after ten rejections: %v", err)
	}
	_ = conn.Close()
}

// --- token is never in the URL --------------------------------------------

func TestHandshakeCarriesNoCredentialInTheURL(t *testing.T) {
	auth := &recordingAuth{identity: Identity{
		UserID: "u1", ExpiresAt: time.Now().Add(time.Hour),
	}}
	h := newSecuredHarness(t, auth, 100, Timeouts{})

	conn := h.dial(t, uuid.New(), allowedOrigin)
	defer func() { _ = conn.Close() }()
	readMessage(t, conn, time.Second)

	// The original design put the token in a query parameter. A URL ends up in
	// access logs, browser history and Referer headers, so the credential
	// moved to an httpOnly cookie -- which the browser attaches to the
	// handshake on its own.
	if strings.Contains(auth.lastURL, "token") ||
		strings.Contains(auth.lastURL, "jwt") {
		t.Fatalf("the handshake URL carries a credential: %s", auth.lastURL)
	}
}

type recordingAuth struct {
	identity Identity
	lastURL  string
}

func (a *recordingAuth) Authenticate(r *http.Request) (Identity, error) {
	a.lastURL = r.URL.String()
	return a.identity, nil
}
