package processor

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"time"

	"github.com/google/uuid"

	"github.com/hkocamandev/pitchlab/internal/db"
	"github.com/hkocamandev/pitchlab/internal/db/dbgen"
	"github.com/hkocamandev/pitchlab/internal/events"
	pkafka "github.com/hkocamandev/pitchlab/internal/kafka"
	"github.com/hkocamandev/pitchlab/internal/mlclient"
)

// Namespace for deterministic identifiers.
//
// Deriving ids from natural keys rather than generating them means the same
// pitch always gets the same id, no matter which process handles it or how
// many times it is replayed. That is what lets a redelivery be recognised
// instead of becoming a second row.
var idNamespace = uuid.MustParse("6f1d5b84-6a2a-4f1e-9f3a-0b3f0f0e2d11")

// AthleteID derives an athlete's id from their MLBAM id.
func AthleteID(mlbamID int) uuid.UUID {
	return uuid.NewSHA1(idNamespace, []byte(fmt.Sprintf("athlete:%d", mlbamID)))
}

// SessionID derives an outing's id from its natural key.
func SessionID(sessionUID string) uuid.UUID {
	return uuid.NewSHA1(idNamespace, []byte("session:"+sessionUID))
}

// PitchID derives a pitch's id from its natural key.
func PitchID(pitchUID string) uuid.UUID {
	return uuid.NewSHA1(idNamespace, []byte("pitch:"+pitchUID))
}

// Pipeline turns raw device readings into scored, persisted pitches.
type Pipeline struct {
	store    *db.Store
	ml       *mlclient.Client
	producer *pkafka.Producer
	log      *slog.Logger
	version  string

	metrics PipelineMetrics
}

// PipelineMetrics receives processing outcomes. Nil callbacks are ignored.
type PipelineMetrics struct {
	OnDuplicate  func()
	OnPersisted  func(d time.Duration)
	OnPrediction func(variant string, d time.Duration, err error)
}

// NewPipeline builds a pipeline.
func NewPipeline(
	store *db.Store, ml *mlclient.Client, producer *pkafka.Producer,
	log *slog.Logger, version string,
) *Pipeline {
	return &Pipeline{store: store, ml: ml, producer: producer, log: log, version: version}
}

// WithMetrics attaches metric callbacks.
func (p *Pipeline) WithMetrics(m PipelineMetrics) *Pipeline {
	p.metrics = m
	return p
}

// HandlePitchRaw processes one device reading.
//
// The order of the steps is the design:
//
//  1. validate          malformed input fails permanently, not repeatedly
//  2. normalize         domain rules, in the one place they are tested
//  3. build features    from a struct that has no field for the outcome
//  4. predict           synchronous, because the pitch is not complete without it
//  5. persist           one transaction, idempotent on the natural key
//  6. publish           after the write, so a consumer can read back what it hears about
//
// Inference failure does not stop the pitch. The row is written with its
// prediction marked failed and can be backfilled from data already in the
// database; losing the pitch because a dependency was down would be the worse
// outcome by far.
func (p *Pipeline) HandlePitchRaw(ctx context.Context, env *events.Envelope) error {
	if env.EventType != events.TypePitchRaw {
		return pkafka.Permanent(
			fmt.Sprintf("unexpected event type %q on the raw pitch topic", env.EventType), nil)
	}

	var raw events.PitchRaw
	if err := env.Unmarshal(&raw); err != nil {
		return pkafka.Permanent("decode pitch payload", err)
	}
	if err := validateRaw(raw); err != nil {
		return pkafka.Permanent("invalid pitch payload", err)
	}

	log := p.log.With(
		"correlation_id", env.CorrelationID.String(),
		"pitch_uid", raw.PitchUID(),
	)

	normalized := Normalize(raw.Measurement, raw.Session.PThrows, raw.Batter.Stand)

	pitcherID := AthleteID(raw.Session.PitcherMLBAMID)
	sessionID := SessionID(raw.Session.SessionUID)
	pitchID := PitchID(raw.PitchUID())

	var batterID *uuid.UUID
	if raw.Batter.MLBAMID != 0 {
		id := AthleteID(raw.Batter.MLBAMID)
		batterID = &id
	}

	predictions := p.predict(ctx, env, normalized, raw)

	status := "OK"
	if len(predictions) == 0 {
		status = "FAILED"
	}

	persistStart := time.Now()
	analyzed, err := p.persist(ctx, persistInput{
		env:         env,
		raw:         raw,
		normalized:  normalized,
		pitcherID:   pitcherID,
		batterID:    batterID,
		sessionID:   sessionID,
		pitchID:     pitchID,
		predictions: predictions,
		status:      status,
	})
	if err != nil {
		return err
	}
	if p.metrics.OnPersisted != nil {
		p.metrics.OnPersisted(time.Since(persistStart))
	}

	if analyzed == nil {
		// The pitch was already stored. Republishing would make the dashboard
		// show it twice, so the redelivery stops here.
		if p.metrics.OnDuplicate != nil {
			p.metrics.OnDuplicate()
		}
		return pkafka.ErrDuplicate
	}

	out, err := env.Derive(events.TypePitchAnalyzed, p.producerName(),
		env.IdempotencyKey, raw.Session.SessionUID, analyzed)
	if err != nil {
		return fmt.Errorf("build analyzed event: %w", err)
	}

	if err := p.producer.Publish(ctx, events.TopicPitchAnalyzed, out); err != nil {
		// The pitch is safely stored; only the notification failed. Returning
		// an error replays the message, and the duplicate path above makes
		// that harmless.
		return fmt.Errorf("publish analyzed event: %w", err)
	}

	log.Debug("pitch processed",
		"pitch_id", pitchID, "index", analyzed.Sequence.SessionPitchIndex,
		"status", status, "predictions", len(predictions))
	return nil
}

