// Package anomaly detects departures from a pitcher's own physical baseline.
//
// The method is a robust rolling z-score: median and MAD over a trailing
// window of that pitcher's own outings, per pitch type.
//
// Mean and standard deviation were rejected. A single injury outing inside the
// window inflates sigma, and every genuine anomaly after it falls inside the
// widened band -- the one event that most needed flagging is what stops the
// detector from flagging anything else. The median has a 50% breakdown point,
// so one bad outing cannot move it.
//
// Isolation Forest was evaluated and rejected as well: the consumer of this
// output is a pitching coach, and "your anomaly score is -0.62" is not
// actionable. It also needs per-pitcher training data, so it cold-starts
// badly, and its contamination parameter is exactly the kind of hand-chosen
// constant this project avoids elsewhere. "Your fastball is 1.8 mph below the
// median of your last ten outings" needs no explanation at all.
package anomaly

import (
	"math"
	"sort"
	"time"

	"github.com/google/uuid"
)

// DetectorVersion identifies the algorithm behind a finding. It is stored
// with every anomaly so a later change to the method stays distinguishable
// from a change in the pitcher.
const DetectorVersion = "robust-mad-v1"

// Metric is a tracked physical quantity.
//
// Only quantities the pitcher controls. Outcome metrics -- whiff rate, ERA --
// are deliberately absent: they are extremely noisy in small samples and
// contaminated by defence and luck, so a change in them says as much about
// the fielders as the pitcher.
type Metric string

const (
	MetricReleaseSpeed     Metric = "release_speed"
	MetricReleaseSpinRate  Metric = "release_spin_rate"
	MetricReleasePosXArm   Metric = "release_pos_x_arm"
	MetricReleasePosZ      Metric = "release_pos_z"
	MetricReleaseExtension Metric = "release_extension"
	MetricPfxXArm          Metric = "pfx_x_arm"
	MetricPfxZ             Metric = "pfx_z"
)

// AllMetrics is the set evaluated for every session.
var AllMetrics = []Metric{
	MetricReleaseSpeed,
	MetricReleaseSpinRate,
	MetricReleasePosXArm,
	MetricReleasePosZ,
	MetricReleaseExtension,
	MetricPfxXArm,
	MetricPfxZ,
}

// Direction is which way the metric moved.
type Direction string

const (
	DirectionIncrease Direction = "INCREASE"
	DirectionDecrease Direction = "DECREASE"
)

// Severity grades a finding by the magnitude of its robust z.
type Severity string

const (
	SeverityMedium Severity = "MEDIUM"
	SeverityHigh   Severity = "HIGH"
)

// Config tunes the detector.
type Config struct {
	// WindowSessions is how many prior outings form the baseline. Ten is
	// roughly two months for a starter: long enough to be stable, short
	// enough to still describe the pitcher as he is now.
	WindowSessions int

	// MinWindowSessions is the fewest prior outings that will produce a
	// finding. Below it the detector stays silent, which is the right
	// behaviour: with three data points there is no baseline to depart from.
	MinWindowSessions int

	// MinPitchesPerSession excludes outings too thin to have a reliable
	// median of their own.
	MinPitchesPerSession int

	// HighThreshold and MediumThreshold are robust-z cutoffs. Three sigma is
	// roughly a 0.3% false-alarm rate under normality.
	HighThreshold   float64
	MediumThreshold float64

	// SigmaFloor is a per-metric lower bound on the robust sigma.
	//
	// Without it, a pitcher whose last ten outings all read 94.0 mph gets a
	// MAD of zero, and the next reading of 94.1 divides by zero into an
	// infinite z. The floor encodes what counts as measurement noise for each
	// quantity, so "suspiciously consistent" does not become "every pitch is
	// an anomaly".
	SigmaFloor map[Metric]float64
}

// DefaultConfig returns the tuning used in production.
func DefaultConfig() Config {
	return Config{
		WindowSessions:       10,
		MinWindowSessions:    5,
		MinPitchesPerSession: 5,
		HighThreshold:        3.0,
		MediumThreshold:      2.5,
		SigmaFloor: map[Metric]float64{
			MetricReleaseSpeed:     0.25, // mph
			MetricReleaseSpinRate:  25.0, // rpm
			MetricReleasePosXArm:   0.02, // feet
			MetricReleasePosZ:      0.02, // feet
			MetricReleaseExtension: 0.03, // feet
			MetricPfxXArm:          0.03, // feet
			MetricPfxZ:             0.03, // feet
		},
	}
}

// SessionPoint is one prior outing's median for a metric.
type SessionPoint struct {
	SessionID  uuid.UUID
	StartedAt  time.Time
	PitchCount int
	Value      float64
}

// Observation is the session under evaluation.
type Observation struct {
	AthleteID  uuid.UUID
	SessionID  uuid.UUID
	PitchType  string
	Metric     Metric
	Value      float64
	PitchCount int
	DetectedAt time.Time
}

