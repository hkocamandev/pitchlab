package processor

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/hkocamandev/pitchlab/internal/anomaly"
	"github.com/hkocamandev/pitchlab/internal/cache"
	"github.com/hkocamandev/pitchlab/internal/db"
	"github.com/hkocamandev/pitchlab/internal/db/dbgen"
	"github.com/hkocamandev/pitchlab/internal/events"
	pkafka "github.com/hkocamandev/pitchlab/internal/kafka"
)

// AnomalyEvaluator watches sessions and flags departures from a pitcher's own
// baseline.
//
// It runs as a second consumer group on pitch.analyzed rather than inside the
// persistence path. Two reasons: an anomaly is a session-level finding, so it
// has nothing to say until an outing is over, and evaluation must not extend
// the transaction that stores a pitch.
//
// Evaluation is triggered by the session going quiet, not by each pitch. A
// single pitch is not an anomaly; a session's median is.
type AnomalyEvaluator struct {
	store    *db.Store
	detector *anomaly.Detector
	producer *pkafka.Producer
	log      *slog.Logger
	version  string

	// cache may be nil; every method on it tolerates that.
	cache *cache.Cache

	// IdleTimeout is how long a session must be silent before it is
	// considered over. Replay at 30x compresses twenty seconds between
	// pitches into under a second, so this is deliberately short; a real
	// deployment would raise it.
	idleTimeout  time.Duration
	minPitches   int
	pollInterval time.Duration

	mu     sync.Mutex
	active map[uuid.UUID]*sessionWatch

	onFinding func(metric, severity string)
}

type sessionWatch struct {
	athleteID  uuid.UUID
	lastSeen   time.Time
	pitchCount int
	evaluated  bool
}

// AnomalyConfig tunes the evaluator.
type AnomalyConfig struct {
	IdleTimeout  time.Duration
	MinPitches   int
	PollInterval time.Duration
	Detector     anomaly.Config
}

// DefaultAnomalyConfig returns sensible defaults.
func DefaultAnomalyConfig() AnomalyConfig {
	return AnomalyConfig{
		IdleTimeout: 20 * time.Second,
		// Below fifteen pitches an outing's median is too noisy to compare
		// against anything.
		MinPitches:   15,
		PollInterval: 5 * time.Second,
		Detector:     anomaly.DefaultConfig(),
	}
}

// WithCache attaches the Redis layer.
func (a *AnomalyEvaluator) WithCache(c *cache.Cache) *AnomalyEvaluator {
	a.cache = c
	return a
}

// NewAnomalyEvaluator builds an evaluator.
func NewAnomalyEvaluator(
	store *db.Store, producer *pkafka.Producer, cfg AnomalyConfig,
	log *slog.Logger, version string,
) *AnomalyEvaluator {
	if cfg.IdleTimeout <= 0 {
		cfg.IdleTimeout = 20 * time.Second
	}
	if cfg.MinPitches <= 0 {
		cfg.MinPitches = 15
	}
	if cfg.PollInterval <= 0 {
		cfg.PollInterval = 5 * time.Second
	}
	return &AnomalyEvaluator{
		store:        store,
		detector:     anomaly.New(cfg.Detector),
		producer:     producer,
		log:          log,
		version:      version,
		idleTimeout:  cfg.IdleTimeout,
		minPitches:   cfg.MinPitches,
		pollInterval: cfg.PollInterval,
		active:       map[uuid.UUID]*sessionWatch{},
	}
}

// OnFinding registers a callback for metrics.
func (a *AnomalyEvaluator) OnFinding(fn func(metric, severity string)) *AnomalyEvaluator {
	a.onFinding = fn
	return a
}

// Handle records that a session is still producing pitches.
//
// Deliberately cheap: this runs on every pitch, and the expensive work
// happens once per session in the sweeper.
func (a *AnomalyEvaluator) Handle(ctx context.Context, env *events.Envelope) error {
	if env.EventType != events.TypePitchAnalyzed {
		return pkafka.Permanent(
			fmt.Sprintf("unexpected event type %q", env.EventType), nil)
	}

	var analyzed events.PitchAnalyzed
	if err := env.Unmarshal(&analyzed); err != nil {
		return pkafka.Permanent("decode analyzed pitch", err)
	}

	a.mu.Lock()
	watch, ok := a.active[analyzed.SessionID]
	if !ok {
		watch = &sessionWatch{athleteID: analyzed.PitcherAthleteID}
		a.active[analyzed.SessionID] = watch
	}
	watch.lastSeen = time.Now()
	watch.pitchCount++
	// A session that resumes after being evaluated gets evaluated again, so a
	// pause mid-outing does not permanently close the book on it.
	watch.evaluated = false
	a.mu.Unlock()

	return nil
}

// Run sweeps for quiet sessions until the context is cancelled.
func (a *AnomalyEvaluator) Run(ctx context.Context) error {
	ticker := time.NewTicker(a.pollInterval)
	defer ticker.Stop()

	a.log.Info("anomaly evaluator started",
		"idle_timeout", a.idleTimeout, "min_pitches", a.minPitches)

	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			a.sweep(ctx)
		}
	}
}

