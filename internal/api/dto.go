// Package api implements the versioned HTTP surface under /api/v1.
package api

import (
	"encoding/json"
	"time"

	"github.com/google/uuid"

	"github.com/hkocamandev/pitchlab/internal/db/dbgen"
)

// Response shapes are declared separately from the database rows rather than
// serialising those directly. The database is free to gain a column without
// that becoming a public API change, and internal fields stay internal.

type AthleteDTO struct {
	ID          uuid.UUID  `json:"id"`
	MLBAMID     int32      `json:"mlbam_id"`
	FullName    string     `json:"full_name"`
	Throws      *string    `json:"throws"`
	Bats        *string    `json:"bats"`
	PrimaryRole string     `json:"primary_role"`
	BirthDate   *time.Time `json:"birth_date,omitempty"`
}

type AthleteListItemDTO struct {
	AthleteDTO
	SessionCount int64 `json:"session_count"`
	PitchCount   int64 `json:"pitch_count"`
}

type AthleteTotalsDTO struct {
	SessionCount int64      `json:"session_count"`
	PitchCount   int64      `json:"pitch_count"`
	FirstPitchAt *time.Time `json:"first_pitch_at,omitempty"`
	LastPitchAt  *time.Time `json:"last_pitch_at,omitempty"`
}

type AthleteDetailDTO struct {
	AthleteDTO
	Totals AthleteTotalsDTO `json:"totals"`
}

type SessionRefDTO struct {
	ID       uuid.UUID `json:"id"`
	FullName string    `json:"full_name"`
	Throws   *string   `json:"throws,omitempty"`
}

type SessionMetricsDTO struct {
	PitchCount          int32            `json:"pitch_count"`
	AvgReleaseSpeed     *float32         `json:"avg_release_speed"`
	AvgReleaseSpinRate  *float32         `json:"avg_release_spin_rate"`
	AvgPitchScore       *float32         `json:"avg_pitch_score"`
	OutcomeDistribution map[string]int64 `json:"outcome_distribution,omitempty"`
}

type SessionDTO struct {
	ID                 uuid.UUID  `json:"id"`
	ExternalSessionUID string     `json:"external_session_uid"`
	PitcherAthleteID   uuid.UUID  `json:"pitcher_athlete_id"`
	ExternalGameRef    *string    `json:"external_game_ref,omitempty"`
	SourceGameDate     *time.Time `json:"source_game_date,omitempty"`
	OpponentTeam       *string    `json:"opponent_team,omitempty"`
	StartedAt          time.Time  `json:"started_at"`
	EndedAt            *time.Time `json:"ended_at,omitempty"`
	Status             string     `json:"status"`
	// IsSynthetic is surfaced deliberately. A viewer must be able to tell
	// replayed data from real data without reading the source.
	IsSynthetic bool              `json:"is_synthetic"`
	Metrics     SessionMetricsDTO `json:"metrics"`
}

type SessionDetailDTO struct {
	SessionDTO
	Pitcher      *SessionRefDTO `json:"pitcher,omitempty"`
	AnomalyCount int64          `json:"anomaly_count"`
}

type PitchContextDTO struct {
	Balls        int16   `json:"balls"`
	Strikes      int16   `json:"strikes"`
	OutsWhenUp   int16   `json:"outs_when_up"`
	Inning       int16   `json:"inning"`
	InningTopBot *string `json:"inning_topbot,omitempty"`
	Stand        string  `json:"stand"`
	PThrows      string  `json:"p_throws"`
	On1B         bool    `json:"on_1b"`
	On2B         bool    `json:"on_2b"`
	On3B         bool    `json:"on_3b"`
}

type MeasurementDTO struct {
	ReleaseSpeed     *float32 `json:"release_speed"`
	ReleaseSpinRate  *float32 `json:"release_spin_rate"`
	SpinAxis         *float32 `json:"spin_axis"`
	PfxX             *float32 `json:"pfx_x"`
	PfxZ             *float32 `json:"pfx_z"`
	PlateX           *float32 `json:"plate_x"`
	PlateZ           *float32 `json:"plate_z"`
	ReleaseExtension *float32 `json:"release_extension"`
	ReleasePosX      *float32 `json:"release_pos_x"`
	ReleasePosZ      *float32 `json:"release_pos_z"`
	ArmAngle         *float32 `json:"arm_angle"`
	// Arm-frame values: positive means arm side for a pitcher, inside for a
	// batter, regardless of handedness.
	PfxXArm            *float32 `json:"pfx_x_arm"`
	PlateXBat          *float32 `json:"plate_x_bat"`
	ZoneHeightNorm     *float32 `json:"zone_height_norm"`
	InZone             *bool    `json:"in_zone"`
	MeasurementQuality string   `json:"measurement_quality"`
}

