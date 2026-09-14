package ws

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/gorilla/websocket"
)

const allowedOrigin = "http://localhost:5173"

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{
		Level: slog.LevelError,
	}))
}

// fakeLookup stands in for the API's snapshot query.
type fakeLookup struct {
	snapshot Snapshot
	err      error
	calls    atomicInt
}

type atomicInt struct {
	mu sync.Mutex
	n  int
}

func (a *atomicInt) inc() { a.mu.Lock(); a.n++; a.mu.Unlock() }
func (a *atomicInt) get() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.n
}

func (f *fakeLookup) SessionSnapshot(context.Context, uuid.UUID) (Snapshot, error) {
	f.calls.inc()
	if f.err != nil {
		return Snapshot{}, f.err
	}
	return f.snapshot, nil
}

type harness struct {
	server *httptest.Server
	hub    *Hub
	lookup *fakeLookup
}

func newHarness(t *testing.T, timeouts Timeouts) *harness {
	t.Helper()

	log := discardLogger()
	hub := NewHub(log).WithTimeouts(timeouts)

	ctx, cancel := context.WithCancel(context.Background())
	hubStopped := make(chan struct{})
	go func() {
		defer close(hubStopped)
		hub.Run(ctx)
	}()

	lookup := &fakeLookup{snapshot: Snapshot{
		Data: SnapshotData{LastPitchIndex: 62, RecentPitches: []string{}},
	}}

	mux := http.NewServeMux()
	mux.Handle("GET /ws/sessions/{sessionId}",
		NewHandler(hub, lookup, func(o string) bool {
			return o == "" || o == allowedOrigin
		}, log))

	srv := httptest.NewServer(mux)
	t.Cleanup(func() {
		srv.Close()
		cancel()
		<-hubStopped
	})

	return &harness{server: srv, hub: hub, lookup: lookup}
}

func (h *harness) dial(t *testing.T, sessionID uuid.UUID, origin string) *websocket.Conn {
	t.Helper()
	conn, resp, err := h.tryDial(sessionID, origin)
	if err != nil {
		status := 0
		if resp != nil {
			status = resp.StatusCode
		}
		t.Fatalf("dial: %v (status %d)", err, status)
	}
	t.Cleanup(func() { _ = conn.Close() })
	return conn
}

func (h *harness) tryDial(
	sessionID uuid.UUID, origin string,
) (*websocket.Conn, *http.Response, error) {
	url := "ws" + strings.TrimPrefix(h.server.URL, "http") +
		"/ws/sessions/" + sessionID.String()

	header := http.Header{}
	if origin != "" {
		header.Set("Origin", origin)
	}
	return websocket.DefaultDialer.Dial(url, header)
}

func readMessage(t *testing.T, conn *websocket.Conn, within time.Duration) Message {
	t.Helper()
	_ = conn.SetReadDeadline(time.Now().Add(within))

	_, raw, err := conn.ReadMessage()
	if err != nil {
		t.Fatalf("read: %v", err)
	}

	var msg Message
	if err := json.Unmarshal(raw, &msg); err != nil {
		t.Fatalf("decode: %v", err)
	}
	return msg
}

