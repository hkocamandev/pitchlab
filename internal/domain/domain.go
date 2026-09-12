// Package domain holds the entities the whole system agrees on.
//
// Seven entities, and the two that are deliberately absent are as much part
// of the design as the ones present:
//
//	Pitcher / Batter  are roles, not identities. The same person can be both,
//	                  MLBAM uses one player id, and separate tables would
//	                  force a UNION for "everything this person did". The
//	                  role lives in Pitch's foreign keys.
//	Game / PlateAppearance  add a join layer without enabling any query.
//	                  Game data denormalizes onto Session; a plate appearance
//	                  is fully described by Pitch's sequence fields.
package domain

import (
	"time"

	"github.com/google/uuid"
)

// ---------------------------------------------------------------------------
// Enumerations. Every one of these is mirrored by a CHECK constraint, so an
// invalid value fails at the database rather than propagating.
// ---------------------------------------------------------------------------

// Handedness is which side a player throws or bats from.
type Handedness string

const (
	HandLeft   Handedness = "L"
	HandRight  Handedness = "R"
	HandSwitch Handedness = "S" // batters only
)

// AthleteRole is a presentation hint. The authoritative role is which foreign
// key on Pitch points at the athlete.
type AthleteRole string

const (
	RoleUnknown AthleteRole = "UNKNOWN"
	RolePitcher AthleteRole = "PITCHER"
	RoleBatter  AthleteRole = "BATTER"
	RoleTwoWay  AthleteRole = "TWO_WAY"
)

// DeviceKind identifies the measurement source. REPLAY_SIMULATOR is what
// makes simulated data distinguishable from real data in the database.
type DeviceKind string

const (
	DeviceReplaySimulator DeviceKind = "REPLAY_SIMULATOR"
	DeviceHawkEye         DeviceKind = "HAWKEYE"
	DeviceTrackMan        DeviceKind = "TRACKMAN"
	DeviceOther           DeviceKind = "OTHER"
)

// SessionStatus is the outing's state machine.
type SessionStatus string

const (
	SessionActive    SessionStatus = "ACTIVE"
	SessionCompleted SessionStatus = "COMPLETED"
	SessionAborted   SessionStatus = "ABORTED"
)

// PitchOutcome is the 5-class target. OutcomeOther exists only for rows whose
// source description fell outside the competitive-pitch set.
type PitchOutcome string

const (
	OutcomeBall           PitchOutcome = "BALL"
	OutcomeCalledStrike   PitchOutcome = "CALLED_STRIKE"
	OutcomeSwingingStrike PitchOutcome = "SWINGING_STRIKE"
	OutcomeFoul           PitchOutcome = "FOUL"
	OutcomeInPlay         PitchOutcome = "IN_PLAY"
	OutcomeOther          PitchOutcome = "OTHER"
)

// OutcomeClasses is the model's class set, in the order the probability
// vector uses. The order is part of the contract with the ML service: it is
// not alphabetical, and treating it as such silently mispairs every class.
var OutcomeClasses = [5]PitchOutcome{
	OutcomeBall,
	OutcomeCalledStrike,
	OutcomeSwingingStrike,
	OutcomeFoul,
	OutcomeInPlay,
}

// IsSwing reports whether the outcome implies the batter offered at the
// pitch. This is the population the stuff model is trained on.
func (o PitchOutcome) IsSwing() bool {
	switch o {
	case OutcomeSwingingStrike, OutcomeFoul, OutcomeInPlay:
		return true
	default:
		return false
	}
}

// PredictionStatus records whether inference succeeded for a pitch. A pitch
// is persisted even when the ML service is unavailable; the data is never
// lost to a downstream dependency.
type PredictionStatus string

const (
	PredictionPending PredictionStatus = "PENDING"
	PredictionOK      PredictionStatus = "OK"
	PredictionFailed  PredictionStatus = "FAILED"
	PredictionSkipped PredictionStatus = "SKIPPED"
)

// ModelVariant distinguishes the two models, which answer different
// questions on different populations.
type ModelVariant string

const (
	// VariantPitching is the 5-class outcome model over every pitch.
	VariantPitching ModelVariant = "pitching"
	// VariantStuff is P(whiff | swing) on physical features alone.
	VariantStuff ModelVariant = "stuff"
)