type PredictionDTO struct {
	ModelVersion     string             `json:"model_version"`
	Variant          string             `json:"variant"`
	Target           string             `json:"target"`
	Probabilities    map[string]float64 `json:"probabilities,omitempty"`
	PredictedClass   *string            `json:"predicted_class,omitempty"`
	WhiffProbability *float32           `json:"whiff_probability,omitempty"`
	ScoredQuantity   float32            `json:"scored_quantity"`
	Score            float32            `json:"score"`
}

type PitchDTO struct {
	ID                uuid.UUID       `json:"id"`
	SessionID         uuid.UUID       `json:"session_id"`
	SessionPitchIndex int32           `json:"session_pitch_index"`
	AtBatNumber       int16           `json:"at_bat_number"`
	PitchNumber       int16           `json:"pitch_number"`
	ThrownAt          time.Time       `json:"thrown_at"`
	Context           PitchContextDTO `json:"context"`
	PitchType         *string         `json:"pitch_type"`
	PitchName         *string         `json:"pitch_name"`
	Measurement       *MeasurementDTO `json:"measurement,omitempty"`
	Predictions       []PredictionDTO `json:"predictions,omitempty"`
	// ActualOutcome is ground truth, shown so the dashboard can put the
	// prediction next to what happened. It is never a model input.
	ActualOutcome    *string `json:"actual_outcome"`
	PredictionStatus string  `json:"prediction_status"`
	CorrelationID    *string `json:"correlation_id,omitempty"`
}

type AnomalyDTO struct {
	ID              uuid.UUID  `json:"id"`
	AthleteID       uuid.UUID  `json:"athlete_id"`
	SessionID       uuid.UUID  `json:"session_id"`
	Metric          string     `json:"metric"`
	PitchType       *string    `json:"pitch_type,omitempty"`
	ObservedValue   float32    `json:"observed_value"`
	BaselineMedian  float32    `json:"baseline_median"`
	RobustSigma     float32    `json:"robust_sigma"`
	ZRobust         float32    `json:"z_robust"`
	Direction       string     `json:"direction"`
	Severity        string     `json:"severity"`
	WindowSessions  int16      `json:"window_sessions"`
	DetectorVersion string     `json:"detector_version"`
	DetectedAt      time.Time  `json:"detected_at"`
	AcknowledgedAt  *time.Time `json:"acknowledged_at"`
}

type ModelPeriodDTO struct {
	Start *time.Time `json:"start,omitempty"`
	End   *time.Time `json:"end,omitempty"`
}

// ModelVersionDTO publishes the model card.
//
// Exposing it makes the system self-documenting: the dashboard footer can
// show which model produced a score and how it scored against its baseline,
// rather than presenting predictions as if they were certainties.
type ModelVersionDTO struct {
	ID           uuid.UUID       `json:"id"`
	Version      string          `json:"version"`
	Variant      string          `json:"variant"`
	Target       string          `json:"target"`
	Algorithm    string          `json:"algorithm"`
	IsActive     bool            `json:"is_active"`
	FeatureCount int             `json:"feature_count"`
	ClassLabels  []string        `json:"class_labels,omitempty"`
	TrainPeriod  ModelPeriodDTO  `json:"train_period"`
	ValPeriod    ModelPeriodDTO  `json:"val_period"`
	TestPeriod   ModelPeriodDTO  `json:"test_period"`
	DataSnapshot string          `json:"data_snapshot"`
	GitSHA       *string         `json:"git_sha,omitempty"`
	Metrics      json.RawMessage `json:"metrics,omitempty"`
	CreatedAt    time.Time       `json:"created_at"`
}

type AthleteSummaryDTO struct {
	PitchCount   int64 `json:"pitch_count"`
	SessionCount int64 `json:"session_count"`
	// Averages are omitted rather than reported as zero when there are no
	// pitches, because a zero average is a lie, not a measurement.
	AvgReleaseSpeed     *float64 `json:"avg_release_speed"`
	AvgReleaseSpinRate  *float64 `json:"avg_release_spin_rate"`
	AvgReleaseExtension *float64 `json:"avg_release_extension"`
	AvgPitchScore       *float64 `json:"avg_pitch_score"`
	WhiffRate           *float64 `json:"whiff_rate"`
	CalledStrikeRate    *float64 `json:"called_strike_rate"`
	InZoneRate          *float64 `json:"in_zone_rate"`
}