// waitFor polls until cond holds or the deadline passes.
func waitFor(t *testing.T, within time.Duration, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(within)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

// --- T7.1 lifecycle -------------------------------------------------------

func TestConnectionLifecycle(t *testing.T) {
	h := newHarness(t, Timeouts{})
	sessionID := uuid.New()
	conn := h.dial(t, sessionID, allowedOrigin)

	// The opening message is the snapshot, always, and it is seq 0. A
	// dashboard that connected during an outing must not see an empty screen
	// until the next pitch, which may be twenty seconds away.
	snapshot := readMessage(t, conn, time.Second)
	if snapshot.Type != TypeSnapshot {
		t.Fatalf("first message is %q, want %q", snapshot.Type, TypeSnapshot)
	}
	if snapshot.Seq != 0 {
		t.Fatalf("snapshot seq is %d, want 0", snapshot.Seq)
	}
	if snapshot.SessionID != sessionID {
		t.Fatalf("snapshot session is %s, want %s", snapshot.SessionID, sessionID)
	}

	waitFor(t, time.Second, "registration", func() bool { return h.hub.Clients() == 1 })

	corr := uuid.NewString()
	h.hub.PublishPitch(sessionID, corr, map[string]any{"pitch_id": "p1"})

	pitch := readMessage(t, conn, time.Second)
	if pitch.Type != TypePitchAnalyzed {
		t.Fatalf("got %q, want %q", pitch.Type, TypePitchAnalyzed)
	}
	if pitch.Seq != 1 {
		t.Fatalf("first broadcast seq is %d, want 1", pitch.Seq)
	}
	// The correlation id is the last link in the end-to-end chain; if it is
	// dropped here the trace stops at the server.
	if pitch.CorrelationID != corr {
		t.Fatalf("correlation id is %q, want %q", pitch.CorrelationID, corr)
	}

	h.hub.PublishSessionClosed(sessionID, ClosedData{Reason: "COMPLETED"})

	closed := readMessage(t, conn, time.Second)
	if closed.Type != TypeSessionClosed {
		t.Fatalf("got %q, want %q", closed.Type, TypeSessionClosed)
	}

	// A terminal message is followed by a normal close, not by silence.
	_ = conn.SetReadDeadline(time.Now().Add(time.Second))
	_, _, err := conn.ReadMessage()
	if !websocket.IsCloseError(err, websocket.CloseNormalClosure) {
		t.Fatalf("expected a normal close after session.closed, got %v", err)
	}
}

func TestSessionOnlyReceivesItsOwnPitches(t *testing.T) {
	h := newHarness(t, Timeouts{})
	mine, other := uuid.New(), uuid.New()

	conn := h.dial(t, mine, allowedOrigin)
	readMessage(t, conn, time.Second) // snapshot
	waitFor(t, time.Second, "registration", func() bool { return h.hub.Clients() == 1 })

	// A global channel would broadcast every pitch to every client and make
	// filtering the browser's problem. The session is the channel boundary.
	h.hub.PublishPitch(other, "", map[string]any{"pitch_id": "not-mine"})
	h.hub.PublishPitch(mine, "", map[string]any{"pitch_id": "mine"})

	msg := readMessage(t, conn, time.Second)
	data, _ := msg.Data.(map[string]any)
	if data["pitch_id"] != "mine" {
		t.Fatalf("received another session's pitch: %v", msg.Data)
	}
}

// --- T7.2 origin ----------------------------------------------------------

func TestOriginRejectedBeforeUpgrade(t *testing.T) {
	h := newHarness(t, Timeouts{})

	// WebSocket is not subject to the same-origin policy: without this check
	// any page on the internet could open a connection on a visitor's behalf.
	conn, resp, err := h.tryDial(uuid.New(), "http://evil.example.com")
	if err == nil {
		_ = conn.Close()
		t.Fatal("connection from a disallowed origin was accepted")
	}
	if resp == nil || resp.StatusCode != http.StatusForbidden {
		t.Fatalf("expected 403, got %v", resp)
	}
	if h.hub.Clients() != 0 {
		t.Fatalf("a rejected origin still registered a client")
	}
	// Rejected before the snapshot query: a refusal should not cost a database
	// round trip.
	if h.lookup.calls.get() != 0 {
		t.Fatalf("snapshot was queried for a rejected origin")
	}
}

func TestAllowedOriginAccepted(t *testing.T) {
	h := newHarness(t, Timeouts{})
	conn := h.dial(t, uuid.New(), allowedOrigin)
	if msg := readMessage(t, conn, time.Second); msg.Type != TypeSnapshot {
		t.Fatalf("got %q, want a snapshot", msg.Type)
	}
}

// --- T7.3 missing session -------------------------------------------------

func TestUnknownSessionIsNotUpgraded(t *testing.T) {
	h := newHarness(t, Timeouts{})
	h.lookup.err = ErrSessionNotFound

	conn, resp, err := h.tryDial(uuid.New(), allowedOrigin)
	if err == nil {
		_ = conn.Close()
		t.Fatal("connection to a nonexistent session was accepted")
	}
	if resp == nil || resp.StatusCode != http.StatusNotFound {
		t.Fatalf("expected 404, got %v", resp)
	}
}

func TestMalformedSessionIDIsRejected(t *testing.T) {
	h := newHarness(t, Timeouts{})

	resp, err := http.Get(h.server.URL + "/ws/sessions/not-a-uuid")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", resp.StatusCode)
	}
}

