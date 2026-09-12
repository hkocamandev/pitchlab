package events

import (
	"time"

	"github.com/google/uuid"
)

// PitchRaw is what a tracking device emits.
//
// Deliberately un-normalized: the arm-frame mirroring, the circular spin
// encoding and the zone normalization are domain rules, and domain rules
// belong in the processor where they are tested, not in whatever produced the
// reading. A real device does not know about handedness conventions.
//
// Equally deliberately, there are no outcome fields here beyond the
// GroundTruth block. A camera cannot know a pitch's exit velocity or its
// change in run expectancy, so the simulator must not be able to send them.
// Imitating the device faithfully is what makes the leakage defense
// structural rather than a matter of discipline.
type PitchRaw struct {
	DeviceID   string `json:"device_id"`
	DeviceKind string `json:"device_kind"`

	ExternalGameRef string `json:"external_game_ref"`
	AtBatNumber     int    `json:"at_bat_number"`
	PitchNumber     int    `json:"pitch_number"`

	// ThrownAt is synthesized under replay. SourceGameDate keeps the real
	// calendar date so the two can never be confused.
	ThrownAt       time.Time `json:"thrown_at"`
	SourceGameDate string    `json:"source_game_date,omitempty"`

	Session SessionRef   `json:"session"`
	Batter  BatterRef    `json:"batter"`
	Context PitchContext `json:"context"`

	Measurement RawMeasurement `json:"measurement"`

	// GroundTruth is what actually happened. It is carried in its own block,
	// not mixed into the measurement, because it must reach exactly one
	// place: the pitches.actual_outcome column, for showing predictions next
	// to reality. A real device integration leaves this empty.
	GroundTruth *GroundTruth `json:"ground_truth,omitempty"`
}

// SessionRef identifies the outing and its pitcher.
type SessionRef struct {
	SessionUID     string `json:"session_uid"`
	PitcherMLBAMID int    `json:"pitcher_mlbam_id"`
	PitcherName    string `json:"pitcher_name,omitempty"`
	PThrows        string `json:"p_throws"`
	OpponentTeam   string `json:"opponent_team,omitempty"`
}

// BatterRef identifies the batter.
type BatterRef struct {
	MLBAMID int    `json:"mlbam_id"`
	Name    string `json:"name,omitempty"`
	Stand   string `json:"stand"`
}

// PitchContext is the pre-pitch game state. Statcast documents every one of
// these as pre-pitch, which is what makes them safe as model features.
type PitchContext struct {
	Balls        int    `json:"balls"`
	Strikes      int    `json:"strikes"`
	OutsWhenUp   int    `json:"outs_when_up"`
	Inning       int    `json:"inning"`
	InningTopBot string `json:"inning_topbot,omitempty"`

	On1B bool `json:"on_1b"`
	On2B bool `json:"on_2b"`
	On3B bool `json:"on_3b"`

	BatScore          *int `json:"bat_score,omitempty"`
	FldScore          *int `json:"fld_score,omitempty"`
	NThruOrderPitcher *int `json:"n_thruorder_pitcher,omitempty"`
}

// RawMeasurement is the device reading, in the device's own frame.
type RawMeasurement struct {
	PitchType string `json:"pitch_type,omitempty"`
	PitchName string `json:"pitch_name,omitempty"`

	ReleaseSpeed     *float64 `json:"release_speed,omitempty"`
	ReleaseSpinRate  *float64 `json:"release_spin_rate,omitempty"`
	SpinAxis         *float64 `json:"spin_axis,omitempty"`
	ReleasePosX      *float64 `json:"release_pos_x,omitempty"`
	ReleasePosY      *float64 `json:"release_pos_y,omitempty"`
	ReleasePosZ      *float64 `json:"release_pos_z,omitempty"`
	ReleaseExtension *float64 `json:"release_extension,omitempty"`
	PfxX             *float64 `json:"pfx_x,omitempty"`
	PfxZ             *float64 `json:"pfx_z,omitempty"`
	PlateX           *float64 `json:"plate_x,omitempty"`
	PlateZ           *float64 `json:"plate_z,omitempty"`
	SzTop            *float64 `json:"sz_top,omitempty"`
	SzBot            *float64 `json:"sz_bot,omitempty"`
	EffectiveSpeed   *float64 `json:"effective_speed,omitempty"`
	ArmAngle         *float64 `json:"arm_angle,omitempty"`
}

// GroundTruth is the observed outcome, for comparison only.
type GroundTruth struct {
	Outcome string `json:"outcome"`
}