// ModelTarget is what a model version predicts.
type ModelTarget string

const (
	TargetOutcome5Class    ModelTarget = "outcome_5class"
	TargetWhiffGivenSwing  ModelTarget = "whiff_given_swing"
)

// MeasurementQuality flags device readings that came back incomplete.
type MeasurementQuality string

const (
	QualityOK      MeasurementQuality = "OK"
	QualityPartial MeasurementQuality = "PARTIAL"
	QualitySuspect MeasurementQuality = "SUSPECT"
)

// AnomalyMetric is the set of pitcher-controlled physical quantities the
// anomaly detector watches. Outcome metrics are deliberately excluded: they
// are noisy in small samples and contaminated by defence and luck.
type AnomalyMetric string

const (
	MetricReleaseSpeed     AnomalyMetric = "release_speed"
	MetricReleaseSpinRate  AnomalyMetric = "release_spin_rate"
	MetricReleasePosXArm   AnomalyMetric = "release_pos_x_arm"
	MetricReleasePosZ      AnomalyMetric = "release_pos_z"
	MetricReleaseExtension AnomalyMetric = "release_extension"
	MetricPfxXArm          AnomalyMetric = "pfx_x_arm"
	MetricPfxZ             AnomalyMetric = "pfx_z"
)

// AnomalyDirection says which way the metric moved. Both directions are
// reported; a velocity gain is not the same event as a velocity loss.
type AnomalyDirection string

const (
	DirectionIncrease AnomalyDirection = "INCREASE"
	DirectionDecrease AnomalyDirection = "DECREASE"
)

// AnomalySeverity is derived from the robust z magnitude.
type AnomalySeverity string

const (
	SeverityMedium AnomalySeverity = "MEDIUM"
	SeverityHigh   AnomalySeverity = "HIGH"
)

// ---------------------------------------------------------------------------
// Entities
// ---------------------------------------------------------------------------

// Athlete is a person. Roles are relationships, not subtypes.
type Athlete struct {
	ID          uuid.UUID
	MLBAMID     int32
	FullName    string
	Throws      *Handedness
	Bats        *Handedness
	PrimaryRole AthleteRole
	BirthDate   *time.Time
	CreatedAt   time.Time
	UpdatedAt   time.Time
}

// Device is a measurement source. Lightweight, but it is what keeps
// provenance answerable and simulated data labelled as such.
type Device struct {
	ID                 uuid.UUID
	DeviceKey          string
	DeviceKind         DeviceKind
	DisplayName        *string
	CalibrationProfile []byte // JSONB
	CreatedAt          time.Time
}

// Session is a pitcher's outing. It carries three architectural roles: the
// WebSocket channel boundary, the Kafka partition key, and the aggregation
// unit for anomaly detection.
type Session struct {
	ID                 uuid.UUID
	ExternalSessionUID string
	PitcherAthleteID   uuid.UUID
	DeviceID           *uuid.UUID

	ExternalGameRef *string
	// SourceGameDate is the real calendar date of the source game, kept
	// separate from StartedAt, which is synthetic under replay.
	SourceGameDate *time.Time
	OpponentTeam   *string

	StartedAt   time.Time
	EndedAt     *time.Time
	Status      SessionStatus
	IsSynthetic bool

	PitchCount         int32
	AvgReleaseSpeed    *float32
	AvgReleaseSpinRate *float32
	AvgPitchScore      *float32

	CreatedAt time.Time
	UpdatedAt time.Time
}

// Pitch is the central event: identity, sequencing, and pre-pitch context.
type Pitch struct {
	ID               uuid.UUID
	ExternalPitchUID string
	SessionID        uuid.UUID
	PitcherAthleteID uuid.UUID
	BatterAthleteID  *uuid.UUID

	AtBatNumber       int16
	PitchNumber       int16
	SessionPitchIndex int32
	ThrownAt          time.Time

	Balls             int16
	Strikes           int16
	OutsWhenUp        int16
	Inning            int16
	InningTopBot      *string
	Stand             Handedness
	PThrows           Handedness
	On1B              bool
	On2B              bool
	On3B              bool
	BatScore          *int16
	FldScore          *int16
	NThruOrderPitcher *int16

	PitchType *string
	PitchName *string

	// ActualOutcome is ground truth for prediction-versus-reality display and
	// production monitoring. It is never a model feature; the feature builder
	// takes an explicit struct that has no field for it, so this cannot reach
	// the model even by accident.
	ActualOutcome    *PitchOutcome
	PredictionStatus PredictionStatus

	CorrelationID *uuid.UUID
	SourceEventID *uuid.UUID

	CreatedAt time.Time
}