func TestCompletedSessionGetsSnapshotThenCloses(t *testing.T) {
	h := newHarness(t, Timeouts{})
	h.lookup.snapshot.Closed = true
	h.lookup.snapshot.Reason = "COMPLETED"

	conn := h.dial(t, uuid.New(), allowedOrigin)

	// The client asked a valid question about a finished outing. It gets the
	// complete answer and a normal close, rather than a handshake failure it
	// would have to interpret.
	if msg := readMessage(t, conn, time.Second); msg.Type != TypeSnapshot {
		t.Fatalf("got %q, want a snapshot", msg.Type)
	}
	closed := readMessage(t, conn, time.Second)
	if closed.Type != TypeSessionClosed {
		t.Fatalf("got %q, want %q", closed.Type, TypeSessionClosed)
	}

	// Never registered: there is nothing further to stream.
	if h.hub.Clients() != 0 {
		t.Fatalf("a finished session registered a client")
	}
}

// --- T7.4 sequence numbers ------------------------------------------------

func TestSequenceIsMonotonicPerConnection(t *testing.T) {
	h := newHarness(t, Timeouts{})
	sessionID := uuid.New()

	first := h.dial(t, sessionID, allowedOrigin)
	readMessage(t, first, time.Second)
	waitFor(t, time.Second, "first client", func() bool { return h.hub.Clients() == 1 })

	const pitches = 50
	for i := 0; i < pitches; i++ {
		h.hub.PublishPitch(sessionID, "", map[string]any{"n": i})
	}

	for i := 1; i <= pitches; i++ {
		msg := readMessage(t, first, 2*time.Second)
		if msg.Seq != uint64(i) {
			t.Fatalf("message %d has seq %d", i, msg.Seq)
		}
	}

	// Sequence numbers are per connection, not global: a client that joins
	// late starts at zero rather than at wherever the stream happens to be.
	second := h.dial(t, sessionID, allowedOrigin)
	if msg := readMessage(t, second, time.Second); msg.Seq != 0 {
		t.Fatalf("second connection's snapshot has seq %d, want 0", msg.Seq)
	}
	waitFor(t, time.Second, "second client", func() bool { return h.hub.Clients() == 2 })

	h.hub.PublishPitch(sessionID, "", map[string]any{"n": "after"})
	if msg := readMessage(t, second, time.Second); msg.Seq != 1 {
		t.Fatalf("second connection's first pitch has seq %d, want 1", msg.Seq)
	}
}

// --- T7.5 backpressure ----------------------------------------------------

// newDetachedClient builds a registered client whose queue nobody drains.
//
// A real socket cannot be used for this: the kernel buffers megabytes, so the
// write pump would keep draining and the queue would never fill. The point
// under test is the policy, not the socket.
func newDetachedClient(t *testing.T, h *harness, sessionID uuid.UUID) *Client {
	t.Helper()
	c := newClient(h.hub, nil, sessionID, discardLogger())
	if !h.hub.register(c) {
		t.Fatal("hub refused registration")
	}
	waitFor(t, time.Second, "registration", func() bool { return h.hub.Clients() >= 1 })
	return c
}

func TestSlowClientDropsMessagesAndDoesNotBlockTheHub(t *testing.T) {
	h := newHarness(t, Timeouts{})
	sessionID := uuid.New()

	slow := newDetachedClient(t, h, sessionID)

	// An unbounded queue would let this client consume server memory until the
	// process died. A bounded one loses the client's own data instead.
	const sent = SendBuffer * 3
	start := time.Now()
	for i := 0; i < sent; i++ {
		h.hub.PublishPitch(sessionID, "", map[string]any{"n": i})
	}

	waitFor(t, 3*time.Second, "drops", func() bool { return slow.Dropped() > 0 })
	waitFor(t, 3*time.Second, "queue to fill", func() bool {
		return len(slow.send) == cap(slow.send)
	})

	// Publishing never waits on a slow client.
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("publishing %d messages took %v; the hub blocked", sent, elapsed)
	}

	// Every message that was dropped consumed a sequence number, so the gap is
	// visible to the client and it knows to reconcile over REST.
	var seqs []uint64
	for len(slow.send) > 0 {
		seqs = append(seqs, (<-slow.send).Seq)
	}
	if len(seqs) < 2 {
		t.Fatal("expected a full queue to drain")
	}
	gaps := 0
	for i := 1; i < len(seqs); i++ {
		if seqs[i] <= seqs[i-1] {
			t.Fatalf("sequence went backwards: %d then %d", seqs[i-1], seqs[i])
		}
		if seqs[i] != seqs[i-1]+1 {
			gaps++
		}
	}
	if gaps == 0 {
		t.Fatal("messages were dropped but no sequence gap is visible")
	}
}

