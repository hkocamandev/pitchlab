package api

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/hkocamandev/pitchlab/internal/db"
	"github.com/hkocamandev/pitchlab/internal/db/dbgen"
	"github.com/hkocamandev/pitchlab/internal/domain"
	"github.com/hkocamandev/pitchlab/internal/events"
	"github.com/hkocamandev/pitchlab/internal/ws"
)

// SnapshotPitchCount is how much history the opening message carries.
//
// Enough to fill a timeline, small enough that a connection is not a page
// query. The client that wants more asks REST for it.
const SnapshotPitchCount = 20

// SessionSnapshot implements ws.SessionLookup.
//
// It runs during the handshake, before the upgrade, so a request for an outing
// that does not exist costs a 404 rather than a socket and two goroutines.
func (s *Server) SessionSnapshot(
	ctx context.Context, sessionID uuid.UUID,
) (ws.Snapshot, error) {
	session, err := s.store.GetSession(ctx, sessionID)
	if err != nil {
		if db.IsNoRows(err) {
			return ws.Snapshot{}, ws.ErrSessionNotFound
		}
		return ws.Snapshot{}, err
	}

	detail := SessionDetailDTO{SessionDTO: toSessionDTO(session)}
	if pitcher, err := s.store.GetAthlete(ctx, session.PitcherAthleteID); err == nil {
		detail.Pitcher = &SessionRefDTO{
			ID: pitcher.ID, FullName: pitcher.FullName, Throws: pitcher.Throws,
		}
	}

	// The denormalized counters on the session row are only recomputed when an
	// outing ends, so during live play they read zero. The live counters are
	// what the dashboard actually needs, and they come from the cache when it
	// has them and from the rows themselves when it does not.
	detail.Metrics = s.liveSessionMetrics(ctx, sessionID, detail.Metrics)

	rows, err := s.store.ListPitchesBySessionDesc(ctx,
		dbgen.ListPitchesBySessionDescParams{
			SessionID: sessionID, Limit: SnapshotPitchCount,
		})
	if err != nil {
		return ws.Snapshot{}, err
	}

	pitches := make([]dbgen.Pitch, 0, len(rows))
	meas := make([]dbgen.PitchMeasurement, 0, len(rows))
	for _, row := range rows {
		pitches = append(pitches, row.Pitch)
		meas = append(meas, row.PitchMeasurement)
	}

	items, err := s.buildPitchDTOs(ctx, pitches, meas,
		map[string]bool{"measurement": true, "prediction": true})
	if err != nil {
		return ws.Snapshot{}, err
	}

	// The highest index actually persisted, not the session's pitch count:
	// the two can disagree for a moment while a write is in flight, and the
	// client uses this number to decide whether it missed anything.
	var lastIndex int32
	if len(pitches) > 0 {
		lastIndex = pitches[0].SessionPitchIndex
	}

	return ws.Snapshot{
		Data: ws.SnapshotData{
			Session:        detail,
			Metrics:        detail.Metrics,
			LastPitchIndex: lastIndex,
			RecentPitches:  items,
		},
		Closed: session.Status != string(domain.SessionActive),
		Reason: session.Status,
	}, nil
}