func validateRaw(raw events.PitchRaw) error {
	switch {
	case raw.ExternalGameRef == "":
		return fmt.Errorf("external_game_ref is required")
	case raw.AtBatNumber < 1:
		return fmt.Errorf("at_bat_number must be positive, got %d", raw.AtBatNumber)
	case raw.PitchNumber < 1:
		return fmt.Errorf("pitch_number must be positive, got %d", raw.PitchNumber)
	case raw.Session.SessionUID == "":
		return fmt.Errorf("session_uid is required")
	case raw.Session.PitcherMLBAMID == 0:
		return fmt.Errorf("pitcher_mlbam_id is required")
	case raw.ThrownAt.IsZero():
		return fmt.Errorf("thrown_at is required")
	}

	// The rulebook, checked before the database does. A device reporting five
	// balls is reporting a fault, and catching it here gives a clear message
	// instead of a constraint violation to reverse-engineer.
	c := raw.Context
	switch {
	case c.Balls < 0 || c.Balls > 3:
		return fmt.Errorf("balls must be 0-3, got %d", c.Balls)
	case c.Strikes < 0 || c.Strikes > 2:
		return fmt.Errorf("strikes must be 0-2, got %d", c.Strikes)
	case c.OutsWhenUp < 0 || c.OutsWhenUp > 2:
		return fmt.Errorf("outs must be 0-2, got %d", c.OutsWhenUp)
	case c.Inning < 1:
		return fmt.Errorf("inning must be positive, got %d", c.Inning)
	}

	if raw.Session.PThrows != "L" && raw.Session.PThrows != "R" {
		return fmt.Errorf("p_throws must be L or R, got %q", raw.Session.PThrows)
	}
	if raw.Batter.Stand != "" && raw.Batter.Stand != "L" && raw.Batter.Stand != "R" {
		return fmt.Errorf("stand must be L or R, got %q", raw.Batter.Stand)
	}
	return nil
}

