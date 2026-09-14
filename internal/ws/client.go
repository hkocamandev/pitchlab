package ws

import (
	"encoding/json"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
	"github.com/gorilla/websocket"
)

// Connection tuning.
const (
	// SendBuffer is how many messages may be queued for one client.
	//
	// Bounded, deliberately. An unbounded queue lets a single slow client
	// consume server memory until the process dies -- ten of them and the
	// answer is an OOM kill. A bounded queue with an explicit drop policy makes
	// the system degrade predictably instead: the slow client loses its own
	// data, and nobody else notices.
	SendBuffer = 256

	// PingInterval and PongWait implement the heartbeat. A TCP connection to a
	// machine that has vanished stays open indefinitely; without a heartbeat
	// the server accumulates connections that will never speak again.
	PingInterval = 30 * time.Second
	PongWait     = 60 * time.Second

	// WriteWait bounds a single write. It is also the "not making progress"
	// detector: a client whose socket buffer is full stalls here, and after
	// this long the connection is torn down rather than held forever.
	WriteWait = 10 * time.Second

	// MaxClientMessage caps an inbound frame. The channel is read-only, so
	// anything a client sends is a control message a few dozen bytes long.
	MaxClientMessage = 1024

	// SlowConsumerDropThreshold is how many drops in a minute earn the client
	// an explicit warning.
	SlowConsumerDropThreshold = 50
)

// Client is one browser connection.
//
// Exactly one goroutine reads the socket and exactly one writes it. This is
// not a stylistic choice: a WebSocket connection supports neither concurrent
// reads nor concurrent writes, and the failure mode for getting it wrong is a
// panic inside the library rather than a corrupted frame.
type Client struct {
	hub       *Hub
	conn      *websocket.Conn
	sessionID uuid.UUID
	log       *slog.Logger
	timeouts  Timeouts

	send chan Message

	// nextSeq is shared because the snapshot is numbered before the hub takes
	// ownership of the client.
	nextSeq atomic.Uint64
	dropped atomic.Uint64

	// metricsEnabled is toggled by the reader and read by the hub, which is
	// the one genuinely shared flag on this struct.
	metricsEnabled atomic.Bool

	// closeSlow is closed when the client must be disconnected for falling too
	// far behind. Closing a channel rather than sending on one means the hub
	// signals without ever blocking.
	closeSlow chan struct{}
	closeOnce sync.Once

	// The fields below are touched only by the hub goroutine.
	dropWindowStart time.Time
	dropWindowCount int
	warnedNearFull  bool
}

// Timeouts group the connection's deadlines.
//
// Grouped and injectable so a test can exercise the heartbeat in milliseconds
// instead of waiting a minute for it. Production uses DefaultTimeouts.
type Timeouts struct {
	Ping  time.Duration
	Pong  time.Duration
	Write time.Duration
}

// DefaultTimeouts returns the production deadlines.
func DefaultTimeouts() Timeouts {
	return Timeouts{Ping: PingInterval, Pong: PongWait, Write: WriteWait}
}

func (t Timeouts) orDefault() Timeouts {
	d := DefaultTimeouts()
	if t.Ping > 0 {
		d.Ping = t.Ping
	}
	if t.Pong > 0 {
		d.Pong = t.Pong
	}
	if t.Write > 0 {
		d.Write = t.Write
	}
	return d
}

func newClient(hub *Hub, conn *websocket.Conn, sessionID uuid.UUID, log *slog.Logger) *Client {
	c := &Client{
		hub:       hub,
		conn:      conn,
		sessionID: sessionID,
		log:       log.With("session_id", sessionID.String()),
		send:      make(chan Message, SendBuffer),
		closeSlow: make(chan struct{}),
		timeouts:  hub.timeouts,
	}
	c.metricsEnabled.Store(true)
	return c
}

// SessionID reports which outing this client is watching.
func (c *Client) SessionID() uuid.UUID { return c.sessionID }

// Dropped reports how many messages this connection has lost.
func (c *Client) Dropped() uint64 { return c.dropped.Load() }