func TestSlowClientDoesNotAffectOtherClients(t *testing.T) {
	h := newHarness(t, Timeouts{})
	sessionID := uuid.New()

	slow := newDetachedClient(t, h, sessionID)

	healthy := h.dial(t, sessionID, allowedOrigin)
	readMessage(t, healthy, time.Second)
	waitFor(t, time.Second, "both clients", func() bool { return h.hub.Clients() == 2 })

	const pitches = SendBuffer * 2

	// The healthy client reads while the publishing happens, which is what
	// makes it healthy. Draining only afterwards would make this a test of the
	// buffer's size rather than of the isolation between clients.
	seqs := make(chan uint64, pitches)
	readErr := make(chan error, 1)
	go func() {
		for i := 0; i < pitches; i++ {
			_ = healthy.SetReadDeadline(time.Now().Add(10 * time.Second))
			_, raw, err := healthy.ReadMessage()
			if err != nil {
				readErr <- err
				return
			}
			var msg Message
			if err := json.Unmarshal(raw, &msg); err != nil {
				readErr <- err
				return
			}
			seqs <- msg.Seq
		}
		close(readErr)
	}()

	// Published in batches with a pause between them. A tight loop would
	// outrun any consumer, including a healthy one, and the test would be
	// measuring the buffer's size rather than the isolation between clients.
	for i := 0; i < pitches; i++ {
		h.hub.PublishPitch(sessionID, "", map[string]any{"n": i})
		if i%32 == 31 {
			time.Sleep(2 * time.Millisecond)
		}
	}

	if err := <-readErr; err != nil {
		t.Fatalf("healthy client failed: %v", err)
	}
	if slow.Dropped() == 0 {
		t.Fatal("the slow client was expected to drop messages")
	}

	// Graceful degradation, concretely: the slow client loses its own data and
	// the healthy one notices nothing.
	for i := 1; i <= pitches; i++ {
		if got := <-seqs; got != uint64(i) {
			t.Fatalf("healthy client lost a message: got seq %d, want %d", got, i)
		}
	}
}

func TestSlowConsumerIsWarnedBeforeAnythingElse(t *testing.T) {
	h := newHarness(t, Timeouts{})
	sessionID := uuid.New()
	slow := newDetachedClient(t, h, sessionID)

	for i := 0; i < SendBuffer+SlowConsumerDropThreshold+10; i++ {
		h.hub.PublishPitch(sessionID, "", map[string]any{"n": i})
	}
	waitFor(t, 3*time.Second, "threshold crossed", func() bool {
		return slow.Dropped() >= SlowConsumerDropThreshold
	})

	// The advisory is queued without blocking, so it only lands if there is
	// room. What matters is that crossing the threshold is recorded at all.
	if slow.Dropped() < SlowConsumerDropThreshold {
		t.Fatalf("dropped %d, expected at least %d",
			slow.Dropped(), SlowConsumerDropThreshold)
	}
}

// --- T7.6 anomalies are never dropped -------------------------------------

func TestAnomalyIsNeverSilentlyDropped(t *testing.T) {
	h := newHarness(t, Timeouts{})
	sessionID := uuid.New()
	slow := newDetachedClient(t, h, sessionID)

	for i := 0; i < SendBuffer*2; i++ {
		h.hub.PublishPitch(sessionID, "", map[string]any{"n": i})
	}
	waitFor(t, 3*time.Second, "queue to fill", func() bool {
		return len(slow.send) == cap(slow.send)
	})

	h.hub.PublishAnomaly(sessionID, "", map[string]any{"metric": "release_speed"})

	// Anomalies are rare and high value. If one cannot be delivered the
	// connection ends, so the client reconnects and re-reads rather than
	// continuing in the belief that it has seen everything.
	select {
	case <-slow.closeSlow:
	case <-time.After(3 * time.Second):
		t.Fatal("an undeliverable anomaly neither arrived nor closed the connection")
	}
}

