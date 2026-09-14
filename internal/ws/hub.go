package ws

import (
	"context"
	"log/slog"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
)

// BroadcastBuffer bounds the hub's inbound queue.
//
// It absorbs a burst from the Kafka fan-out without making the consumer wait
// on the hub. If it ever fills, the publisher is told so rather than blocked:
// a stalled Kafka consumer is a worse outcome than a missed live notification,
// because the durable record is already in PostgreSQL either way.
const BroadcastBuffer = 1024

// HubMetrics receives counters. Nil callbacks are ignored, so instrumentation
// is optional and the hub stays testable without it.
// Deliberately no session identifier in any of these. A metrics hook that
// hands the caller an unbounded identifier is an invitation to use it as a
// label, and one label per session is one time series per outing. The session
// belongs in the logs, which already carry it.
type HubMetrics struct {
	OnSent         func(messageType string)
	OnDropped      func(messageType string)
	OnSlowConsumer func(delta int)
}

func (m HubMetrics) slowConsumer(delta int) {
	if m.OnSlowConsumer != nil {
		m.OnSlowConsumer(delta)
	}
}

func (m HubMetrics) dropped(messageType string) {
	if m.OnDropped != nil {
		m.OnDropped(messageType)
	}
}

func (m HubMetrics) sent(messageType string) {
	if m.OnSent != nil {
		m.OnSent(messageType)
	}
}

// Hub is the client registry and the broadcast fan-out.
//
// The registry is owned by exactly one goroutine and no other goroutine ever
// touches it. That is what makes the whole package lock-free: there is no
// mutex to forget, no lock ordering to get wrong, and no read lock held across
// a slow send.
//
// The alternative -- a map guarded by an RWMutex -- works, but the read lock
// is held for the duration of a broadcast, so registering a client waits
// behind the slowest send in the loop.
type Hub struct {
	registerCh   chan *Client
	unregisterCh chan *Client
	broadcastCh  chan Message

	// sessions is touched only by run().
	sessions map[uuid.UUID]map[*Client]struct{}

	log     *slog.Logger
	metrics HubMetrics

	clients  atomic.Int64
	done     chan struct{}
	started  atomic.Bool
	timeouts Timeouts
}

// NewHub builds a hub. It does nothing until Run is called.
func NewHub(log *slog.Logger) *Hub {
	return &Hub{
		registerCh:   make(chan *Client),
		unregisterCh: make(chan *Client),
		broadcastCh:  make(chan Message, BroadcastBuffer),
		sessions:     make(map[uuid.UUID]map[*Client]struct{}),
		log:          log.With("component", "ws-hub"),
		done:         make(chan struct{}),
		timeouts:     DefaultTimeouts(),
	}
}

// WithTimeouts overrides the connection deadlines. Only tests should need it.
func (h *Hub) WithTimeouts(t Timeouts) *Hub {
	h.timeouts = t.orDefault()
	return h
}

// WithMetrics attaches counters.
func (h *Hub) WithMetrics(m HubMetrics) *Hub {
	h.metrics = m
	return h
}

// Run owns the registry until the context is cancelled.
func (h *Hub) Run(ctx context.Context) {
	h.started.Store(true)
	defer close(h.done)

	for {
		select {
		case c := <-h.registerCh:
			clients, ok := h.sessions[c.sessionID]
			if !ok {
				clients = make(map[*Client]struct{})
				h.sessions[c.sessionID] = clients
			}
			clients[c] = struct{}{}
			h.clients.Add(1)
			h.log.Debug("client registered",
				"session_id", c.sessionID, "session_clients", len(clients))

		case c := <-h.unregisterCh:
			h.remove(c)

		case msg := <-h.broadcastCh:
			for c := range h.sessions[msg.SessionID] {
				c.enqueue(msg)
			}

		case <-ctx.Done():
			h.shutdown()
			return
		}
	}
}

// remove drops a client from the registry and closes its queue.
//
// Closing the queue here is safe precisely because this runs in the same
// goroutine as every enqueue: once the client is out of the map, no send can
// follow.
func (h *Hub) remove(c *Client) {
	clients, ok := h.sessions[c.sessionID]
	if !ok {
		return
	}
	if _, ok := clients[c]; !ok {
		// Already gone. readPump and a backpressure close can both land here.
		return
	}

	delete(clients, c)
	if len(clients) == 0 {
		// Sessions are unbounded in number and most are short-lived, so an
		// empty map left behind for every outing ever watched is a slow leak.
		delete(h.sessions, c.sessionID)
	}
	close(c.send)
	h.clients.Add(-1)

	h.log.Debug("client unregistered",
		"session_id", c.sessionID, "dropped", c.Dropped())
}

// shutdown clears the registry.
//
// The send channels are deliberately left open: closing the hub's done channel
// is what the write pumps are waiting on, and it lets them close with 1001
// Going Away. Closing the queues instead would look to them like a completed
// session and report 1000, telling clients not to reconnect to a server that
// is merely restarting.
func (h *Hub) shutdown() {
	h.log.Info("hub stopping", "clients", h.clients.Load())
	h.sessions = make(map[uuid.UUID]map[*Client]struct{})
	h.clients.Store(0)
}

// register adds a client, or reports that the hub is no longer running.
func (h *Hub) register(c *Client) bool {
	select {
	case h.registerCh <- c:
		return true
	case <-h.done:
		return false
	}
}

// unregister removes a client. Safe to call for a client that was never
// registered, and safe to call after the hub has stopped.
func (h *Hub) unregister(c *Client) {
	select {
	case h.unregisterCh <- c:
	case <-h.done:
	}
}

// Clients reports the number of live connections.
func (h *Hub) Clients() int64 { return h.clients.Load() }

// Publish queues a message for everyone watching one session.
//
// Non-blocking by contract. The caller is a Kafka consumer whose offset must
// keep advancing; making it wait on a dashboard would turn a slow browser into
// consumer lag.
func (h *Hub) Publish(sessionID uuid.UUID, msgType, correlationID string, data any) bool {
	msg := Message{
		Type:          msgType,
		TS:            time.Now().UTC(),
		SessionID:     sessionID,
		CorrelationID: correlationID,
		Data:          data,
	}

	// Checked in two steps rather than one select: a select with a default
	// case never blocks, so a ready done channel and a ready queue would be
	// chosen between at random and a publish after shutdown could report
	// success.
	select {
	case <-h.done:
		return false
	default:
	}

	select {
	case h.broadcastCh <- msg:
		return true
	default:
		h.log.Warn("broadcast queue full; dropping", "type", msgType,
			"session_id", sessionID)
		h.metrics.dropped(msgType)
		return false
	}
}