func (p *Pipeline) predict(
	ctx context.Context, env *events.Envelope,
	normalized events.NormalizedMeasurement, raw events.PitchRaw,
) []events.PredictionResult {
	ctx = mlclient.WithCorrelationID(ctx, env.CorrelationID.String())

	input := FeatureInput{
		Measurement: normalized,
		Context:     raw.Context,
		Stand:       raw.Batter.Stand,
		PThrows:     raw.Session.PThrows,
		PitchType:   raw.Measurement.PitchType,
	}

	var out []events.PredictionResult
	for _, variant := range []mlclient.Variant{
		mlclient.VariantPitching, mlclient.VariantStuff,
	} {
		features := BuildFeatures(input, variant)
		addSequenceFeatures(features, raw.PitchNumber, nil)

		start := time.Now()
		resp, err := p.ml.Predict(ctx, variant, []mlclient.PitchFeatures{{
			PitchID:    PitchID(raw.PitchUID()).String(),
			CountState: CountState(raw.Context.Balls, raw.Context.Strikes),
			Features:   features,
		}})
		if p.metrics.OnPrediction != nil {
			p.metrics.OnPrediction(string(variant), time.Since(start), err)
		}

		if err != nil {
			// Logged at warn, not error: with the breaker open this is the
			// expected steady state during an outage, and an error per pitch
			// would drown the log that explains why.
			p.log.Warn("prediction unavailable",
				"variant", variant, "correlation_id", env.CorrelationID.String(),
				"error", err)
			continue
		}
		if len(resp.Predictions) == 0 {
			continue
		}

		pred := resp.Predictions[0]
		result := events.PredictionResult{
			Variant:        string(variant),
			ModelVersion:   resp.ModelVersion,
			Target:         resp.Target,
			Probabilities:  pred.Probabilities,
			ScoredQuantity: pred.ScoredQuantity,
			Score:          pred.Score,
		}
		if pred.PredictedClass != nil {
			result.PredictedClass = *pred.PredictedClass
		}
		result.WhiffProbability = pred.WhiffProbability
		out = append(out, result)
	}
	return out
}

type persistInput struct {
	env         *events.Envelope
	raw         events.PitchRaw
	normalized  events.NormalizedMeasurement
	pitcherID   uuid.UUID
	batterID    *uuid.UUID
	sessionID   uuid.UUID
	pitchID     uuid.UUID
	predictions []events.PredictionResult
	status      string
}

// persist writes everything for one pitch in a single transaction.
//
// Returns nil when the pitch already existed, which is the duplicate signal.
//
// One transaction rather than several writes: a pitch with no measurement, or
// a measurement with no pitch, is a state no reader knows how to interpret.
func (p *Pipeline) persist(ctx context.Context, in persistInput) (*events.PitchAnalyzed, error) {
	var analyzed *events.PitchAnalyzed

	err := p.store.InTx(ctx, func(q *dbgen.Queries) error {
		if _, err := q.UpsertAthlete(ctx, dbgen.UpsertAthleteParams{
			ID:          in.pitcherID,
			MlbamID:     int32(in.raw.Session.PitcherMLBAMID),
			FullName:    displayName(in.raw.Session.PitcherName, in.raw.Session.PitcherMLBAMID),
			Throws:      strPtr(in.raw.Session.PThrows),
			PrimaryRole: "PITCHER",
		}); err != nil {
			return fmt.Errorf("upsert pitcher: %w", err)
		}

		if in.batterID != nil {
			if _, err := q.UpsertAthlete(ctx, dbgen.UpsertAthleteParams{
				ID:          *in.batterID,
				MlbamID:     int32(in.raw.Batter.MLBAMID),
				FullName:    displayName(in.raw.Batter.Name, in.raw.Batter.MLBAMID),
				Bats:        strPtr(in.raw.Batter.Stand),
				PrimaryRole: "BATTER",
			}); err != nil {
				return fmt.Errorf("upsert batter: %w", err)
			}
		}

		deviceID, err := p.resolveDevice(ctx, q, in.raw)
		if err != nil {
			return err
		}

		session, err := q.UpsertSession(ctx, dbgen.UpsertSessionParams{
			ID:                 in.sessionID,
			ExternalSessionUid: in.raw.Session.SessionUID,
			PitcherAthleteID:   in.pitcherID,
			DeviceID:           deviceID,
			ExternalGameRef:    strPtr(in.raw.ExternalGameRef),
			SourceGameDate:     parseDate(in.raw.SourceGameDate),
			OpponentTeam:       strPtr(in.raw.Session.OpponentTeam),
			StartedAt:          in.raw.ThrownAt,
			Status:             "ACTIVE",
			IsSynthetic:        in.env.Synthetic,
		})
		if err != nil {
			return fmt.Errorf("upsert session: %w", err)
		}

		// Safe because a session maps to exactly one partition and a
		// partition is consumed by a single goroutine, so writes for one
		// outing are serialized by construction rather than by locking.
		index, err := q.NextSessionPitchIndex(ctx, session.ID)
		if err != nil {
			return fmt.Errorf("next pitch index: %w", err)
		}
		if index < 1 {
			index = 1
		}

		pitch, err := q.InsertPitch(ctx, dbgen.InsertPitchParams{
			ID:                in.pitchID,
			ExternalPitchUid:  in.raw.PitchUID(),
			SessionID:         session.ID,
			PitcherAthleteID:  in.pitcherID,
			BatterAthleteID:   in.batterID,
			AtBatNumber:       int16(in.raw.AtBatNumber),
			PitchNumber:       int16(in.raw.PitchNumber),
			SessionPitchIndex: index,
			ThrownAt:          in.raw.ThrownAt,
			Balls:             int16(in.raw.Context.Balls),
			Strikes:           int16(in.raw.Context.Strikes),
			OutsWhenUp:        int16(in.raw.Context.OutsWhenUp),
			Inning:            int16(in.raw.Context.Inning),
			InningTopbot:      strPtr(in.raw.Context.InningTopBot),
			Stand:             defaultStand(in.raw.Batter.Stand),
			PThrows:           in.raw.Session.PThrows,
			On1b:              in.raw.Context.On1B,
			On2b:              in.raw.Context.On2B,
			On3b:              in.raw.Context.On3B,
			BatScore:          int16Ptr(in.raw.Context.BatScore),
			FldScore:          int16Ptr(in.raw.Context.FldScore),
			NThruorderPitcher: int16Ptr(in.raw.Context.NThruOrderPitcher),
			PitchType:         strPtr(in.raw.Measurement.PitchType),
			PitchName:         strPtr(in.raw.Measurement.PitchName),
			ActualOutcome:     groundTruth(in.raw.GroundTruth),
			PredictionStatus:  in.status,
			CorrelationID:     &in.env.CorrelationID,
			SourceEventID:     &in.env.EventID,
		})
		if db.IsNoRows(err) {
			// The unique constraint refused a redelivery. Not an error.
			return nil
		}
		if err != nil {
			return fmt.Errorf("insert pitch: %w", err)
		}

		if _, err := q.UpsertPitchMeasurement(ctx,
			measurementParams(pitch.ID, deviceID, in.normalized)); err != nil {
			return fmt.Errorf("upsert measurement: %w", err)
		}

		for _, pred := range in.predictions {
			if err := p.persistPrediction(ctx, q, pitch.ID, pred); err != nil {
				return err
			}
		}

		analyzed = buildAnalyzed(in, pitch, session)
		return nil
	})

	return analyzed, err
}

