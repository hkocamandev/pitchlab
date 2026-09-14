package api

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/hkocamandev/pitchlab/internal/events"
	"github.com/hkocamandev/pitchlab/internal/ws"
)

func fanoutLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard,
		&slog.HandlerOptions{Level: slog.LevelError}))
}

// recorder captures what the fan-out published.
type recorder struct {
	mu   sync.Mutex
	sent []published
}

type published struct {
	sessionID     uuid.UUID
	messageType   string
	correlationID string
	data          any
}

func (r *recorder) Publish(
	sessionID uuid.UUID, msgType, correlationID string, data any,
) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.sent = append(r.sent, published{sessionID, msgType, correlationID, data})
	return true
}

func (r *recorder) all() []published {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]published(nil), r.sent...)
}

func (r *recorder) ofType(t string) []published {
	var out []published
	for _, p := range r.all() {
		if p.messageType == t {
			out = append(out, p)
		}
	}
	return out
}

func envelopeFor(t *testing.T, eventType string, payload any) *events.Envelope {
	t.Helper()
	env, err := events.NewEnvelope(eventType, "test", "key", "partition",
		time.Now(), payload)
	if err != nil {
		t.Fatalf("envelope: %v", err)
	}
	return env
}

func TestFanoutPublishesAnalyzedPitches(t *testing.T) {
	rec := &recorder{}
	fanout := NewFanout(rec, fanoutLogger())

	sessionID := uuid.New()
	pitchID := uuid.New()
	speed := 92.6

	env := envelopeFor(t, events.TypePitchAnalyzed, events.PitchAnalyzed{
		PitchID:   pitchID,
		SessionID: sessionID,
		Sequence: events.Sequence{
			AtBatNumber: 41, PitchNumber: 3, SessionPitchIndex: 63,
		},
		PitchType:        "SL",
		PredictionStatus: "OK",
		ActualOutcome:    "SWINGING_STRIKE",
		Measurement: events.NormalizedMeasurement{
			RawMeasurement: events.RawMeasurement{ReleaseSpeed: &speed},
		},
	})

	if err := fanout.Handle(context.Background(), env); err != nil {
		t.Fatalf("handle: %v", err)
	}

	pitches := rec.ofType(ws.TypePitchAnalyzed)
	if len(pitches) != 1 {
		t.Fatalf("published %d pitch messages, want 1", len(pitches))
	}
	if pitches[0].sessionID != sessionID {
		t.Fatalf("published to session %s, want %s",
			pitches[0].sessionID, sessionID)
	}
	// The correlation id is the last link in the end-to-end chain: the same id
	// that followed this pitch from the simulator has to reach the browser.
	if pitches[0].correlationID != env.CorrelationID.String() {
		t.Fatalf("correlation id is %q, want %q",
			pitches[0].correlationID, env.CorrelationID)
	}

	dto, ok := pitches[0].data.(LivePitchDTO)
	if !ok {
		t.Fatalf("payload is %T, want LivePitchDTO", pitches[0].data)
	}
	if dto.PitchID != pitchID || dto.SessionPitchIndex != 63 {
		t.Fatalf("payload does not describe the pitch: %+v", dto)
	}

	// The first pitch of an outing also carries a rollup; after that the
	// rollup is throttled.
	if len(rec.ofType(ws.TypeSessionMetrics)) != 1 {
		t.Fatalf("expected exactly one rollup for the first pitch")
	}
}

func TestFanoutPublishesAnomalies(t *testing.T) {
	rec := &recorder{}
	fanout := NewFanout(rec, fanoutLogger())

	sessionID := uuid.New()
	env := envelopeFor(t, events.TypeAnomaly, events.AnomalyDetected{
		AnomalyID: uuid.New(), AthleteID: uuid.New(), SessionID: sessionID,
		Metric: "release_speed", ZRobust: -3.4,
		Direction: "DECREASE", Severity: "HIGH",
	})

	if err := fanout.Handle(context.Background(), env); err != nil {
		t.Fatalf("handle: %v", err)
	}

	anomalies := rec.ofType(ws.TypeAnomalyDetected)
	if len(anomalies) != 1 {
		t.Fatalf("published %d anomaly messages, want 1", len(anomalies))
	}
	if anomalies[0].sessionID != sessionID {
		t.Fatalf("anomaly went to the wrong session")
	}
}