func (a *AnomalyEvaluator) sweep(ctx context.Context) {
	now := time.Now()

	a.mu.Lock()
	var due []uuid.UUID
	for id, w := range a.active {
		if !w.evaluated && now.Sub(w.lastSeen) >= a.idleTimeout {
			due = append(due, id)
			w.evaluated = true
		}
	}
	a.mu.Unlock()

	for _, sessionID := range due {
		if err := a.EvaluateSession(ctx, sessionID); err != nil {
			a.log.Error("evaluate session", "session_id", sessionID, "error", err)
		}
	}
}

// EvaluateSession compares one finished outing against the pitcher's baseline.
func (a *AnomalyEvaluator) EvaluateSession(ctx context.Context, sessionID uuid.UUID) error {
	session, err := a.store.GetSession(ctx, sessionID)
	if err != nil {
		if db.IsNoRows(err) {
			return nil
		}
		return fmt.Errorf("load session: %w", err)
	}

	if _, err := a.store.RecomputeSessionAggregates(ctx, sessionID); err != nil {
		a.log.Warn("recompute session aggregates", "session_id", sessionID, "error", err)
	} else {
		// The outing is over and PostgreSQL now holds the authoritative
		// aggregates, so the running counters have nothing left to accelerate.
		// Dropped only after the recompute succeeds: otherwise a failure here
		// would leave both copies gone.
		a.cache.DropLiveSession(ctx, sessionID)
	}

	pitchTypes, err := a.sessionPitchTypes(ctx, sessionID)
	if err != nil {
		return err
	}

	var findings int
	for _, pitchType := range pitchTypes {
		n, err := a.evaluatePitchType(ctx, session, pitchType)
		if err != nil {
			return err
		}
		findings += n
	}

	if findings > 0 {
		a.log.Info("anomalies detected",
			"session_id", sessionID, "athlete_id", session.PitcherAthleteID,
			"findings", findings)
	}
	return nil
}

func (a *AnomalyEvaluator) evaluatePitchType(
	ctx context.Context, session dbgen.Session, pitchType string,
) (int, error) {
	observed, err := a.store.GetSessionMetricMedians(ctx,
		dbgen.GetSessionMetricMediansParams{SessionID: session.ID, PitchType: &pitchType})
	if err != nil {
		if db.IsNoRows(err) {
			return 0, nil
		}
		return 0, fmt.Errorf("session medians: %w", err)
	}
	if observed.PitchCount < int64(a.minPitches) {
		return 0, nil
	}

	window, err := a.store.GetAnomalyBaselineWindow(ctx,
		dbgen.GetAnomalyBaselineWindowParams{
			PitcherAthleteID: session.PitcherAthleteID,
			PitchType:        &pitchType,
			// Strictly before this outing: including it in its own baseline
			// would pull the median toward the very value being tested.
			Before:     session.StartedAt,
			MinPitches: int32(a.minPitches),
			WindowSize: 10,
		})
	if err != nil {
		return 0, fmt.Errorf("baseline window: %w", err)
	}
	if len(window) == 0 {
		return 0, nil
	}

	var stored int
	for _, metric := range anomaly.AllMetrics {
		value, ok := metricValue(observed, metric)
		if !ok {
			continue
		}

		points := make([]anomaly.SessionPoint, 0, len(window))
		for _, row := range window {
			v, ok := windowValue(row, metric)
			if !ok {
				continue
			}
			points = append(points, anomaly.SessionPoint{
				SessionID:  row.SessionID,
				StartedAt:  row.StartedAt,
				PitchCount: int(row.PitchCount),
				Value:      v,
			})
		}

		finding := a.detector.Evaluate(anomaly.Observation{
			AthleteID:  session.PitcherAthleteID,
			SessionID:  session.ID,
			PitchType:  pitchType,
			Metric:     metric,
			Value:      value,
			PitchCount: int(observed.PitchCount),
			DetectedAt: time.Now().UTC(),
		}, points)
		if finding == nil {
			continue
		}

		if err := a.store_finding(ctx, *finding, pitchType); err != nil {
			return stored, err
		}
		stored++
	}
	return stored, nil
}