// CountState renders the pre-pitch count as Statcast writes it.
func (p Pitch) CountState() string {
	return string(rune('0'+p.Balls)) + "-" + string(rune('0'+p.Strikes))
}

// PitchMeasurement is the device payload for a pitch, kept separate because
// it differs in provenance, null profile, lifecycle and row width.
type PitchMeasurement struct {
	PitchID  uuid.UUID
	DeviceID *uuid.UUID

	ReleaseSpeed     *float32
	ReleaseSpinRate  *float32
	SpinAxis         *float32
	ReleasePosX      *float32
	ReleasePosY      *float32
	ReleasePosZ      *float32
	ReleaseExtension *float32
	PfxX             *float32
	PfxZ             *float32
	PlateX           *float32
	PlateZ           *float32
	SzTop            *float32
	SzBot            *float32
	EffectiveSpeed   *float32
	ArmAngle         *float32

	// Derived, handedness-neutral frame: positive means arm side for a
	// pitcher and inside for a batter. The direction was established from
	// data, not from geometry.
	PfxXArm         *float32
	ReleasePosXArm  *float32
	PlateXBat       *float32
	ZoneHeightNorm  *float32
	InZone          *bool

	// NormalizationVersion records which pass produced the derived columns.
	// Without it, a partial re-normalization silently mixes conventions.
	NormalizationVersion int16
	MeasurementQuality   MeasurementQuality
	CreatedAt            time.Time
}

// ModelVersion is the model card. A prediction without it is uninterpretable.
type ModelVersion struct {
	ID        uuid.UUID
	Version   string
	Variant   ModelVariant
	Target    ModelTarget
	Algorithm string

	FeatureColumns []byte // JSONB: the ordered feature contract
	ClassLabels    []byte // JSONB, 5-class model only
	RunValueTable  []byte // JSONB, 5-class model only
	ScoreMu        float32
	ScoreSigma     float32

	TrainPeriodStart time.Time
	TrainPeriodEnd   time.Time
	ValPeriodStart   *time.Time
	ValPeriodEnd     *time.Time
	TestPeriodStart  *time.Time
	TestPeriodEnd    *time.Time
	DataSnapshot     string
	GitSHA           *string

	Metrics   []byte // JSONB
	IsActive  bool
	CreatedAt time.Time
}

// PitchPrediction is one model's output for one pitch. Separate from Pitch
// because a pitch has N predictions: two variants now, plus every retrained
// version, which is what makes "is v1.1 better than v1.0" answerable.
type PitchPrediction struct {
	ID             uuid.UUID
	PitchID        uuid.UUID
	ModelVersionID uuid.UUID

	// Probabilities and PredictedClass are set by the 5-class model;
	// WhiffProbability by the binary stuff model. Exactly one shape is
	// populated, enforced by a CHECK constraint.
	Probabilities    []byte // JSONB
	PredictedClass   *PitchOutcome
	WhiffProbability *float32

	// ScoredQuantity is the raw model-derived number the score comes from:
	// expected run value for the outcome model, negated whiff probability for
	// the stuff model. Stored so a displayed score can be traced back.
	ScoredQuantity float32
	Score          float32

	InferenceMs *float32
	CreatedAt   time.Time
}

// PerformanceAnomaly is a session-level deviation from the pitcher's own
// robust baseline. The baseline is stored with the finding because the
// trailing window moves on and recomputing it later gives a different answer.
type PerformanceAnomaly struct {
	ID        uuid.UUID
	AthleteID uuid.UUID
	SessionID uuid.UUID

	Metric          AnomalyMetric
	PitchType       *string
	ObservedValue   float32
	BaselineMedian  float32
	RobustSigma     float32
	ZRobust         float32
	Direction       AnomalyDirection
	Severity        AnomalySeverity
	WindowSessions  int16
	DetectorVersion string

	DetectedAt     time.Time
	AcknowledgedAt *time.Time
}