// sendSnapshot queues the opening message.
//
// Called before the client is registered, so nothing else can be racing for a
// sequence number: the snapshot is seq 0 and the first broadcast is seq 1.
func (c *Client) sendSnapshot(data SnapshotData) {
	// Counted like any other message. It is one the server sent, and leaving
	// it out makes the send counter disagree with what the client received --
	// which is worse than not counting at all, because the number looks
	// authoritative.
	c.hub.metrics.sent(TypeSnapshot)
	c.send <- Message{
		Type:      TypeSnapshot,
		Seq:       0,
		TS:        time.Now().UTC(),
		SessionID: c.sessionID,
		Data:      data,
	}
}

// enqueue hands a message to this connection.
//
// Called only from the hub goroutine, and it never blocks -- that is its
// entire contract. One slow client must not slow the broadcast down for every
// other client, so a full queue is resolved here, by policy, rather than by
// waiting.
func (c *Client) enqueue(m Message) {
	if m.Type == TypeSessionMetrics && !c.metricsEnabled.Load() {
		return
	}

	m.Seq = c.nextSeq.Add(1)
	m.TS = time.Now().UTC()
	m.SessionID = c.sessionID

	select {
	case c.send <- m:
		c.hub.metrics.sent(m.Type)
		c.checkNearFull()
		return
	default:
	}

	switch m.Type {
	case TypeAnomalyDetected, TypeSessionClosed:
		// Never dropped. An anomaly is rare and it is the single most valuable
		// thing this channel carries; silently discarding it would defeat the
		// point of detecting it. If it cannot be delivered, the connection is
		// ended so the client reconnects and re-reads over REST, rather than
		// continuing in the belief that it has seen everything.
		c.log.Warn("cannot deliver undroppable message; closing connection",
			"type", m.Type, "seq", m.Seq, "dropped_total", c.dropped.Load())
		c.closeForBackpressure()

	default:
		// The sequence number has already been spent, so the gap is visible to
		// the client and it knows to reconcile.
		total := c.dropped.Add(1)
		c.hub.metrics.dropped(m.Type)
		c.noteDrop()
		c.log.Debug("dropped message", "type", m.Type, "seq", m.Seq,
			"dropped_total", total)
	}
}

// checkNearFull reports a queue approaching its limit.
//
// Logged on the way up and reset on the way down, so a client that is
// permanently borderline produces one line rather than thousands.
func (c *Client) checkNearFull() {
	nearFull := len(c.send)*5 >= cap(c.send)*4
	switch {
	case nearFull && !c.warnedNearFull:
		c.warnedNearFull = true
		c.log.Warn("client send buffer nearly full",
			"queued", len(c.send), "capacity", cap(c.send))
		c.hub.metrics.slowConsumer(1)
	case !nearFull && c.warnedNearFull:
		c.warnedNearFull = false
		c.hub.metrics.slowConsumer(-1)
	}
}

// noteDrop counts drops within a rolling minute and warns the client once it
// crosses the threshold.
//
// The warning is best-effort: it is queued without blocking and skipped if
// there is no room, because the situation it describes is precisely the one
// where there is no room. The action that is not best-effort is the close.
func (c *Client) noteDrop() {
	now := time.Now()
	if now.Sub(c.dropWindowStart) > time.Minute {
		c.dropWindowStart = now
		c.dropWindowCount = 0
	}
	c.dropWindowCount++
	if c.dropWindowCount != SlowConsumerDropThreshold {
		return
	}

	c.log.Warn("slow consumer", "dropped_in_window", c.dropWindowCount,
		"dropped_total", c.dropped.Load())

	notice := Message{
		Type:      TypeError,
		Seq:       c.nextSeq.Add(1),
		TS:        now.UTC(),
		SessionID: c.sessionID,
		Data: ErrorData{
			Code:    CodeSlowConsumer,
			Message: "client is not keeping up; messages are being dropped",
		},
	}
	select {
	case c.send <- notice:
	default:
	}
}

func (c *Client) closeForBackpressure() {
	c.closeOnce.Do(func() { close(c.closeSlow) })
}