// PitchUID returns this pitch's natural key.
func (p PitchRaw) PitchUID() string {
	return PitchUID(p.ExternalGameRef, p.AtBatNumber, p.PitchNumber)
}

// PitchAnalyzed is a normalized, scored, persisted pitch.
//
// Published after the database write, not before: a consumer that reacts to
// this event and then reads the pitch back must find it there.
type PitchAnalyzed struct {
	PitchID          uuid.UUID  `json:"pitch_id"`
	ExternalPitchUID string     `json:"external_pitch_uid"`
	SessionID        uuid.UUID  `json:"session_id"`
	PitcherAthleteID uuid.UUID  `json:"pitcher_athlete_id"`
	BatterAthleteID  *uuid.UUID `json:"batter_athlete_id,omitempty"`

	Sequence Sequence     `json:"sequence"`
	ThrownAt time.Time    `json:"thrown_at"`
	Context  PitchContext `json:"context"`
	Stand    string       `json:"stand"`
	PThrows  string       `json:"p_throws"`

	PitchType string `json:"pitch_type,omitempty"`
	PitchName string `json:"pitch_name,omitempty"`

	Measurement NormalizedMeasurement `json:"measurement"`
	Predictions []PredictionResult    `json:"predictions,omitempty"`

	ActualOutcome    string `json:"actual_outcome,omitempty"`
	PredictionStatus string `json:"prediction_status"`
	IsSynthetic      bool   `json:"is_synthetic"`
}

// Sequence locates a pitch within its outing.
type Sequence struct {
	AtBatNumber       int   `json:"at_bat_number"`
	PitchNumber       int   `json:"pitch_number"`
	SessionPitchIndex int32 `json:"session_pitch_index"`
}

// NormalizedMeasurement is the reading after the domain rules are applied.
type NormalizedMeasurement struct {
	RawMeasurement

	// Arm-frame values: positive means arm side for a pitcher and inside for
	// a batter, whichever hand is involved.
	PfxXArm        *float64 `json:"pfx_x_arm,omitempty"`
	ReleasePosXArm *float64 `json:"release_pos_x_arm,omitempty"`
	PlateXBat      *float64 `json:"plate_x_bat,omitempty"`
	ZoneHeightNorm *float64 `json:"zone_height_norm,omitempty"`
	InZone         *bool    `json:"in_zone,omitempty"`

	NormalizationVersion int    `json:"normalization_version"`
	MeasurementQuality   string `json:"measurement_quality"`
}

// PredictionResult is one model's output.
type PredictionResult struct {
	Variant          string             `json:"variant"`
	ModelVersion     string             `json:"model_version"`
	Target           string             `json:"target"`
	Probabilities    map[string]float64 `json:"probabilities,omitempty"`
	PredictedClass   string             `json:"predicted_class,omitempty"`
	WhiffProbability *float64           `json:"whiff_probability,omitempty"`
	ScoredQuantity   float64            `json:"scored_quantity"`
	Score            float64            `json:"score"`
}

// AnomalyDetected is a deviation from a pitcher's own baseline.
//
// Its own topic rather than a flag on PitchAnalyzed: different granularity
// (session, not pitch), different timing (minutes later), different partition
// key (athlete, not session), and vastly different volume. Folding a rare,
// high-value event into a high-volume stream buries it.
type AnomalyDetected struct {
	AnomalyID uuid.UUID `json:"anomaly_id"`
	AthleteID uuid.UUID `json:"athlete_id"`
	SessionID uuid.UUID `json:"session_id"`

	Metric    string `json:"metric"`
	PitchType string `json:"pitch_type,omitempty"`

	ObservedValue float64 `json:"observed_value"`
	// The baseline travels with the finding. Recomputing it later gives a
	// different answer, because the trailing window has moved on.
	BaselineMedian float64 `json:"baseline_median"`
	RobustSigma    float64 `json:"robust_sigma"`
	ZRobust        float64 `json:"z_robust"`

	Direction       string    `json:"direction"`
	Severity        string    `json:"severity"`
	WindowSessions  int       `json:"window_sessions"`
	DetectorVersion string    `json:"detector_version"`
	DetectedAt      time.Time `json:"detected_at"`
}

// SessionCompleted signals that an outing is over and can be evaluated.
type SessionCompleted struct {
	SessionID  uuid.UUID `json:"session_id"`
	SessionUID string    `json:"session_uid"`
	PitchCount int       `json:"pitch_count"`
	Reason     string    `json:"reason"`
}