func (p *Pipeline) persistPrediction(
	ctx context.Context, q *dbgen.Queries, pitchID uuid.UUID, pred events.PredictionResult,
) error {
	model, err := q.GetActiveModelVersion(ctx, pred.Variant)
	if err != nil {
		if db.IsNoRows(err) {
			// No registered model for this variant. The prediction cannot be
			// stored interpretably without one, so it is skipped rather than
			// written with a dangling reference.
			p.log.Warn("no active model version registered; skipping prediction",
				"variant", pred.Variant)
			return nil
		}
		return fmt.Errorf("load model version: %w", err)
	}

	params := dbgen.UpsertPitchPredictionParams{
		ID:             db.NewID(),
		PitchID:        pitchID,
		ModelVersionID: model.ID,
		ScoredQuantity: float32(pred.ScoredQuantity),
		Score:          float32(pred.Score),
	}

	// The schema enforces exactly one output shape: a class distribution or a
	// whiff probability, never both.
	if len(pred.Probabilities) > 0 {
		probs, err := json.Marshal(pred.Probabilities)
		if err != nil {
			return fmt.Errorf("encode probabilities: %w", err)
		}
		params.Probabilities = probs
		params.PredictedClass = strPtr(pred.PredictedClass)
	} else if pred.WhiffProbability != nil {
		w := float32(*pred.WhiffProbability)
		params.WhiffProbability = &w
	} else {
		return nil
	}

	if _, err := q.UpsertPitchPrediction(ctx, params); err != nil {
		return fmt.Errorf("upsert prediction (%s): %w", pred.Variant, err)
	}
	return nil
}

func (p *Pipeline) resolveDevice(
	ctx context.Context, q *dbgen.Queries, raw events.PitchRaw,
) (*uuid.UUID, error) {
	if raw.DeviceID == "" {
		return nil, nil
	}
	device, err := q.UpsertDevice(ctx, dbgen.UpsertDeviceParams{
		ID:                 uuid.NewSHA1(idNamespace, []byte("device:"+raw.DeviceID)),
		DeviceKey:          raw.DeviceID,
		DeviceKind:         defaultDeviceKind(raw.DeviceKind),
		CalibrationProfile: []byte(`{}`),
	})
	if err != nil {
		return nil, fmt.Errorf("upsert device: %w", err)
	}
	return &device.ID, nil
}

