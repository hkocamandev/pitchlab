package ws

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/gorilla/websocket"

	"github.com/hkocamandev/pitchlab/internal/httpx"
)

// ErrSessionNotFound is returned by a SessionLookup for an outing that does
// not exist.
var ErrSessionNotFound = errors.New("session not found")

// Snapshot is everything the opening message needs.
type Snapshot struct {
	Data SnapshotData

	// Closed marks an outing that is already over. The connection still
	// upgrades, sends the snapshot and a session.closed, and ends normally --
	// the client asked a valid question and gets the complete answer, rather
	// than a handshake failure it would have to interpret.
	Closed bool
	Reason string
}

// SessionLookup provides the snapshot for one outing.
//
// An interface, so this package never imports the API or the database. It also
// keeps the snapshot's shape the API's business: this package transports it
// and does not interpret it.
type SessionLookup interface {
	SessionSnapshot(ctx context.Context, sessionID uuid.UUID) (Snapshot, error)
}

// HandshakeTimeout bounds how long building a snapshot may take. A handshake
// that hangs on a slow query holds an HTTP connection open for nothing.
const HandshakeTimeout = 5 * time.Second

// MaxConnectionsPerAddress caps how many sockets one address may hold open.
//
// A connection costs a socket, two goroutines and a 256-slot buffer, and
// nothing else stops one client from opening them until the process runs out
// of file descriptors. Twenty is far above what a person with several tabs
// needs and far below what an attack would want.
//
// Keyed by address rather than by user on purpose: an unauthenticated flood is
// the case worth stopping, and by the time a user is known the resources have
// already been committed.
const MaxConnectionsPerAddress = 20

// WithConnectionLimit overrides the per-address cap.
//
// Only tests should need it: they open many sockets from one loopback address,
// which is exactly the shape the cap exists to refuse.
func (h *Handler) WithConnectionLimit(n int) *Handler {
	h.limit = n
	return h
}

// Authenticator decides whether a request may open a socket.
//
// An interface so this package keeps knowing nothing about tokens, users or
// roles -- the same reason SessionLookup exists. A nil authenticator means the
// channel is open, which is what the package tests use and what the service
// refuses to do in production.
type Authenticator interface {
	// Authenticate reports the identity behind a request, or an error that is
	// already shaped as an HTTP problem.
	Authenticate(r *http.Request) (Identity, error)
}

// Identity is the authenticated caller, as much of it as this package needs.
type Identity struct {
	UserID   string
	Username string
	Role     string
	// ExpiresAt is when the credential stops being valid. The connection is
	// closed at that moment rather than living on, because a socket opened
	// with a valid token would otherwise outlive it indefinitely -- a
	// long-lived connection is the one place where "check on entry" is not
	// enough.
	ExpiresAt time.Time
}

// Handler upgrades HTTP requests to the session channel.
type Handler struct {
	hub      *Hub
	lookup   SessionLookup
	auth     Authenticator
	log      *slog.Logger
	upgrader websocket.Upgrader

	// perAddress counts live connections per client address.
	mu         sync.Mutex
	perAddress map[string]int
	limit      int
}

// WithAuthenticator requires a verified identity before upgrading.
func (h *Handler) WithAuthenticator(a Authenticator) *Handler {
	h.auth = a
	return h
}

// NewHandler builds the upgrade handler.
//
// allowOrigin is required. WebSocket is not subject to the same-origin policy,
// so a server that does not check Origin lets any page on the internet open a
// connection on a visitor's behalf -- cross-site WebSocket hijacking. v1 has
// no authentication, and that is a scope decision; it is not a reason to skip
// the one check that does not need any.
func NewHandler(
	hub *Hub, lookup SessionLookup, allowOrigin func(string) bool, log *slog.Logger,
) *Handler {
	if allowOrigin == nil {
		panic("ws: allowOrigin must not be nil")
	}
	return &Handler{
		hub:        hub,
		lookup:     lookup,
		log:        log.With("component", "ws"),
		perAddress: make(map[string]int),
		limit:      MaxConnectionsPerAddress,
		upgrader: websocket.Upgrader{
			HandshakeTimeout: HandshakeTimeout,
			ReadBufferSize:   1024,
			WriteBufferSize:  4096,
			CheckOrigin: func(r *http.Request) bool {
				return allowOrigin(r.Header.Get("Origin"))
			},
		},
	}
}