// Finding is a detected departure from baseline.
//
// The baseline travels with the finding rather than being recomputed on
// demand: the trailing window moves, so recomputing later gives a different
// answer and "why was this flagged" becomes unanswerable.
type Finding struct {
	AthleteID uuid.UUID
	SessionID uuid.UUID
	Metric    Metric
	PitchType string

	ObservedValue  float64
	BaselineMedian float64
	RobustSigma    float64
	ZRobust        float64

	Direction       Direction
	Severity        Severity
	WindowSessions  int
	DetectorVersion string
	DetectedAt      time.Time
}

// Detector evaluates observations against a baseline.
type Detector struct {
	cfg Config
}

// New builds a detector.
func New(cfg Config) *Detector {
	if cfg.WindowSessions <= 0 {
		cfg.WindowSessions = 10
	}
	if cfg.MinWindowSessions <= 0 {
		cfg.MinWindowSessions = 5
	}
	if cfg.MinPitchesPerSession <= 0 {
		cfg.MinPitchesPerSession = 5
	}
	if cfg.HighThreshold <= 0 {
		cfg.HighThreshold = 3.0
	}
	if cfg.MediumThreshold <= 0 {
		cfg.MediumThreshold = 2.5
	}
	if cfg.SigmaFloor == nil {
		cfg.SigmaFloor = DefaultConfig().SigmaFloor
	}
	return &Detector{cfg: cfg}
}

// Evaluate compares one observation against its baseline window.
//
// Returns nil when there is nothing to report, which includes the case of too
// little history. Silence on thin data is a feature: a detector that fires on
// three data points trains its users to ignore it.
func (d *Detector) Evaluate(obs Observation, window []SessionPoint) *Finding {
	if obs.PitchCount < d.cfg.MinPitchesPerSession {
		return nil
	}

	usable := make([]float64, 0, len(window))
	for _, p := range window {
		if p.PitchCount >= d.cfg.MinPitchesPerSession && !math.IsNaN(p.Value) {
			usable = append(usable, p.Value)
		}
		if len(usable) >= d.cfg.WindowSessions {
			break
		}
	}
	if len(usable) < d.cfg.MinWindowSessions {
		return nil
	}

	median := Median(usable)
	sigma := RobustSigma(usable)

	if floor, ok := d.cfg.SigmaFloor[obs.Metric]; ok && sigma < floor {
		sigma = floor
	}
	if sigma <= 0 || math.IsNaN(sigma) {
		return nil
	}

	z := (obs.Value - median) / sigma
	if math.IsNaN(z) || math.IsInf(z, 0) {
		return nil
	}

	abs := math.Abs(z)
	var severity Severity
	switch {
	case abs >= d.cfg.HighThreshold:
		severity = SeverityHigh
	case abs >= d.cfg.MediumThreshold:
		severity = SeverityMedium
	default:
		return nil
	}

	direction := DirectionIncrease
	if z < 0 {
		direction = DirectionDecrease
	}

	detectedAt := obs.DetectedAt
	if detectedAt.IsZero() {
		detectedAt = time.Now().UTC()
	}

	return &Finding{
		AthleteID:       obs.AthleteID,
		SessionID:       obs.SessionID,
		Metric:          obs.Metric,
		PitchType:       obs.PitchType,
		ObservedValue:   obs.Value,
		BaselineMedian:  median,
		RobustSigma:     sigma,
		ZRobust:         z,
		Direction:       direction,
		Severity:        severity,
		WindowSessions:  len(usable),
		DetectorVersion: DetectorVersion,
		DetectedAt:      detectedAt,
	}
}

// Median returns the middle value. Does not modify its input.
func Median(values []float64) float64 {
	if len(values) == 0 {
		return math.NaN()
	}
	sorted := append([]float64(nil), values...)
	sort.Float64s(sorted)

	mid := len(sorted) / 2
	if len(sorted)%2 == 1 {
		return sorted[mid]
	}
	return (sorted[mid-1] + sorted[mid]) / 2
}

// MAD is the median absolute deviation from the median.
func MAD(values []float64) float64 {
	if len(values) == 0 {
		return math.NaN()
	}
	median := Median(values)
	deviations := make([]float64, len(values))
	for i, v := range values {
		deviations[i] = math.Abs(v - median)
	}
	return Median(deviations)
}

// madScale converts MAD into a standard-deviation-equivalent.
//
// 1/Phi^-1(0.75) ~= 1.4826. It makes MAD a consistent estimator of sigma for
// normally distributed data, so the familiar three-sigma intuition still
// applies to the robust z.
const madScale = 1.4826

// RobustSigma estimates the scale of a sample without letting outliers set it.
func RobustSigma(values []float64) float64 {
	return madScale * MAD(values)
}