func buildAnalyzed(
	in persistInput, pitch dbgen.Pitch, session dbgen.Session,
) *events.PitchAnalyzed {
	out := &events.PitchAnalyzed{
		PitchID:          pitch.ID,
		ExternalPitchUID: pitch.ExternalPitchUid,
		SessionID:        session.ID,
		PitcherAthleteID: in.pitcherID,
		BatterAthleteID:  in.batterID,
		Sequence: events.Sequence{
			AtBatNumber:       in.raw.AtBatNumber,
			PitchNumber:       in.raw.PitchNumber,
			SessionPitchIndex: pitch.SessionPitchIndex,
		},
		ThrownAt:         pitch.ThrownAt,
		Context:          in.raw.Context,
		Stand:            pitch.Stand,
		PThrows:          pitch.PThrows,
		PitchType:        in.raw.Measurement.PitchType,
		PitchName:        in.raw.Measurement.PitchName,
		Measurement:      in.normalized,
		Predictions:      in.predictions,
		PredictionStatus: in.status,
		IsSynthetic:      session.IsSynthetic,
	}
	if in.raw.GroundTruth != nil {
		out.ActualOutcome = in.raw.GroundTruth.Outcome
	}
	return out
}

func measurementParams(
	pitchID uuid.UUID, deviceID *uuid.UUID, m events.NormalizedMeasurement,
) dbgen.UpsertPitchMeasurementParams {
	return dbgen.UpsertPitchMeasurementParams{
		PitchID:              pitchID,
		DeviceID:             deviceID,
		ReleaseSpeed:         f32(m.ReleaseSpeed),
		ReleaseSpinRate:      f32(m.ReleaseSpinRate),
		SpinAxis:             f32(m.SpinAxis),
		ReleasePosX:          f32(m.ReleasePosX),
		ReleasePosY:          f32(m.ReleasePosY),
		ReleasePosZ:          f32(m.ReleasePosZ),
		ReleaseExtension:     f32(m.ReleaseExtension),
		PfxX:                 f32(m.PfxX),
		PfxZ:                 f32(m.PfxZ),
		PlateX:               f32(m.PlateX),
		PlateZ:               f32(m.PlateZ),
		SzTop:                f32(m.SzTop),
		SzBot:                f32(m.SzBot),
		EffectiveSpeed:       f32(m.EffectiveSpeed),
		ArmAngle:             f32(m.ArmAngle),
		PfxXArm:              f32(m.PfxXArm),
		ReleasePosXArm:       f32(m.ReleasePosXArm),
		PlateXBat:            f32(m.PlateXBat),
		ZoneHeightNorm:       f32(m.ZoneHeightNorm),
		InZone:               m.InZone,
		NormalizationVersion: int16(m.NormalizationVersion),
		MeasurementQuality:   m.MeasurementQuality,
	}
}

func (p *Pipeline) producerName() string {
	return "pitchlab-processor/" + p.version
}

// --- small conversions -----------------------------------------------------

func strPtr(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

func int16Ptr(v *int) *int16 {
	if v == nil {
		return nil
	}
	out := int16(*v)
	return &out
}

func f32(v *float64) *float32 {
	if v == nil {
		return nil
	}
	out := float32(*v)
	return &out
}

func groundTruth(gt *events.GroundTruth) *string {
	if gt == nil || gt.Outcome == "" {
		return nil
	}
	return &gt.Outcome
}

func displayName(name string, mlbamID int) string {
	if name != "" {
		return name
	}
	return fmt.Sprintf("Player %d", mlbamID)
}

func defaultStand(s string) string {
	if s == "" {
		return "R"
	}
	return s
}

func defaultDeviceKind(kind string) string {
	switch kind {
	case "REPLAY_SIMULATOR", "HAWKEYE", "TRACKMAN":
		return kind
	default:
		return "OTHER"
	}
}

func parseDate(s string) *time.Time {
	if s == "" {
		return nil
	}
	t, err := time.Parse("2006-01-02", s)
	if err != nil {
		return nil
	}
	return &t
}