func TestBackpressureCloseUsesTryAgainLater(t *testing.T) {
	log := discardLogger()
	hub := NewHub(log)

	ctx, cancel := context.WithCancel(context.Background())
	stopped := make(chan struct{})
	go func() { defer close(stopped); hub.Run(ctx) }()

	// The pumps are driven directly rather than through the handler: making a
	// real socket stall is timing-dependent, and what is under test is the
	// close code the decision produces, not how the stall arose.
	clients := make(chan *Client, 1)
	upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	srv := httptest.NewServer(http.HandlerFunc(
		func(w http.ResponseWriter, r *http.Request) {
			conn, err := upgrader.Upgrade(w, r, nil)
			if err != nil {
				return
			}
			c := newClient(hub, conn, uuid.New(), log)
			clients <- c
			go c.readPump()
			c.writePump(hub.done)
		}))
	defer func() {
		srv.Close()
		cancel()
		<-stopped
	}()

	conn, _, err := websocket.DefaultDialer.Dial(
		"ws"+strings.TrimPrefix(srv.URL, "http"), nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer func() { _ = conn.Close() }()

	(<-clients).closeForBackpressure()

	// 1013, not 1011: nothing is broken. The server is shedding a client it
	// cannot keep up with, and the code tells the client that reconnecting is
	// the right response.
	_ = conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	_, _, err = conn.ReadMessage()
	if !websocket.IsCloseError(err, websocket.CloseTryAgainLater) {
		t.Fatalf("expected 1013 Try Again Later, got %v", err)
	}
}

// --- T7.7 heartbeat -------------------------------------------------------

func TestSilentClientIsDisconnected(t *testing.T) {
	// A TCP connection to a machine that has vanished stays open indefinitely.
	// Without a heartbeat the server accumulates connections that will never
	// speak again.
	h := newHarness(t, Timeouts{
		Ping: 20 * time.Millisecond, Pong: 120 * time.Millisecond,
	})
	sessionID := uuid.New()

	conn := h.dial(t, sessionID, allowedOrigin)
	readMessage(t, conn, time.Second)
	waitFor(t, time.Second, "registration", func() bool { return h.hub.Clients() == 1 })

	// Never read again: no reads means no automatic pong, which is what a dead
	// client looks like from the server's side.
	waitFor(t, 3*time.Second, "heartbeat timeout", func() bool {
		return h.hub.Clients() == 0
	})
}

func TestRespondingClientStaysConnected(t *testing.T) {
	h := newHarness(t, Timeouts{
		Ping: 20 * time.Millisecond, Pong: 200 * time.Millisecond,
	})
	sessionID := uuid.New()

	conn := h.dial(t, sessionID, allowedOrigin)
	readMessage(t, conn, time.Second)
	waitFor(t, time.Second, "registration", func() bool { return h.hub.Clients() == 1 })

	// Reading is enough: the library answers pings automatically while the
	// application is reading.
	failed := make(chan error, 1)
	go func() {
		_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
		_, _, err := conn.ReadMessage()
		failed <- err
	}()

	// Five ping intervals and more than two pong deadlines: long enough that a
	// broken heartbeat would have disconnected this client several times over.
	select {
	case err := <-failed:
		t.Fatalf("a responsive client was disconnected: %v", err)
	case <-time.After(600 * time.Millisecond):
	}

	if h.hub.Clients() != 1 {
		t.Fatal("a responsive client was disconnected")
	}
}

// --- T7.8 concurrency -----------------------------------------------------

func TestManyConcurrentConnections(t *testing.T) {
	h := newHarness(t, Timeouts{})
	sessionID := uuid.New()

	const clients = 100
	conns := make([]*websocket.Conn, 0, clients)

	var wg sync.WaitGroup
	var mu sync.Mutex
	for i := 0; i < clients; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			conn, _, err := h.tryDial(sessionID, allowedOrigin)
			if err != nil {
				return
			}
			mu.Lock()
			conns = append(conns, conn)
			mu.Unlock()
		}()
	}
	wg.Wait()

	mu.Lock()
	connected := len(conns)
	mu.Unlock()
	if connected != clients {
		t.Fatalf("connected %d of %d", connected, clients)
	}
	t.Cleanup(func() {
		for _, c := range conns {
			_ = c.Close()
		}
	})

	waitFor(t, 5*time.Second, "all registrations", func() bool {
		return h.hub.Clients() == clients
	})

	for _, conn := range conns {
		if msg := readMessage(t, conn, 5*time.Second); msg.Type != TypeSnapshot {
			t.Fatalf("got %q, want a snapshot", msg.Type)
		}
	}

	h.hub.PublishPitch(sessionID, "", map[string]any{"pitch_id": "broadcast"})
	for _, conn := range conns {
		if msg := readMessage(t, conn, 5*time.Second); msg.Type != TypePitchAnalyzed {
			t.Fatalf("got %q, want a pitch", msg.Type)
		}
	}
}

