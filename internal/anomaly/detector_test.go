package anomaly

import (
	"math"
	"testing"
	"time"

	"github.com/google/uuid"
)

func points(values ...float64) []SessionPoint {
	out := make([]SessionPoint, len(values))
	base := time.Now().UTC().Add(-time.Duration(len(values)) * 24 * time.Hour)
	for i, v := range values {
		out[i] = SessionPoint{
			SessionID:  uuid.New(),
			StartedAt:  base.Add(time.Duration(i) * 24 * time.Hour),
			PitchCount: 90,
			Value:      v,
		}
	}
	return out
}

func obs(metric Metric, value float64) Observation {
	return Observation{
		AthleteID: uuid.New(), SessionID: uuid.New(),
		PitchType: "FF", Metric: metric, Value: value, PitchCount: 90,
		DetectedAt: time.Now().UTC(),
	}
}

// --- the statistics --------------------------------------------------------

func TestMedian(t *testing.T) {
	cases := []struct {
		in   []float64
		want float64
	}{
		{[]float64{1, 2, 3}, 2},
		{[]float64{1, 2, 3, 4}, 2.5},
		{[]float64{3, 1, 2}, 2},
		{[]float64{5}, 5},
	}
	for _, tc := range cases {
		if got := Median(tc.in); got != tc.want {
			t.Errorf("Median(%v) = %v, want %v", tc.in, got, tc.want)
		}
	}
}

func TestMedianDoesNotModifyItsInput(t *testing.T) {
	in := []float64{3, 1, 2}
	Median(in)
	if in[0] != 3 || in[1] != 1 || in[2] != 2 {
		t.Fatalf("input was sorted in place: %v", in)
	}
}

func TestRobustSigmaMatchesStdDevOnCleanData(t *testing.T) {
	// The 1.4826 scaling exists to make MAD a consistent estimator of sigma
	// for normal data, so the familiar three-sigma intuition still applies.
	values := []float64{93.6, 94.0, 94.2, 94.4, 94.8, 94.1, 93.9, 94.3, 94.5, 94.0}

	mean := 0.0
	for _, v := range values {
		mean += v
	}
	mean /= float64(len(values))
	variance := 0.0
	for _, v := range values {
		variance += (v - mean) * (v - mean)
	}
	stddev := math.Sqrt(variance / float64(len(values)-1))

	robust := RobustSigma(values)
	if math.Abs(robust-stddev) > 0.15 {
		t.Fatalf("robust sigma %.4f is far from stddev %.4f on clean data",
			robust, stddev)
	}
}

func TestOneBadOutingCannotInflateTheBaseline(t *testing.T) {
	// This is the whole argument for median and MAD over mean and stddev.
	//
	// A single injury outing raises the mean and inflates sigma, so every
	// genuine anomaly afterwards falls inside the widened band -- the one
	// event that most needed flagging is what stops the detector from
	// flagging anything else.
	clean := []float64{94.0, 94.1, 93.9, 94.2, 94.0, 94.1, 93.8, 94.3, 94.0, 94.1}
	contaminated := append([]float64(nil), clean...)
	contaminated[4] = 88.0 // one outing with a dead arm

	cleanMedian, dirtyMedian := Median(clean), Median(contaminated)
	if math.Abs(cleanMedian-dirtyMedian) > 0.1 {
		t.Errorf("median moved from %.3f to %.3f", cleanMedian, dirtyMedian)
	}

	cleanSigma, dirtySigma := RobustSigma(clean), RobustSigma(contaminated)
	if dirtySigma > cleanSigma*2 {
		t.Errorf("robust sigma inflated from %.4f to %.4f", cleanSigma, dirtySigma)
	}

	// For contrast: the mean-based estimator does exactly what we are
	// avoiding.
	meanOf := func(vs []float64) float64 {
		s := 0.0
		for _, v := range vs {
			s += v
		}
		return s / float64(len(vs))
	}
	stdOf := func(vs []float64) float64 {
		m := meanOf(vs)
		s := 0.0
		for _, v := range vs {
			s += (v - m) * (v - m)
		}
		return math.Sqrt(s / float64(len(vs)-1))
	}
	if stdOf(contaminated) < stdOf(clean)*3 {
		t.Log("note: the contrast case did not inflate as expected; " +
			"the fixture may need a more extreme outlier")
	}
}

