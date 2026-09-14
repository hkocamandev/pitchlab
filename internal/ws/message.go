// Package ws implements the real-time broadcast channel.
//
// The channel is read-only by construction. Pitch data reaches the browser
// only after it has been through Kafka, the processor and PostgreSQL; nothing
// a client sends can create or alter a pitch. Accepting writes here would open
// a second write path into the system, in the same way that letting the replay
// simulator write to PostgreSQL would, and it is rejected for the same reason.
package ws

import (
	"time"

	"github.com/google/uuid"
)

// Server-to-client message types.
const (
	TypeSnapshot        = "session.snapshot"
	TypePitchAnalyzed   = "pitch.analyzed"
	TypeSessionMetrics  = "session.metrics"
	TypeAnomalyDetected = "anomaly.detected"
	TypeSessionClosed   = "session.closed"
	TypeError           = "error"
)

// Client-to-server message types.
const (
	TypePong             = "pong"
	TypeSubscribeMetrics = "subscribe.metrics"
)

// Error codes carried in an Error message's payload.
const (
	CodeSlowConsumer = "SLOW_CONSUMER"
	CodeBadRequest   = "BAD_REQUEST"
)

// Message is the envelope for every frame in both directions.
type Message struct {
	Type string `json:"type"`

	// Seq is a per-connection counter, assigned when the message is handed to
	// the connection rather than when it is written.
	//
	// That distinction is the whole design. A message dropped because the
	// client is too slow has already consumed its number, so the client sees a
	// gap and knows to reconcile over REST. This is the WebSocket equivalent of
	// a Kafka offset: not a guarantee of delivery, but a guarantee that
	// non-delivery is detectable.
	Seq uint64 `json:"seq"`

	TS time.Time `json:"ts"`

	// SessionID is repeated in every message even though the channel is
	// already session-scoped, so a message stays interpretable on its own --
	// in a log, in a test fixture, or in a browser console.
	SessionID uuid.UUID `json:"session_id"`

	// CorrelationID is the last link in the end-to-end chain: the same id that
	// followed this pitch from the simulator through Kafka and the inference
	// call arrives in the browser.
	CorrelationID string `json:"correlation_id,omitempty"`

	Data any `json:"data"`
}

// SnapshotData is the first message on a connection.
//
// It exists because a WebSocket-only dashboard shows an empty screen until the
// next pitch, which may be twenty seconds away. The client fetches REST first
// and then connects; the snapshot closes the gap between those two steps.
type SnapshotData struct {
	Session any `json:"session"`
	Metrics any `json:"metrics"`

	// LastPitchIndex lets the client detect what it missed between its REST
	// read and the moment this connection was registered. Without it, a pitch
	// that lands in those milliseconds is lost with nothing to indicate it.
	LastPitchIndex int32 `json:"last_pitch_index"`

	RecentPitches any `json:"recent_pitches"`
}

// ClosedData explains why the channel is ending.
type ClosedData struct {
	Reason       string `json:"reason"`
	FinalMetrics any    `json:"final_metrics,omitempty"`
	DroppedTotal uint64 `json:"dropped_total,omitempty"`
}

// ErrorData is an advisory sent to the client.
type ErrorData struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// clientCommand is a message from the browser.
//
// Exactly two are accepted, and neither carries data: a heartbeat reply and a
// subscription toggle. Anything else is ignored rather than answered, because
// answering an unknown command is how a read-only channel grows a write path.
type clientCommand struct {
	Type    string `json:"type"`
	Enabled *bool  `json:"enabled,omitempty"`
}