// ServeHTTP performs the handshake.
//
// Everything that can reject the request is checked before the upgrade: a
// rejected connection should cost an HTTP response, not a socket, two
// goroutines and a 256-slot buffer.
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	sessionID, err := httpx.PathUUID(r, "sessionId")
	if err != nil {
		httpx.WriteProblem(w, r, err)
		return
	}

	// Checked here as well as in CheckOrigin, so a rejected origin produces the
	// same problem document as every other refusal instead of the library's
	// bare 403.
	origin := r.Header.Get("Origin")
	if !h.upgrader.CheckOrigin(r) {
		h.log.Warn("websocket origin rejected",
			"origin", origin, "session_id", sessionID)
		httpx.WriteProblem(w, r, httpx.ErrForbidden("origin is not allowed"))
		return
	}

	// Authentication before the upgrade, not after.
	//
	// After the handshake the only way to refuse is to close the socket, which
	// means the resources were committed to an unauthenticated caller first --
	// and a client that cannot tell a rejection from a network problem will
	// reconnect against it forever. A 401 before the upgrade is unambiguous.
	var identity Identity
	if h.auth != nil {
		var err error
		identity, err = h.auth.Authenticate(r)
		if err != nil {
			h.log.Warn("websocket authentication rejected",
				"session_id", sessionID, "remote", httpx.ClientIP(r))
			httpx.WriteProblem(w, r, err)
			return
		}
	}

	// The address cap is checked before the snapshot query, so a flood costs a
	// comparison rather than a database round trip.
	addr := httpx.ClientIP(r)
	if !h.reserve(addr) {
		h.log.Warn("websocket connection limit reached",
			"remote", addr, "limit", h.limit)
		httpx.WriteProblem(w, r, httpx.ErrTooManyRequests(
			"too many open connections from this address"))
		return
	}
	released := false
	release := func() {
		if !released {
			released = true
			h.release(addr)
		}
	}
	defer release()

	ctx, cancel := context.WithTimeout(r.Context(), HandshakeTimeout)
	defer cancel()

	snapshot, err := h.lookup.SessionSnapshot(ctx, sessionID)
	if err != nil {
		if errors.Is(err, ErrSessionNotFound) {
			httpx.WriteProblem(w, r, httpx.ErrNotFound("session"))
			return
		}
		httpx.WriteProblem(w, r, httpx.ErrInternal(err))
		return
	}

	conn, err := h.upgrader.Upgrade(w, r, nil)
	if err != nil {
		// Upgrade has already written its own response.
		h.log.Debug("upgrade failed", "session_id", sessionID, "error", err)
		return
	}

	client := newClient(h.hub, conn, sessionID, h.log)
	client.identity = identity
	// The reservation now belongs to the connection, not to this handler.
	client.onClose = func() { h.release(addr) }
	released = true
	client.sendSnapshot(snapshot.Data)

	if snapshot.Closed {
		// Never registered, so it never appears in the fan-out: there is
		// nothing further to stream for a finished outing.
		h.serveClosed(client, snapshot)
		return
	}

	if !h.hub.register(client) {
		h.log.Warn("hub not running; refusing connection", "session_id", sessionID)
		client.writeClose(websocket.CloseGoingAway, "server shutting down")
		_ = conn.Close()
		return
	}

	go client.writePump(h.hub.done)
	go client.readPump()
}

// reserve takes a connection slot for an address.
func (h *Handler) reserve(addr string) bool {
	h.mu.Lock()
	defer h.mu.Unlock()

	if h.perAddress[addr] >= h.limit {
		return false
	}
	h.perAddress[addr]++
	return true
}

// release gives a slot back.
func (h *Handler) release(addr string) {
	h.mu.Lock()
	defer h.mu.Unlock()

	if n := h.perAddress[addr] - 1; n > 0 {
		h.perAddress[addr] = n
	} else {
		// Deleted rather than left at zero: addresses are unbounded over a
		// long run, and a map that only grows is a leak that takes weeks to
		// become visible.
		delete(h.perAddress, addr)
	}
}

// serveClosed delivers the snapshot and the terminal message, then hangs up.
func (h *Handler) serveClosed(client *Client, snapshot Snapshot) {
	reason := snapshot.Reason
	if reason == "" {
		reason = "COMPLETED"
	}
	client.send <- Message{
		Type:      TypeSessionClosed,
		Seq:       client.nextSeq.Add(1),
		TS:        time.Now().UTC(),
		SessionID: client.sessionID,
		Data:      ClosedData{Reason: reason, FinalMetrics: snapshot.Data.Metrics},
	}
	close(client.send)

	// The read side still runs: without it the close handshake is never
	// completed and the client waits for a reply that never comes.
	go client.readPump()
	client.writePump(h.hub.done)
}

// PublishPitch broadcasts an analyzed pitch.
func (h *Hub) PublishPitch(sessionID uuid.UUID, correlationID string, data any) bool {
	return h.Publish(sessionID, TypePitchAnalyzed, correlationID, data)
}

// PublishAnomaly broadcasts a detected anomaly.
func (h *Hub) PublishAnomaly(sessionID uuid.UUID, correlationID string, data any) bool {
	return h.Publish(sessionID, TypeAnomalyDetected, correlationID, data)
}

// PublishMetrics broadcasts a rollup.
func (h *Hub) PublishMetrics(sessionID uuid.UUID, data any) bool {
	return h.Publish(sessionID, TypeSessionMetrics, "", data)
}

// PublishSessionClosed broadcasts the terminal message for an outing.
func (h *Hub) PublishSessionClosed(sessionID uuid.UUID, data ClosedData) bool {
	return h.Publish(sessionID, TypeSessionClosed, "", data)
}