// --- detection -------------------------------------------------------------

func TestVelocityDropIsFlagged(t *testing.T) {
	d := New(DefaultConfig())
	window := points(94.0, 94.1, 93.9, 94.2, 94.0, 94.1, 93.8, 94.3, 94.0, 94.1)

	f := d.Evaluate(obs(MetricReleaseSpeed, 92.4), window)
	if f == nil {
		t.Fatal("a 1.7 mph drop against a tight baseline should be flagged")
	}
	if f.Direction != DirectionDecrease {
		t.Errorf("direction %s", f.Direction)
	}
	if f.Severity != SeverityHigh {
		t.Errorf("severity %s, want HIGH", f.Severity)
	}
	if f.ZRobust >= 0 {
		t.Errorf("z should be negative for a drop, got %.2f", f.ZRobust)
	}
	// The finding must be self-explaining: a coach reading the row alone
	// should be able to see why it fired.
	if f.BaselineMedian == 0 || f.RobustSigma == 0 {
		t.Error("the finding must carry its baseline")
	}
	if f.DetectorVersion != DetectorVersion {
		t.Error("detector version missing")
	}
}

func TestNormalVariationIsNotFlagged(t *testing.T) {
	// A detector that fires on ordinary noise trains its users to ignore it.
	d := New(DefaultConfig())
	window := points(94.0, 94.1, 93.9, 94.2, 94.0, 94.1, 93.8, 94.3, 94.0, 94.1)

	if f := d.Evaluate(obs(MetricReleaseSpeed, 94.05), window); f != nil {
		t.Fatalf("ordinary variation was flagged: z=%.2f", f.ZRobust)
	}
}

func TestVelocityGainIsAlsoReported(t *testing.T) {
	// Both directions are reported. A sudden velocity gain is a different
	// event from a loss, but it is still worth a coach's attention.
	d := New(DefaultConfig())
	window := points(90.0, 90.1, 89.9, 90.2, 90.0, 90.1, 89.8, 90.3, 90.0, 90.1)

	f := d.Evaluate(obs(MetricReleaseSpeed, 92.0), window)
	if f == nil {
		t.Fatal("a large gain should be reported")
	}
	if f.Direction != DirectionIncrease {
		t.Errorf("direction %s", f.Direction)
	}
}

func TestThinHistoryProducesSilence(t *testing.T) {
	// With three data points there is no baseline to depart from.
	d := New(DefaultConfig())

	if f := d.Evaluate(obs(MetricReleaseSpeed, 88.0), points(94.0, 94.1, 93.9)); f != nil {
		t.Fatal("the detector should stay silent on thin history")
	}
}

func TestThinSessionsAreExcludedFromTheBaseline(t *testing.T) {
	d := New(DefaultConfig())
	window := points(94.0, 94.1, 93.9, 94.2, 94.0, 94.1, 93.8, 94.3, 94.0, 94.1)
	for i := range window {
		window[i].PitchCount = 2 // every outing too thin to have a reliable median
	}

	if f := d.Evaluate(obs(MetricReleaseSpeed, 88.0), window); f != nil {
		t.Fatal("thin outings must not form a baseline")
	}
}

func TestAThinObservationIsNotEvaluated(t *testing.T) {
	d := New(DefaultConfig())
	window := points(94.0, 94.1, 93.9, 94.2, 94.0, 94.1, 93.8, 94.3, 94.0, 94.1)

	o := obs(MetricReleaseSpeed, 88.0)
	o.PitchCount = 3
	if f := d.Evaluate(o, window); f != nil {
		t.Fatal("a three-pitch outing has no reliable median of its own")
	}
}