type PitchTypeStatDTO struct {
	PitchType          string   `json:"pitch_type"`
	PitchName          *string  `json:"pitch_name,omitempty"`
	Count              int64    `json:"count"`
	UsagePct           float64  `json:"usage_pct"`
	AvgReleaseSpeed    *float64 `json:"avg_release_speed"`
	AvgReleaseSpinRate *float64 `json:"avg_release_spin_rate"`
	AvgPfxXArm         *float64 `json:"avg_pfx_x_arm"`
	AvgPfxZ            *float64 `json:"avg_pfx_z"`
	AvgPitchScore      *float64 `json:"avg_pitch_score"`
	WhiffRate          *float64 `json:"whiff_rate"`
}

type TrendPointDTO struct {
	SessionID          uuid.UUID  `json:"session_id"`
	SourceGameDate     *time.Time `json:"source_game_date,omitempty"`
	StartedAt          time.Time  `json:"started_at"`
	PitchCount         int32      `json:"pitch_count"`
	AvgReleaseSpeed    *float32   `json:"avg_release_speed"`
	AvgReleaseSpinRate *float32   `json:"avg_release_spin_rate"`
	AvgPitchScore      *float32   `json:"avg_pitch_score"`
}

type AnalyticsDTO struct {
	AthleteID             uuid.UUID          `json:"athlete_id"`
	Period                ModelPeriodDTO     `json:"period"`
	Summary               AthleteSummaryDTO  `json:"summary"`
	PitchTypeDistribution []PitchTypeStatDTO `json:"pitch_type_distribution,omitempty"`
	Trend                 []TrendPointDTO    `json:"trend,omitempty"`
	OpenAnomalyCount      int64              `json:"open_anomaly_count"`
}

// --- mapping ---------------------------------------------------------------

func toAthleteDTO(a dbgen.Athlete) AthleteDTO {
	return AthleteDTO{
		ID: a.ID, MLBAMID: a.MlbamID, FullName: a.FullName,
		Throws: a.Throws, Bats: a.Bats, PrimaryRole: a.PrimaryRole,
		BirthDate: a.BirthDate,
	}
}

func toSessionDTO(s dbgen.Session) SessionDTO {
	return SessionDTO{
		ID: s.ID, ExternalSessionUID: s.ExternalSessionUid,
		PitcherAthleteID: s.PitcherAthleteID, ExternalGameRef: s.ExternalGameRef,
		SourceGameDate: s.SourceGameDate, OpponentTeam: s.OpponentTeam,
		StartedAt: s.StartedAt, EndedAt: s.EndedAt, Status: s.Status,
		IsSynthetic: s.IsSynthetic,
		Metrics: SessionMetricsDTO{
			PitchCount:         s.PitchCount,
			AvgReleaseSpeed:    s.AvgReleaseSpeed,
			AvgReleaseSpinRate: s.AvgReleaseSpinRate,
			AvgPitchScore:      s.AvgPitchScore,
		},
	}
}

func toMeasurementDTO(m dbgen.PitchMeasurement) *MeasurementDTO {
	// A LEFT JOIN with no match yields a zero-valued struct; the nil pitch id
	// is how that is told apart from a real all-null measurement row.
	if m.PitchID == uuid.Nil {
		return nil
	}
	return &MeasurementDTO{
		ReleaseSpeed: m.ReleaseSpeed, ReleaseSpinRate: m.ReleaseSpinRate,
		SpinAxis: m.SpinAxis, PfxX: m.PfxX, PfxZ: m.PfxZ,
		PlateX: m.PlateX, PlateZ: m.PlateZ,
		ReleaseExtension: m.ReleaseExtension,
		ReleasePosX:      m.ReleasePosX, ReleasePosZ: m.ReleasePosZ,
		ArmAngle: m.ArmAngle, PfxXArm: m.PfxXArm, PlateXBat: m.PlateXBat,
		ZoneHeightNorm: m.ZoneHeightNorm, InZone: m.InZone,
		MeasurementQuality: m.MeasurementQuality,
	}
}