// writePump owns the socket's write side.
func (c *Client) writePump(done <-chan struct{}) {
	ticker := time.NewTicker(c.timeouts.Ping)
	defer func() {
		ticker.Stop()
		_ = c.conn.Close()
	}()

	for {
		select {
		case msg, ok := <-c.send:
			if !ok {
				c.writeClose(websocket.CloseNormalClosure, "")
				return
			}
			if err := c.writeJSON(msg); err != nil {
				c.log.Debug("write failed; closing", "error", err, "seq", msg.Seq)
				return
			}
			// A terminal message is terminal: the outing is over and there is
			// nothing further to stream.
			if msg.Type == TypeSessionClosed {
				c.writeClose(websocket.CloseNormalClosure, "session closed")
				return
			}

		case <-ticker.C:
			_ = c.conn.SetWriteDeadline(time.Now().Add(c.timeouts.Write))
			if err := c.conn.WriteMessage(websocket.PingMessage, nil); err != nil {
				c.log.Debug("ping failed; closing", "error", err)
				return
			}

		case <-c.closeSlow:
			// 1013 Try Again Later, not 1011 Internal Error: nothing is broken.
			// The server is shedding a client it cannot keep up with, and the
			// code tells the client that reconnecting is the right response.
			c.writeClose(websocket.CloseTryAgainLater, "client too slow")
			return

		case <-done:
			c.writeClose(websocket.CloseGoingAway, "server shutting down")
			return
		}
	}
}

func (c *Client) writeJSON(msg Message) error {
	_ = c.conn.SetWriteDeadline(time.Now().Add(c.timeouts.Write))
	body, err := json.Marshal(msg)
	if err != nil {
		// Encoding a message must never take the connection down over one bad
		// payload, but it does mean a sequence number went nowhere.
		c.log.Error("encode message", "type", msg.Type, "error", err)
		return nil
	}
	return c.conn.WriteMessage(websocket.TextMessage, body)
}

func (c *Client) writeClose(code int, reason string) {
	_ = c.conn.WriteControl(websocket.CloseMessage,
		websocket.FormatCloseMessage(code, reason),
		time.Now().Add(c.timeouts.Write))
}

// readPump owns the socket's read side.
//
// It exists even though clients have almost nothing to say: without a reader,
// protocol-level pongs are never processed, close frames are never noticed,
// and the heartbeat cannot work.
func (c *Client) readPump() {
	defer func() {
		c.hub.unregister(c)
		_ = c.conn.Close()
	}()

	c.conn.SetReadLimit(MaxClientMessage)
	_ = c.conn.SetReadDeadline(time.Now().Add(c.timeouts.Pong))
	c.conn.SetPongHandler(func(string) error {
		return c.conn.SetReadDeadline(time.Now().Add(c.timeouts.Pong))
	})

	for {
		_, raw, err := c.conn.ReadMessage()
		if err != nil {
			if websocket.IsUnexpectedCloseError(err,
				websocket.CloseNormalClosure, websocket.CloseGoingAway) {
				c.log.Debug("read error", "error", err)
			}
			return
		}
		c.handleCommand(raw)
	}
}

func (c *Client) handleCommand(raw []byte) {
	var cmd clientCommand
	if err := json.Unmarshal(raw, &cmd); err != nil {
		// Ignored, not answered. A read-only channel that replies to malformed
		// input has started to have an inbound protocol.
		c.log.Debug("ignoring unreadable client message")
		return
	}

	switch cmd.Type {
	case TypePong:
		// An application-level fallback for clients that cannot see the
		// protocol-level pong. Either one keeps the connection alive.
		_ = c.conn.SetReadDeadline(time.Now().Add(c.timeouts.Pong))

	case TypeSubscribeMetrics:
		enabled := cmd.Enabled == nil || *cmd.Enabled
		c.metricsEnabled.Store(enabled)
		c.log.Debug("metrics subscription changed", "enabled", enabled)

	default:
		c.log.Debug("ignoring unknown client message", "type", cmd.Type)
	}
}