// liveSessionMetrics reports an outing's running numbers.
//
// Cache first, PostgreSQL second, and the session row's stored aggregates last.
// The fallback is not a degraded mode that needs explaining to a user: it
// produces the same numbers, just by scanning rows instead of reading eight
// integers, which is exactly the trade the cache exists to make.
func (s *Server) liveSessionMetrics(
	ctx context.Context, sessionID uuid.UUID, stored SessionMetricsDTO,
) SessionMetricsDTO {
	if live, ok := s.cache.LiveSession(ctx, sessionID); ok {
		return SessionMetricsDTO{
			PitchCount:         int32(live.PitchCount),
			AvgReleaseSpeed:    float32Ptr(live.AvgReleaseSpeed),
			AvgReleaseSpinRate: float32Ptr(live.AvgReleaseSpin),
			AvgPitchScore:      float32Ptr(live.AvgPitchScore),
		}
	}

	row, err := s.store.GetSessionLiveMetrics(ctx, sessionID)
	if err != nil {
		s.log.Warn("could not rebuild live session metrics",
			"session_id", sessionID, "error", err)
		return stored
	}
	if row.PitchCount == 0 {
		return stored
	}

	return SessionMetricsDTO{
		PitchCount: int32(row.PitchCount),
		// Paired with their own counts, so a field that was null on every
		// pitch is reported as absent rather than as zero.
		AvgReleaseSpeed:    float32IfCounted(row.AvgReleaseSpeed, row.SpeedCount),
		AvgReleaseSpinRate: float32IfCounted(row.AvgReleaseSpinRate, row.SpinCount),
		AvgPitchScore:      float32IfCounted(row.AvgPitchScore, row.ScoreCount),
	}
}

func float32Ptr(v *float64) *float32 {
	if v == nil {
		return nil
	}
	f := float32(*v)
	return &f
}

func float32IfCounted(v float64, n int64) *float32 {
	if n <= 0 {
		return nil
	}
	f := float32(v)
	return &f
}

// WithWebSocket attaches the real-time channel.
//
// Optional, so the API still builds and serves REST with no hub -- which is
// what the handler tests rely on, and what a deployment that only needs the
// REST surface can do.
func (s *Server) WithWebSocket(hub *ws.Hub) *Server {
	s.wsHandler = ws.NewHandler(hub, s, s.cfg.AllowsOrigin, s.log)
	return s
}

// --- Kafka fan-out --------------------------------------------------------

// MetricsInterval throttles rollup messages.
//
// At 800x replay a pitch arrives every few milliseconds. Sending a metrics
// update with each one would force the browser to re-render hundreds of times
// a second to display numbers a human cannot read that fast.
const MetricsInterval = time.Second

// Fanout turns Kafka events into WebSocket broadcasts.
//
// It is a consumer like any other, with one difference that shapes everything:
// it owns no durable state. The record of a pitch is already in PostgreSQL by
// the time this event exists, so a missed broadcast costs a live update, not
// data. That is why it joins the group at the latest offset, has no
// dead-letter queue, and never blocks.
type Fanout struct {
	hub Broadcaster
	log *slog.Logger

	mu       sync.Mutex
	lastSent map[uuid.UUID]time.Time
}

// Broadcaster is the slice of the hub the fan-out needs.
//
// An interface so the mapping from Kafka event to WebSocket message can be
// tested for what it produces, rather than only for not returning an error.
type Broadcaster interface {
	Publish(sessionID uuid.UUID, msgType, correlationID string, data any) bool
}

// NewFanout builds the fan-out handler.
func NewFanout(hub Broadcaster, log *slog.Logger) *Fanout {
	return &Fanout{
		hub:      hub,
		log:      log.With("component", "ws-fanout"),
		lastSent: make(map[uuid.UUID]time.Time),
	}
}

// Handle dispatches one envelope.
//
// It returns nil for everything it cannot use. An error here would be retried
// and then dead-lettered, which is the wrong response for a broadcast: the
// authoritative copy is in the database and a live notification that cannot be
// delivered is simply gone.
func (f *Fanout) Handle(_ context.Context, env *events.Envelope) error {
	switch env.EventType {
	case events.TypePitchAnalyzed:
		return f.handlePitch(env)
	case events.TypeAnomaly:
		return f.handleAnomaly(env)
	default:
		return nil
	}
}

func (f *Fanout) handlePitch(env *events.Envelope) error {
	var payload events.PitchAnalyzed
	if err := env.Unmarshal(&payload); err != nil {
		f.log.Warn("undecodable pitch event", "error", err,
			"correlation_id", env.CorrelationID)
		return nil
	}

	f.hub.Publish(payload.SessionID, ws.TypePitchAnalyzed,
		env.CorrelationID.String(), toLivePitchDTO(payload))

	if f.shouldSendMetrics(payload.SessionID) {
		f.hub.Publish(payload.SessionID, ws.TypeSessionMetrics, "", LiveMetricsDTO{
			PitchCount:   payload.Sequence.SessionPitchIndex,
			LastPitchAt:  payload.ThrownAt,
			ReleaseSpeed: payload.Measurement.ReleaseSpeed,
		})
	}
	return nil
}