func TestIdenticalHistoryDoesNotProduceInfiniteZ(t *testing.T) {
	// A pitcher whose last ten outings all read 94.0 has a MAD of zero. The
	// sigma floor is what stops "suspiciously consistent" from becoming
	// "every pitch is an anomaly".
	d := New(DefaultConfig())
	window := points(94.0, 94.0, 94.0, 94.0, 94.0, 94.0, 94.0, 94.0, 94.0, 94.0)

	if f := d.Evaluate(obs(MetricReleaseSpeed, 94.05), window); f != nil {
		t.Fatalf("a rounding-sized change was flagged: z=%.2f", f.ZRobust)
	}

	f := d.Evaluate(obs(MetricReleaseSpeed, 92.0), window)
	if f == nil {
		t.Fatal("a genuine 2 mph drop should still be caught")
	}
	if math.IsInf(f.ZRobust, 0) || math.IsNaN(f.ZRobust) {
		t.Fatalf("z is not finite: %v", f.ZRobust)
	}
	if f.RobustSigma < DefaultConfig().SigmaFloor[MetricReleaseSpeed]-1e-9 {
		t.Errorf("sigma %.4f is below the floor", f.RobustSigma)
	}
}

func TestSeverityBands(t *testing.T) {
	cfg := DefaultConfig()
	d := New(cfg)
	// A baseline with a known robust sigma, so z can be aimed precisely.
	window := points(100, 100, 100, 100, 100, 101, 101, 101, 99, 99)
	sigma := RobustSigma([]float64{100, 100, 100, 100, 100, 101, 101, 101, 99, 99})
	if sigma < cfg.SigmaFloor[MetricReleaseSpeed] {
		sigma = cfg.SigmaFloor[MetricReleaseSpeed]
	}
	median := 100.0

	cases := []struct {
		z    float64
		want any
	}{
		{2.0, nil},
		{2.7, SeverityMedium},
		{3.5, SeverityHigh},
	}
	for _, tc := range cases {
		value := median + tc.z*sigma
		f := d.Evaluate(obs(MetricReleaseSpeed, value), window)
		if tc.want == nil {
			if f != nil {
				t.Errorf("z=%.1f should not fire, got %s", tc.z, f.Severity)
			}
			continue
		}
		if f == nil {
			t.Errorf("z=%.1f should fire", tc.z)
			continue
		}
		if f.Severity != tc.want {
			t.Errorf("z=%.1f: severity %s, want %s", tc.z, f.Severity, tc.want)
		}
	}
}

func TestWindowIsBounded(t *testing.T) {
	// The baseline describes the pitcher as he is now, not his whole career.
	cfg := DefaultConfig()
	cfg.WindowSessions = 5
	d := New(cfg)

	// Twelve outings: the five most recent sit near 90, the older ones at 94.
	window := points(90.0, 90.1, 89.9, 90.2, 90.0, 94.0, 94.1, 93.9, 94.2, 94.0, 94.1, 93.8)

	f := d.Evaluate(obs(MetricReleaseSpeed, 90.05), window)
	if f != nil {
		t.Fatalf("compared against the recent baseline, this is normal: z=%.2f", f.ZRobust)
	}
	if f2 := d.Evaluate(obs(MetricReleaseSpeed, 94.0), window); f2 == nil {
		t.Fatal("94 mph is a large departure from a 90 mph recent baseline")
	}
}

func TestEveryTrackedMetricIsPitcherControlled(t *testing.T) {
	// Outcome metrics are deliberately absent: they are noisy in small
	// samples and contaminated by defence and luck, so a change in them says
	// as much about the fielders as the pitcher.
	forbidden := map[Metric]bool{
		"whiff_rate": true, "era": true, "batting_average": true,
		"launch_speed": true, "xwoba": true,
	}
	for _, m := range AllMetrics {
		if forbidden[m] {
			t.Errorf("%s is an outcome metric and must not be tracked", m)
		}
	}
	if len(AllMetrics) != 7 {
		t.Errorf("expected 7 metrics, got %d", len(AllMetrics))
	}
}

func TestEveryMetricHasASigmaFloor(t *testing.T) {
	// A metric without a floor divides by zero the first time a pitcher is
	// perfectly consistent.
	cfg := DefaultConfig()
	for _, m := range AllMetrics {
		if floor, ok := cfg.SigmaFloor[m]; !ok || floor <= 0 {
			t.Errorf("%s has no sigma floor", m)
		}
	}
}