func toPitchDTO(p dbgen.Pitch, m dbgen.PitchMeasurement) PitchDTO {
	dto := PitchDTO{
		ID: p.ID, SessionID: p.SessionID,
		SessionPitchIndex: p.SessionPitchIndex,
		AtBatNumber:       p.AtBatNumber, PitchNumber: p.PitchNumber,
		ThrownAt: p.ThrownAt,
		Context: PitchContextDTO{
			Balls: p.Balls, Strikes: p.Strikes, OutsWhenUp: p.OutsWhenUp,
			Inning: p.Inning, InningTopBot: p.InningTopbot,
			Stand: p.Stand, PThrows: p.PThrows,
			On1B: p.On1b, On2B: p.On2b, On3B: p.On3b,
		},
		PitchType: p.PitchType, PitchName: p.PitchName,
		Measurement:      toMeasurementDTO(m),
		ActualOutcome:    p.ActualOutcome,
		PredictionStatus: p.PredictionStatus,
	}
	if p.CorrelationID != nil {
		s := p.CorrelationID.String()
		dto.CorrelationID = &s
	}
	return dto
}

func toPredictionDTO(pr dbgen.PitchPrediction, mv dbgen.ModelVersion) PredictionDTO {
	dto := PredictionDTO{
		ModelVersion: mv.Version, Variant: mv.Variant, Target: mv.Target,
		PredictedClass:   pr.PredictedClass,
		WhiffProbability: pr.WhiffProbability,
		ScoredQuantity:   pr.ScoredQuantity,
		Score:            pr.Score,
	}
	if len(pr.Probabilities) > 0 {
		var probs map[string]float64
		if err := json.Unmarshal(pr.Probabilities, &probs); err == nil {
			dto.Probabilities = probs
		}
	}
	return dto
}

func toAnomalyDTO(a dbgen.PerformanceAnomaly) AnomalyDTO {
	return AnomalyDTO{
		ID: a.ID, AthleteID: a.AthleteID, SessionID: a.SessionID,
		Metric: a.Metric, PitchType: a.PitchType,
		ObservedValue: a.ObservedValue, BaselineMedian: a.BaselineMedian,
		RobustSigma: a.RobustSigma, ZRobust: a.ZRobust,
		Direction: a.Direction, Severity: a.Severity,
		WindowSessions: a.WindowSessions, DetectorVersion: a.DetectorVersion,
		DetectedAt: a.DetectedAt, AcknowledgedAt: a.AcknowledgedAt,
	}
}

func toModelVersionDTO(mv dbgen.ModelVersion) ModelVersionDTO {
	dto := ModelVersionDTO{
		ID: mv.ID, Version: mv.Version, Variant: mv.Variant,
		Target: mv.Target, Algorithm: mv.Algorithm, IsActive: mv.IsActive,
		TrainPeriod:  ModelPeriodDTO{Start: &mv.TrainPeriodStart, End: &mv.TrainPeriodEnd},
		ValPeriod:    ModelPeriodDTO{Start: mv.ValPeriodStart, End: mv.ValPeriodEnd},
		TestPeriod:   ModelPeriodDTO{Start: mv.TestPeriodStart, End: mv.TestPeriodEnd},
		DataSnapshot: mv.DataSnapshot, GitSHA: mv.GitSha,
		Metrics:   json.RawMessage(mv.Metrics),
		CreatedAt: mv.CreatedAt,
	}
	var cols []string
	if err := json.Unmarshal(mv.FeatureColumns, &cols); err == nil {
		dto.FeatureCount = len(cols)
	}
	if len(mv.ClassLabels) > 0 {
		var labels []string
		if err := json.Unmarshal(mv.ClassLabels, &labels); err == nil {
			dto.ClassLabels = labels
		}
	}
	return dto
}

// nilIfEmpty suppresses an aggregate when there were no rows behind it.
func nilIfEmpty(v float64, pitchCount int64) *float64 {
	if pitchCount == 0 {
		return nil
	}
	return &v
}

// Postgres aggregates over an empty set are NULL, and sqlc cannot always tell
// that from the query text, so a few columns arrive as interface{}. Casting
// them in SQL only moves the problem: sqlc then emits a non-pointer type that
// fails at scan time on precisely the rows that are null. Converting here
// keeps the nullability visible instead of hiding it behind a wrong type.

func asTimePtr(v any) *time.Time {
	t, ok := v.(time.Time)
	if !ok {
		return nil
	}
	return &t
}

func asStringPtr(v any) *string {
	s, ok := v.(string)
	if !ok || s == "" {
		return nil
	}
	return &s
}