func (a *AnomalyEvaluator) store_finding(
	ctx context.Context, f anomaly.Finding, pitchType string,
) error {
	row, err := a.store.InsertAnomaly(ctx, dbgen.InsertAnomalyParams{
		ID:              db.NewID(),
		AthleteID:       f.AthleteID,
		SessionID:       f.SessionID,
		Metric:          string(f.Metric),
		PitchType:       &pitchType,
		ObservedValue:   float32(f.ObservedValue),
		BaselineMedian:  float32(f.BaselineMedian),
		RobustSigma:     float32(f.RobustSigma),
		ZRobust:         float32(f.ZRobust),
		Direction:       string(f.Direction),
		Severity:        string(f.Severity),
		WindowSessions:  int16(f.WindowSessions),
		DetectorVersion: f.DetectorVersion,
	})
	if db.IsNoRows(err) {
		// Already recorded. The unique index absorbs a re-consumed event,
		// which at-least-once delivery guarantees will happen.
		return nil
	}
	if err != nil {
		return fmt.Errorf("insert anomaly: %w", err)
	}

	payload := events.AnomalyDetected{
		AnomalyID: row.ID, AthleteID: row.AthleteID, SessionID: row.SessionID,
		Metric: row.Metric, PitchType: pitchType,
		ObservedValue:   f.ObservedValue,
		BaselineMedian:  f.BaselineMedian,
		RobustSigma:     f.RobustSigma,
		ZRobust:         f.ZRobust,
		Direction:       string(f.Direction),
		Severity:        string(f.Severity),
		WindowSessions:  f.WindowSessions,
		DetectorVersion: f.DetectorVersion,
		DetectedAt:      row.DetectedAt,
	}

	env, err := events.NewEnvelope(
		events.TypeAnomaly, "pitchlab-processor/"+a.version,
		events.IdempotencyKeyFor(f.SessionID.String(), string(f.Metric),
			pitchType, f.DetectorVersion),
		// Keyed by athlete, not session: a pitcher's alerts are meaningful in
		// sequence, and sessions are independent of each other.
		f.AthleteID.String(),
		row.DetectedAt, payload)
	if err != nil {
		return fmt.Errorf("build anomaly event: %w", err)
	}

	if err := a.producer.Publish(ctx, events.TopicAnomaly, env); err != nil {
		// The finding is stored; only the notification failed. The dashboard
		// still shows it on the next read.
		a.log.Warn("publish anomaly event", "anomaly_id", row.ID, "error", err)
	}

	if a.onFinding != nil {
		a.onFinding(string(f.Metric), string(f.Severity))
	}

	a.log.Info("anomaly",
		"athlete_id", f.AthleteID, "metric", f.Metric, "pitch_type", pitchType,
		"observed", fmt.Sprintf("%.2f", f.ObservedValue),
		"baseline", fmt.Sprintf("%.2f", f.BaselineMedian),
		"z_robust", fmt.Sprintf("%.2f", f.ZRobust),
		"severity", f.Severity)
	return nil
}

func (a *AnomalyEvaluator) sessionPitchTypes(
	ctx context.Context, sessionID uuid.UUID,
) ([]string, error) {
	rows, err := a.store.Pool().Query(ctx, `
		SELECT pitch_type, count(*) AS n
		FROM pitches
		WHERE session_id = $1 AND pitch_type IS NOT NULL
		GROUP BY pitch_type
		HAVING count(*) >= $2
		ORDER BY n DESC`, sessionID, a.minPitches)
	if err != nil {
		return nil, fmt.Errorf("session pitch types: %w", err)
	}
	defer rows.Close()

	var out []string
	for rows.Next() {
		var pitchType string
		var n int64
		if err := rows.Scan(&pitchType, &n); err != nil {
			return nil, err
		}
		out = append(out, pitchType)
	}
	return out, rows.Err()
}

// metricValue reads one metric from the session under evaluation.
//
// The count that travels with each median is what makes the zero unambiguous:
// COALESCE keeps the scan safe, but a zero median and "no readings at all"
// would otherwise be the same value.
func metricValue(r dbgen.GetSessionMetricMediansRow, m anomaly.Metric) (float64, bool) {
	switch m {
	case anomaly.MetricReleaseSpeed:
		return r.MedianReleaseSpeed, r.NReleaseSpeed > 0
	case anomaly.MetricReleaseSpinRate:
		return r.MedianReleaseSpinRate, r.NReleaseSpinRate > 0
	case anomaly.MetricReleasePosXArm:
		return r.MedianReleasePosXArm, r.NReleasePosXArm > 0
	case anomaly.MetricReleasePosZ:
		return r.MedianReleasePosZ, r.NReleasePosZ > 0
	case anomaly.MetricReleaseExtension:
		return r.MedianReleaseExtension, r.NReleaseExtension > 0
	case anomaly.MetricPfxXArm:
		return r.MedianPfxXArm, r.NPfxXArm > 0
	case anomaly.MetricPfxZ:
		return r.MedianPfxZ, r.NPfxZ > 0
	}
	return 0, false
}

// windowValue reads one metric from a baseline outing.
func windowValue(r dbgen.GetAnomalyBaselineWindowRow, m anomaly.Metric) (float64, bool) {
	switch m {
	case anomaly.MetricReleaseSpeed:
		return r.MedianReleaseSpeed, r.NReleaseSpeed > 0
	case anomaly.MetricReleaseSpinRate:
		return r.MedianReleaseSpinRate, r.NReleaseSpinRate > 0
	case anomaly.MetricReleasePosXArm:
		return r.MedianReleasePosXArm, r.NReleasePosXArm > 0
	case anomaly.MetricReleasePosZ:
		return r.MedianReleasePosZ, r.NReleasePosZ > 0
	case anomaly.MetricReleaseExtension:
		return r.MedianReleaseExtension, r.NReleaseExtension > 0
	case anomaly.MetricPfxXArm:
		return r.MedianPfxXArm, r.NPfxXArm > 0
	case anomaly.MetricPfxZ:
		return r.MedianPfxZ, r.NPfxZ > 0
	}
	return 0, false
}