func TestFanoutIgnoresUnrelatedEvents(t *testing.T) {
	rec := &recorder{}
	fanout := NewFanout(rec, fanoutLogger())

	env := envelopeFor(t, events.TypePitchRaw, events.PitchRaw{AtBatNumber: 1})

	// Returning an error would have the consumer retry and then dead-letter a
	// message it simply has no use for.
	if err := fanout.Handle(context.Background(), env); err != nil {
		t.Fatalf("an unrelated event was treated as a failure: %v", err)
	}
	if len(rec.all()) != 0 {
		t.Fatalf("a raw pitch reached the dashboard: %+v", rec.all())
	}
}

func TestFanoutSwallowsUndecodablePayloads(t *testing.T) {
	rec := &recorder{}
	fanout := NewFanout(rec, fanoutLogger())

	env := envelopeFor(t, events.TypePitchAnalyzed, events.PitchAnalyzed{})
	env.Data = json.RawMessage(`{"session_id": 12345}`) // a number, not a UUID

	// This consumer owns no durable state: the pitch is already in PostgreSQL,
	// so a broadcast that cannot be built costs a live update and nothing more.
	// Failing here would stall the partition over a cosmetic problem.
	if err := fanout.Handle(context.Background(), env); err != nil {
		t.Fatalf("an undecodable payload was treated as a failure: %v", err)
	}
	if len(rec.all()) != 0 {
		t.Fatal("a message was published from an undecodable payload")
	}
}

func TestMetricsAreThrottledPerSession(t *testing.T) {
	fanout := NewFanout(&recorder{}, fanoutLogger())
	first, second := uuid.New(), uuid.New()

	// At 800x replay a pitch arrives every few milliseconds. Without a
	// throttle the browser would be asked to re-render hundreds of times a
	// second to show numbers nobody can read that fast.
	if !fanout.shouldSendMetrics(first) {
		t.Fatal("the first rollup for a session was throttled")
	}
	if fanout.shouldSendMetrics(first) {
		t.Fatal("a second rollup within the interval was not throttled")
	}

	// The throttle is per session: a busy outing must not silence a quiet one.
	if !fanout.shouldSendMetrics(second) {
		t.Fatal("a different session was throttled by the first one's rollup")
	}
}

func TestMetricsThrottleReleasesAfterTheInterval(t *testing.T) {
	fanout := NewFanout(&recorder{}, fanoutLogger())
	sessionID := uuid.New()

	fanout.shouldSendMetrics(sessionID)

	fanout.mu.Lock()
	fanout.lastSent[sessionID] = time.Now().Add(-2 * MetricsInterval)
	fanout.mu.Unlock()

	if !fanout.shouldSendMetrics(sessionID) {
		t.Fatal("the throttle did not release after the interval")
	}
}

func TestBurstOfPitchesProducesOneRollup(t *testing.T) {
	rec := &recorder{}
	fanout := NewFanout(rec, fanoutLogger())
	sessionID := uuid.New()

	for i := 0; i < 200; i++ {
		env := envelopeFor(t, events.TypePitchAnalyzed, events.PitchAnalyzed{
			PitchID:   uuid.New(),
			SessionID: sessionID,
			Sequence:  events.Sequence{SessionPitchIndex: int32(i)},
		})
		if err := fanout.Handle(context.Background(), env); err != nil {
			t.Fatalf("handle: %v", err)
		}
	}

	// Every pitch is broadcast; the rollup is not.
	if got := len(rec.ofType(ws.TypePitchAnalyzed)); got != 200 {
		t.Fatalf("broadcast %d pitches, want 200", got)
	}
	if got := len(rec.ofType(ws.TypeSessionMetrics)); got != 1 {
		t.Fatalf("published %d rollups for a burst, want 1", got)
	}
}

func TestLivePitchCarriesNoPostContactMeasurements(t *testing.T) {
	dto := toLivePitchDTO(events.PitchAnalyzed{
		PitchID:       uuid.New(),
		ActualOutcome: "IN_PLAY",
	})

	raw, err := json.Marshal(dto)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	body := string(raw)

	// The leakage discipline reaches the browser too. actual_outcome is ground
	// truth and is sent so the dashboard can put the prediction next to what
	// happened; exit velocity and run value are not measurable by the device
	// and must not appear anywhere on this path.
	forbidden := []string{
		"launch_speed", "launch_angle", "hit_distance_sc",
		"estimated_woba_using_speedangle", "estimated_ba_using_speedangle",
		"delta_run_exp", "woba_value", "babip_value", "iso_value",
	}
	for _, field := range forbidden {
		if strings.Contains(body, field) {
			t.Fatalf("live pitch payload carries %q", field)
		}
	}
	if !strings.Contains(body, "actual_outcome") {
		t.Fatal("actual_outcome is missing; the dashboard cannot show reality")
	}
}