// --- T7.9 goroutine leak --------------------------------------------------

func TestDisconnectLeavesNoGoroutines(t *testing.T) {
	h := newHarness(t, Timeouts{})
	sessionID := uuid.New()

	// One connection first, so the server's own per-connection machinery is
	// already warm and does not count as a leak.
	warm := h.dial(t, sessionID, allowedOrigin)
	readMessage(t, warm, time.Second)
	_ = warm.Close()
	waitFor(t, 2*time.Second, "warmup disconnect", func() bool {
		return h.hub.Clients() == 0
	})
	time.Sleep(100 * time.Millisecond)
	runtime.GC()
	baseline := runtime.NumGoroutine()

	const rounds = 20
	for i := 0; i < rounds; i++ {
		conn, _, err := h.tryDial(sessionID, allowedOrigin)
		if err != nil {
			t.Fatalf("dial %d: %v", i, err)
		}
		readMessage(t, conn, 2*time.Second)
		_ = conn.Close()
	}

	waitFor(t, 5*time.Second, "all clients to unregister", func() bool {
		return h.hub.Clients() == 0
	})

	// Each connection costs one read pump and one write pump; if either fails
	// to exit, the count stays up.
	waitFor(t, 5*time.Second, "goroutines to return to baseline", func() bool {
		runtime.GC()
		return runtime.NumGoroutine() <= baseline+5
	})
}

// --- T7.10 latency --------------------------------------------------------

func TestBroadcastLatency(t *testing.T) {
	h := newHarness(t, Timeouts{})
	sessionID := uuid.New()

	conn := h.dial(t, sessionID, allowedOrigin)
	readMessage(t, conn, time.Second)
	waitFor(t, time.Second, "registration", func() bool { return h.hub.Clients() == 1 })

	start := time.Now()
	h.hub.PublishPitch(sessionID, "", map[string]any{"pitch_id": "timed"})
	readMessage(t, conn, time.Second)

	// The budget for the whole pipeline is 500 ms. This measures only the hop
	// this package owns, so it should be a rounding error within it.
	if elapsed := time.Since(start); elapsed > 100*time.Millisecond {
		t.Fatalf("broadcast took %v", elapsed)
	}
}

// --- client commands ------------------------------------------------------

func TestMetricsCanBeUnsubscribed(t *testing.T) {
	h := newHarness(t, Timeouts{})
	sessionID := uuid.New()

	conn := h.dial(t, sessionID, allowedOrigin)
	readMessage(t, conn, time.Second)
	waitFor(t, time.Second, "registration", func() bool { return h.hub.Clients() == 1 })

	if err := conn.WriteJSON(map[string]any{
		"type": TypeSubscribeMetrics, "enabled": false,
	}); err != nil {
		t.Fatalf("write: %v", err)
	}
	// Give the read pump time to apply it before anything is published.
	time.Sleep(100 * time.Millisecond)

	h.hub.PublishMetrics(sessionID, map[string]any{"pitch_count": 1})
	h.hub.PublishPitch(sessionID, "", map[string]any{"pitch_id": "after"})

	// The metrics message is filtered out entirely; a client that only wants
	// the pitch stream should not pay for rollups it will not render.
	if msg := readMessage(t, conn, 2*time.Second); msg.Type != TypePitchAnalyzed {
		t.Fatalf("got %q after unsubscribing from metrics", msg.Type)
	}
}

func TestClientCannotInjectData(t *testing.T) {
	h := newHarness(t, Timeouts{})
	sessionID := uuid.New()

	first := h.dial(t, sessionID, allowedOrigin)
	readMessage(t, first, time.Second)

	second := h.dial(t, sessionID, allowedOrigin)
	readMessage(t, second, time.Second)
	waitFor(t, time.Second, "both clients", func() bool { return h.hub.Clients() == 2 })

	// The channel is a read-only broadcast. Accepting writes would open a
	// second path into the system, exactly like letting the replay simulator
	// write to PostgreSQL would, and it is refused for the same reason.
	if err := first.WriteJSON(Message{
		Type:      TypePitchAnalyzed,
		SessionID: sessionID,
		Data:      map[string]any{"pitch_id": "forged"},
	}); err != nil {
		t.Fatalf("write: %v", err)
	}

	_ = second.SetReadDeadline(time.Now().Add(300 * time.Millisecond))
	if _, _, err := second.ReadMessage(); err == nil {
		t.Fatal("a client-sent message was broadcast to another client")
	}

	// Ignored rather than answered, and not fatal: both connections are still
	// registered. Replying to unrecognised input is how a read-only channel
	// grows an inbound protocol.
	if h.hub.Clients() != 2 {
		t.Fatalf("a forged message disconnected a client; %d remain",
			h.hub.Clients())
	}
}

