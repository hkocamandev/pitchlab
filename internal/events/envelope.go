// Package events defines the wire contract for everything on Kafka.
//
// One envelope wraps every message on every topic. It exists to carry the
// four things the architecture's guarantees depend on:
//
//	correlation_id   one pitch's journey across every service and topic
//	causation_id     which event directly produced this one
//	idempotency_key  what makes a redelivery recognisable
//	schema_version   what makes a format change detectable rather than silent
package events

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
)

// Topic names.
//
// The schema version is in the topic name, not only in the payload. An
// incompatible change opens a v2 topic and consumers migrate across; evolving
// one topic through incompatible shapes means every consumer must handle
// every historical shape forever.
const (
	TopicPitchRaw      = "pitchlab.pitch.raw.v1"
	TopicPitchAnalyzed = "pitchlab.pitch.analyzed.v1"
	TopicAnomaly       = "pitchlab.anomaly.detected.v1"

	TopicPitchRawDLQ      = TopicPitchRaw + ".dlq"
	TopicPitchAnalyzedDLQ = TopicPitchAnalyzed + ".dlq"
)

// Event types.
const (
	TypePitchRaw      = "pitchlab.pitch.raw"
	TypePitchAnalyzed = "pitchlab.pitch.analyzed"
	TypeAnomaly       = "pitchlab.anomaly.detected"
)

// SchemaVersion is the current envelope version.
const SchemaVersion = 1

// Envelope wraps every message.
type Envelope struct {
	EventID uuid.UUID `json:"event_id"`

	EventType     string `json:"event_type"`
	SchemaVersion int    `json:"schema_version"`

	// OccurredAt is when the thing happened in the world -- when the pitch was
	// thrown. ProducedAt is when it entered the system. Under replay these
	// differ by years, and keeping them apart is what stops a replayed 2025
	// pitch from being mistaken for something that happened this second.
	OccurredAt time.Time `json:"occurred_at"`
	ProducedAt time.Time `json:"produced_at"`

	Producer string `json:"producer"`

	// CorrelationID is copied unchanged into every downstream event, so one
	// grep follows a pitch from the simulator to the browser.
	CorrelationID uuid.UUID `json:"correlation_id"`
	// CausationID is the event that directly produced this one. Correlation
	// says which journey; causation says which step.
	CausationID *uuid.UUID `json:"causation_id,omitempty"`

	// IdempotencyKey is derived from the payload's natural key, never random.
	// A random value would make every redelivery look like a new event, which
	// is precisely backwards.
	IdempotencyKey string `json:"idempotency_key"`

	PartitionKey string `json:"partition_key"`

	// Synthetic marks data that came from replay rather than a real device.
	Synthetic bool `json:"synthetic"`

	Data json.RawMessage `json:"data"`
}

// Validation errors. These are permanent: retrying a malformed envelope
// produces the same malformed envelope.
var (
	ErrMissingEventID       = errors.New("event_id is required")
	ErrMissingEventType     = errors.New("event_type is required")
	ErrMissingCorrelationID = errors.New("correlation_id is required")
	ErrMissingIdempotency   = errors.New("idempotency_key is required")
	ErrMissingPartitionKey  = errors.New("partition_key is required")
	ErrMissingOccurredAt    = errors.New("occurred_at is required")
	ErrEmptyData            = errors.New("data is required")
)

// ErrUnsupportedSchema reports an envelope this build cannot read.
type ErrUnsupportedSchema struct {
	Got      int
	Expected int
}

func (e *ErrUnsupportedSchema) Error() string {
	return fmt.Sprintf("unsupported schema version %d (this build reads %d)",
		e.Got, e.Expected)
}