func (f *Fanout) handleAnomaly(env *events.Envelope) error {
	var payload events.AnomalyDetected
	if err := env.Unmarshal(&payload); err != nil {
		f.log.Warn("undecodable anomaly event", "error", err,
			"correlation_id", env.CorrelationID)
		return nil
	}

	f.hub.Publish(payload.SessionID, ws.TypeAnomalyDetected,
		env.CorrelationID.String(), payload)
	return nil
}

// shouldSendMetrics rate-limits the rollup per session.
func (f *Fanout) shouldSendMetrics(sessionID uuid.UUID) bool {
	now := time.Now()

	f.mu.Lock()
	defer f.mu.Unlock()

	if last, ok := f.lastSent[sessionID]; ok && now.Sub(last) < MetricsInterval {
		return false
	}
	f.lastSent[sessionID] = now

	// Sessions are unbounded over a long run, so the throttle map is swept of
	// entries that can no longer matter. Without this it is a slow leak that
	// only shows up after a day of replaying.
	if len(f.lastSent) > 1024 {
		for id, t := range f.lastSent {
			if now.Sub(t) > 10*MetricsInterval {
				delete(f.lastSent, id)
			}
		}
	}
	return true
}

// LiveMetricsDTO is the throttled rollup.
type LiveMetricsDTO struct {
	PitchCount   int32     `json:"pitch_count"`
	LastPitchAt  time.Time `json:"last_pitch_at"`
	ReleaseSpeed *float64  `json:"last_release_speed,omitempty"`
}

// LivePitchDTO is one analyzed pitch as the dashboard sees it.
//
// Built from the event rather than read back from PostgreSQL: the row is
// already written, and a query per pitch would put a database round trip on
// the live path for data the message already carries.
type LivePitchDTO struct {
	PitchID           uuid.UUID `json:"pitch_id"`
	SessionPitchIndex int32     `json:"session_pitch_index"`
	AtBatNumber       int       `json:"at_bat_number"`
	PitchNumber       int       `json:"pitch_number"`
	ThrownAt          time.Time `json:"thrown_at"`

	Context events.PitchContext `json:"context"`
	Stand   string              `json:"stand"`
	PThrows string              `json:"p_throws"`

	PitchType string `json:"pitch_type,omitempty"`
	PitchName string `json:"pitch_name,omitempty"`

	Measurement events.NormalizedMeasurement `json:"measurement"`
	Predictions []events.PredictionResult    `json:"predictions,omitempty"`

	// ActualOutcome is ground truth, sent so the dashboard can show the
	// prediction next to what happened. It is never a model input, and it
	// reaches the browser only after the prediction it is compared against.
	ActualOutcome    string `json:"actual_outcome,omitempty"`
	PredictionStatus string `json:"prediction_status"`
	IsSynthetic      bool   `json:"is_synthetic"`
}

func toLivePitchDTO(p events.PitchAnalyzed) LivePitchDTO {
	return LivePitchDTO{
		PitchID:           p.PitchID,
		SessionPitchIndex: p.Sequence.SessionPitchIndex,
		AtBatNumber:       p.Sequence.AtBatNumber,
		PitchNumber:       p.Sequence.PitchNumber,
		ThrownAt:          p.ThrownAt,
		Context:           p.Context,
		Stand:             p.Stand,
		PThrows:           p.PThrows,
		PitchType:         p.PitchType,
		PitchName:         p.PitchName,
		Measurement:       p.Measurement,
		Predictions:       p.Predictions,
		ActualOutcome:     p.ActualOutcome,
		PredictionStatus:  p.PredictionStatus,
		IsSynthetic:       p.IsSynthetic,
	}
}