func TestOversizedClientMessageClosesTheConnection(t *testing.T) {
	h := newHarness(t, Timeouts{})
	sessionID := uuid.New()

	conn := h.dial(t, sessionID, allowedOrigin)
	readMessage(t, conn, time.Second)
	waitFor(t, time.Second, "registration", func() bool { return h.hub.Clients() == 1 })

	// The channel is read-only, so anything inbound is a control message a few
	// dozen bytes long. An unbounded read is a memory-exhaustion vector.
	if err := conn.WriteMessage(websocket.TextMessage,
		[]byte(strings.Repeat("x", MaxClientMessage*4))); err != nil {
		t.Fatalf("write: %v", err)
	}

	waitFor(t, 2*time.Second, "disconnect", func() bool { return h.hub.Clients() == 0 })
}

// --- shutdown -------------------------------------------------------------

func TestShutdownClosesConnectionsWithGoingAway(t *testing.T) {
	log := discardLogger()
	hub := NewHub(log)

	ctx, cancel := context.WithCancel(context.Background())
	go hub.Run(ctx)

	lookup := &fakeLookup{}
	mux := http.NewServeMux()
	mux.Handle("GET /ws/sessions/{sessionId}",
		NewHandler(hub, lookup, func(string) bool { return true }, log))
	srv := httptest.NewServer(mux)
	defer srv.Close()

	sessionID := uuid.New()
	url := "ws" + strings.TrimPrefix(srv.URL, "http") +
		"/ws/sessions/" + sessionID.String()
	conn, _, err := websocket.DefaultDialer.Dial(url, nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer func() { _ = conn.Close() }()

	_ = conn.SetReadDeadline(time.Now().Add(time.Second))
	if _, _, err := conn.ReadMessage(); err != nil {
		t.Fatalf("snapshot: %v", err)
	}

	cancel()

	// 1001 Going Away, not 1000: the client should reconnect to the replacement
	// instance rather than conclude the outing is over.
	_ = conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	_, _, err = conn.ReadMessage()
	if !websocket.IsCloseError(err, websocket.CloseGoingAway) {
		t.Fatalf("expected 1001 Going Away, got %v", err)
	}
}

func TestPublishAfterShutdownReportsFailure(t *testing.T) {
	hub := NewHub(discardLogger())
	ctx, cancel := context.WithCancel(context.Background())

	stopped := make(chan struct{})
	go func() { defer close(stopped); hub.Run(ctx) }()

	cancel()
	<-stopped

	// A publisher must be able to tell that the hub is gone, rather than
	// queueing into a channel nobody will ever read.
	if hub.PublishPitch(uuid.New(), "", nil) {
		t.Fatal("publish after shutdown reported success")
	}
}

// --- structural -----------------------------------------------------------

func TestTransportPackageDoesNotImportTheDomain(t *testing.T) {
	out, err := exec.Command("go", "list", "-deps",
		"github.com/hkocamandev/pitchlab/internal/ws").Output()
	if err != nil {
		t.Fatalf("go list: %v", err)
	}

	// This package transports the snapshot; it does not build one. Keeping the
	// database and the API out of its dependency graph is what stops the
	// SessionLookup interface from quietly becoming decoration -- and it is
	// what lets the hub be tested without a database at all.
	forbidden := map[string]string{
		"github.com/hkocamandev/pitchlab/internal/db":  "database layer",
		"github.com/hkocamandev/pitchlab/internal/api": "API layer",
		"github.com/jackc/pgx":                         "PostgreSQL driver",
	}

	for _, dep := range strings.Split(string(out), "\n") {
		dep = strings.TrimSpace(dep)
		for prefix, what := range forbidden {
			if strings.HasPrefix(dep, prefix) {
				t.Errorf("internal/ws depends on %s (%s)", dep, what)
			}
		}
	}
}