// Validate checks the envelope's invariants.
//
// An unreadable schema version fails loudly rather than being parsed on a
// best-effort basis: a partially-understood message is worse than a rejected
// one, because it persists wrong data instead of raising an alarm.
func (e *Envelope) Validate() error {
	switch {
	case e.EventID == uuid.Nil:
		return ErrMissingEventID
	case e.EventType == "":
		return ErrMissingEventType
	case e.CorrelationID == uuid.Nil:
		return ErrMissingCorrelationID
	case e.IdempotencyKey == "":
		return ErrMissingIdempotency
	case e.PartitionKey == "":
		return ErrMissingPartitionKey
	case e.OccurredAt.IsZero():
		return ErrMissingOccurredAt
	case len(e.Data) == 0:
		return ErrEmptyData
	}
	if e.SchemaVersion != SchemaVersion {
		return &ErrUnsupportedSchema{Got: e.SchemaVersion, Expected: SchemaVersion}
	}
	return nil
}

// NewEnvelope builds an envelope for a payload.
func NewEnvelope(
	eventType, producer, idempotencyKey, partitionKey string,
	occurredAt time.Time,
	payload any,
) (*Envelope, error) {
	data, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("encode payload: %w", err)
	}

	// UUIDv7 rather than v4: it sorts by creation time, so event ids stay
	// chronological in logs and in any index they land in.
	id, err := uuid.NewV7()
	if err != nil {
		return nil, fmt.Errorf("generate event id: %w", err)
	}

	return &Envelope{
		EventID:        id,
		EventType:      eventType,
		SchemaVersion:  SchemaVersion,
		OccurredAt:     occurredAt.UTC(),
		ProducedAt:     time.Now().UTC(),
		Producer:       producer,
		CorrelationID:  id, // the first event in a chain correlates to itself
		IdempotencyKey: idempotencyKey,
		PartitionKey:   partitionKey,
		Data:           data,
	}, nil
}

// Derive builds a downstream envelope that continues this one's chain.
//
// The correlation id is carried forward unchanged and the causation id points
// back here, so the chain is reconstructable in both directions: everything
// belonging to one pitch, and what directly caused what.
func (e *Envelope) Derive(
	eventType, producer, idempotencyKey, partitionKey string,
	payload any,
) (*Envelope, error) {
	out, err := NewEnvelope(eventType, producer, idempotencyKey, partitionKey,
		e.OccurredAt, payload)
	if err != nil {
		return nil, err
	}
	out.CorrelationID = e.CorrelationID
	cause := e.EventID
	out.CausationID = &cause
	out.Synthetic = e.Synthetic
	return out, nil
}

// Unmarshal decodes the payload.
func (e *Envelope) Unmarshal(target any) error {
	if err := json.Unmarshal(e.Data, target); err != nil {
		return fmt.Errorf("decode %s payload: %w", e.EventType, err)
	}
	return nil
}

// Parse decodes and validates an envelope from raw bytes.
func Parse(raw []byte) (*Envelope, error) {
	var env Envelope
	if err := json.Unmarshal(raw, &env); err != nil {
		return nil, fmt.Errorf("decode envelope: %w", err)
	}
	if err := env.Validate(); err != nil {
		return nil, err
	}
	return &env, nil
}

// Lag is how far behind real time this event is.
//
// Under replay it is years, which is the point: it makes obvious that the
// system is processing history rather than live play.
func (e *Envelope) Lag() time.Duration {
	return e.ProducedAt.Sub(e.OccurredAt)
}

// IdempotencyKeyFor derives a deterministic key from a natural key.
//
// Deterministic, never random: the same pitch must produce the same key every
// time it is published, or a redelivery would be indistinguishable from a new
// event and the consumer's duplicate detection would never fire.
func IdempotencyKeyFor(parts ...string) string {
	h := sha256.New()
	for i, p := range parts {
		if i > 0 {
			h.Write([]byte("|"))
		}
		h.Write([]byte(p))
	}
	return "sha256:" + hex.EncodeToString(h.Sum(nil))
}

// PitchUID is the natural key of a pitch: an at-bat has exactly one pitcher,
// so game, at-bat and pitch number identify it uniquely.
func PitchUID(gameRef string, atBat, pitchNumber int) string {
	return fmt.Sprintf("%s:%d:%d", gameRef, atBat, pitchNumber)
}

// SessionUID is the natural key of an outing.
func SessionUID(gameRef string, pitcherMLBAMID int) string {
	return fmt.Sprintf("%s:%d", gameRef, pitcherMLBAMID)
}
