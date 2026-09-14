package replay

import (
	"context"
	"encoding/json"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/hkocamandev/pitchlab/internal/events"
)

func f64(v float64) *float64 { return &v }

func tapeLine(atBat, pitchNo, inning int, on1b bool, strikes int) TapeRecord {
	return TapeRecord{
		ExternalGameRef: "747123",
		AtBatNumber:     atBat,
		PitchNumber:     pitchNo,
		SourceGameDate:  "2025-07-14",
		Session: events.SessionRef{
			SessionUID: "747123:669373", PitcherMLBAMID: 669373,
			PThrows: "R", OpponentTeam: "SEA",
		},
		Batter: events.BatterRef{MLBAMID: 665742, Stand: "L"},
		Context: events.PitchContext{
			Balls: 1, Strikes: strikes, OutsWhenUp: 1, Inning: inning,
			InningTopBot: "Top", On1B: on1b,
		},
		Measurement: events.RawMeasurement{
			PitchType: "SL", ReleaseSpeed: f64(87.4), PfxX: f64(0.71),
			PlateX: f64(0.34), PlateZ: f64(1.92),
		},
		GroundTruth: &events.GroundTruth{Outcome: "SWINGING_STRIKE"},
	}
}

func buildTape(n int) *Tape {
	s := Session{
		UID: "747123:669373", PitcherMLBAMID: 669373,
		GameRef: "747123", SourceGameDate: "2025-07-14",
	}
	for i := 1; i <= n; i++ {
		s.Pitches = append(s.Pitches, tapeLine(i, 1, 1, false, 0))
	}
	return &Tape{Sessions: []Session{s}}
}

func collect(t *testing.T, tape *Tape, cfg Config) []*events.Envelope {
	t.Helper()
	var got []*events.Envelope
	sim := New(cfg, EmitterFunc(func(_ context.Context, env *events.Envelope) error {
		got = append(got, env)
		return nil
	}))
	// No waiting: the pacing has its own test, and every other test would
	// otherwise take as long as a ballgame.
	sim.sleep = func(context.Context, time.Duration) error { return nil }

	if _, err := sim.Run(context.Background(), tape); err != nil {
		t.Fatalf("run: %v", err)
	}
	return got
}

// --- the tape --------------------------------------------------------------

func TestTapeGroupsByOuting(t *testing.T) {
	lines := []string{}
	for _, rec := range []struct {
		session string
		atBat   int
	}{
		{"747123:669373", 1}, {"747124:543037", 1},
		{"747123:669373", 2}, {"747124:543037", 2},
	} {
		r := tapeLine(rec.atBat, 1, 1, false, 0)
		r.Session.SessionUID = rec.session
		b, _ := json.Marshal(r)
		lines = append(lines, string(b))
	}

	tape, err := ReadTape(strings.NewReader(strings.Join(lines, "\n")))
	if err != nil {
		t.Fatal(err)
	}
	if len(tape.Sessions) != 2 {
		t.Fatalf("expected 2 outings, got %d", len(tape.Sessions))
	}
	for _, s := range tape.Sessions {
		if len(s.Pitches) != 2 {
			t.Errorf("%s has %d pitches, want 2", s.UID, len(s.Pitches))
		}
	}
}

func TestTapeSortsPitchesWithinAnOuting(t *testing.T) {
	// The exporter writes them in order, but a hand-assembled tape should
	// still replay correctly: out-of-order pitches would silently corrupt the
	// session index downstream.
	var lines []string
	for _, atBat := range []int{3, 1, 2} {
		b, _ := json.Marshal(tapeLine(atBat, 1, 1, false, 0))
		lines = append(lines, string(b))
	}

	tape, err := ReadTape(strings.NewReader(strings.Join(lines, "\n")))
	if err != nil {
		t.Fatal(err)
	}
	got := tape.Sessions[0].Pitches
	for i, want := range []int{1, 2, 3} {
		if got[i].AtBatNumber != want {
			t.Fatalf("position %d holds at-bat %d, want %d", i, got[i].AtBatNumber, want)
		}
	}
}

func TestEmptyTapeIsRejected(t *testing.T) {
	if _, err := ReadTape(strings.NewReader("")); err == nil {
		t.Fatal("an empty tape should be an error, not a silent no-op")
	}
}

// --- timing ----------------------------------------------------------------

func TestTimestampsAreStrictlyIncreasing(t *testing.T) {
	// A session's pitches are consumed in order; a clock that went backwards
	// would make the timeline nonsense even with ordering intact.
	got := collect(t, buildTape(50), DefaultConfig())

	for i := 1; i < len(got); i++ {
		if !got[i].OccurredAt.After(got[i-1].OccurredAt) {
			t.Fatalf("pitch %d is not after pitch %d: %s vs %s",
				i, i-1, got[i].OccurredAt, got[i-1].OccurredAt)
		}
	}
}

func TestSameSeedReplaysIdentically(t *testing.T) {
	// What makes a demo rehearsable and this file's timing assertions
	// possible at all.
	cfg := DefaultConfig()
	first := collect(t, buildTape(30), cfg)
	second := collect(t, buildTape(30), cfg)

	if len(first) != len(second) {
		t.Fatalf("different lengths: %d vs %d", len(first), len(second))
	}
	for i := range first {
		if !first[i].OccurredAt.Equal(second[i].OccurredAt) {
			t.Fatalf("pitch %d differs: %s vs %s",
				i, first[i].OccurredAt, second[i].OccurredAt)
		}
	}
}

func TestDifferentSeedsGiveDifferentTimings(t *testing.T) {
	a := collect(t, buildTape(30), Config{Speed: 0, Seed: 1})
	b := collect(t, buildTape(30), Config{Speed: 0, Seed: 2})

	same := 0
	for i := range a {
		if a[i].OccurredAt.Equal(b[i].OccurredAt) {
			same++
		}
	}
	if same == len(a) {
		t.Fatal("the seed had no effect on the timings")
	}
}

func TestGapsReflectTheGameState(t *testing.T) {
	// Pace is governed by the state, not by a constant: runners slow a
	// pitcher down, a new batter takes time to settle, and a change of sides
	// is an order of magnitude longer.
	clock := NewClock("2025-07-14", 1, 42)

	first := clock.Next(events.PitchContext{Inning: 1}, 1)
	sameAtBat := clock.Next(events.PitchContext{Inning: 1}, 1)
	newAtBat := clock.Next(events.PitchContext{Inning: 1}, 2)
	newInning := clock.Next(events.PitchContext{Inning: 2}, 3)

	withinAtBat := sameAtBat.Sub(first)
	betweenAtBats := newAtBat.Sub(sameAtBat)
	betweenInnings := newInning.Sub(newAtBat)

	if withinAtBat < 15*time.Second || withinAtBat > 25*time.Second {
		t.Errorf("gap within an at-bat is %v; expected roughly 19s", withinAtBat)
	}
	if betweenAtBats <= withinAtBat {
		t.Errorf("a new batter (%v) should take longer than the next pitch (%v)",
			betweenAtBats, withinAtBat)
	}
	if betweenInnings <= betweenAtBats {
		t.Errorf("a change of sides (%v) should take longer than a new batter (%v)",
			betweenInnings, betweenAtBats)
	}
	if betweenInnings < 2*time.Minute {
		t.Errorf("a half-inning break of %v is too short to be a break", betweenInnings)
	}
}

func TestRunnersOnSlowThePaceDown(t *testing.T) {
	// Two clocks with the same seed, so the jitter is identical and the only
	// difference between them is the base interval.
	gapWith := func(ctx events.PitchContext) time.Duration {
		c := NewClock("2025-07-14", 1, 7)
		first := c.Next(ctx, 1)
		return c.Next(ctx, 1).Sub(first)
	}

	basesEmpty := gapWith(events.PitchContext{Inning: 1})
	runnerOn := gapWith(events.PitchContext{Inning: 1, On1B: true})

	if runnerOn <= basesEmpty {
		t.Errorf("runner on: %v, bases empty: %v; the runner should slow it down",
			runnerOn, basesEmpty)
	}
	if diff := runnerOn - basesEmpty; diff != GapRunnersOn-GapBasesEmpty {
		t.Errorf("the difference is %v; with the jitter held equal it should be %v",
			diff, GapRunnersOn-GapBasesEmpty)
	}
}

func TestTwoStrikeCountsSlowThePaceDown(t *testing.T) {
	gapWith := func(strikes int) time.Duration {
		c := NewClock("2025-07-14", 1, 11)
		ctx := events.PitchContext{Inning: 1, Strikes: strikes}
		first := c.Next(ctx, 1)
		return c.Next(ctx, 1).Sub(first)
	}

	if gapWith(2) <= gapWith(0) {
		t.Error("pitchers work more deliberately with the at-bat on the line")
	}
}

func TestPacingHonoursTheSpeedMultiplier(t *testing.T) {
	// The multiplier is the difference between a demo that fits in a meeting
	// and one that takes as long as a ballgame.
	var slept time.Duration
	sim := New(Config{Speed: 100, Seed: 42}, EmitterFunc(
		func(context.Context, *events.Envelope) error { return nil }))
	sim.sleep = func(_ context.Context, d time.Duration) error {
		slept += d
		return nil
	}

	stats, err := sim.Run(context.Background(), buildTape(20))
	if err != nil {
		t.Fatal(err)
	}

	want := time.Duration(float64(stats.SimulatedSpan) / 100)
	if slept < want*9/10 || slept > want*11/10 {
		t.Errorf("slept %v for a simulated span of %v at 100x; expected about %v",
			slept, stats.SimulatedSpan, want)
	}
}

func TestBurstModeDoesNotWait(t *testing.T) {
	sim := New(Config{Speed: 0, Seed: 42}, EmitterFunc(
		func(context.Context, *events.Envelope) error { return nil }))

	slept := false
	sim.sleep = func(context.Context, time.Duration) error {
		slept = true
		return nil
	}
	if _, err := sim.Run(context.Background(), buildTape(20)); err != nil {
		t.Fatal(err)
	}
	if slept {
		t.Fatal("burst mode should not wait at all")
	}
}

func TestCancellationStopsTheReplay(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())

	// A high multiplier so the real sleep is still exercised but the test
	// does not take as long as the innings it is replaying.
	emitted := 0
	sim := New(Config{Speed: 5000, Seed: 42}, EmitterFunc(
		func(context.Context, *events.Envelope) error {
			emitted++
			if emitted == 3 {
				cancel()
			}
			return nil
		}))
	sim.sleep = sleepCtx

	_, err := sim.Run(ctx, buildTape(50))
	if err == nil {
		t.Fatal("expected the replay to stop")
	}
	if emitted > 5 {
		t.Errorf("kept going after cancellation: %d pitches", emitted)
	}
}

// --- the event -------------------------------------------------------------

func TestEventsCarryTheSessionAsTheirKey(t *testing.T) {
	// The partition key is the entire mechanism guaranteeing that an outing
	// is processed in order.
	for _, env := range collect(t, buildTape(10), DefaultConfig()) {
		if env.PartitionKey != "747123:669373" {
			t.Fatalf("partition key %q", env.PartitionKey)
		}
	}
}

func TestIdempotencyKeyIsDerivedFromThePitch(t *testing.T) {
	// A rerun of the same tape must look like a redelivery, not like a new
	// set of pitches. A random key would make the consumer's duplicate
	// detection useless.
	first := collect(t, buildTape(10), DefaultConfig())
	second := collect(t, buildTape(10), DefaultConfig())

	for i := range first {
		if first[i].IdempotencyKey != second[i].IdempotencyKey {
			t.Fatalf("pitch %d got a different key on replay", i)
		}
		if first[i].EventID == second[i].EventID {
			t.Fatalf("pitch %d reused its event id; each emission is a new event", i)
		}
	}
}

func TestEventsAreMarkedSynthetic(t *testing.T) {
	// The flag travels into the session record and out through the API, so
	// nobody mistakes a simulation for live play.
	for _, env := range collect(t, buildTape(5), DefaultConfig()) {
		if !env.Synthetic {
			t.Fatal("a replayed event must be marked synthetic")
		}
	}
}

func TestRealDateAndSyntheticTimeAreBothCarried(t *testing.T) {
	// Keeping them apart is what stops a 2025 pitch from being read as
	// something that just happened.
	env := collect(t, buildTape(1), DefaultConfig())[0]

	var payload events.PitchRaw
	if err := env.Unmarshal(&payload); err != nil {
		t.Fatal(err)
	}
	if payload.SourceGameDate != "2025-07-14" {
		t.Errorf("source date %q", payload.SourceGameDate)
	}
	if payload.ThrownAt.Year() != 2025 {
		t.Errorf("synthetic time %v should sit on the source date", payload.ThrownAt)
	}
	if env.Lag() < 24*time.Hour {
		t.Error("a replayed event should show a large lag between occurring and being produced")
	}
}

func TestMeasurementsAreEmittedUnnormalized(t *testing.T) {
	// Normalization is a domain rule and belongs in the processor, where it
	// is tested against the training code. A camera does not know about
	// handedness conventions.
	env := collect(t, buildTape(1), DefaultConfig())[0]

	raw, err := json.Marshal(env)
	if err != nil {
		t.Fatal(err)
	}
	for _, derived := range []string{
		"pfx_x_arm", "release_pos_x_arm", "plate_x_bat",
		"zone_height_norm", "in_zone", "spin_axis_sin", "spin_axis_cos",
	} {
		if strings.Contains(string(raw), derived) {
			t.Errorf("the event carries the derived field %s", derived)
		}
	}
	if !strings.Contains(string(raw), "pfx_x") {
		t.Error("the event should carry the raw reading")
	}
}

func TestEventCannotCarryAnOutcomeMeasurement(t *testing.T) {
	// A camera cannot know a pitch's exit velocity. The payload has nowhere
	// to put one, which makes the leakage defense structural rather than a
	// matter of remembering.
	env := collect(t, buildTape(1), DefaultConfig())[0]

	raw, err := json.Marshal(env)
	if err != nil {
		t.Fatal(err)
	}
	for _, leaked := range []string{
		"launch_speed", "launch_angle", "hit_distance", "delta_run_exp",
		"estimated_woba", "estimated_ba", "bat_speed", "swing_length",
		"woba_value", "babip_value", "launch_speed_angle",
	} {
		if strings.Contains(string(raw), leaked) {
			t.Errorf("the event carries %s", leaked)
		}
	}

	// Ground truth is allowed, in its own block, because the dashboard shows
	// predictions next to what happened. It reaches one column and no
	// feature vector.
	var payload events.PitchRaw
	if err := env.Unmarshal(&payload); err != nil {
		t.Fatal(err)
	}
	if payload.GroundTruth == nil || payload.GroundTruth.Outcome == "" {
		t.Error("ground truth should travel in its own block")
	}
}

func TestEventsAreValid(t *testing.T) {
	for i, env := range collect(t, buildTape(10), DefaultConfig()) {
		if err := env.Validate(); err != nil {
			t.Fatalf("event %d is invalid: %v", i, err)
		}
	}
}

// --- the constraint --------------------------------------------------------

func TestReplayBinaryCannotReachTheDatabase(t *testing.T) {
	// The rule that keeps the simulator honest, enforced by inspecting the
	// dependency graph rather than by discipline.
	//
	// A real tracking camera does not write to your database; it emits
	// events. If the simulator wrote directly, the Kafka to processor to
	// inference to storage backbone would never execute during a demo, and
	// normalization and idempotency would end up implemented twice and
	// inevitably diverge.
	out, err := exec.Command("go", "list", "-deps",
		"github.com/hkocamandev/pitchlab/cmd/replay").Output()
	if err != nil {
		t.Skipf("could not inspect dependencies: %v", err)
	}

	forbidden := map[string]string{
		"github.com/jackc/pgx":                              "PostgreSQL driver",
		"github.com/hkocamandev/pitchlab/internal/db":       "database layer",
		"github.com/hkocamandev/pitchlab/internal/mlclient": "inference client",
		"github.com/redis/go-redis":                         "Redis client",
	}

	for _, dep := range strings.Split(string(out), "\n") {
		dep = strings.TrimSpace(dep)
		for prefix, what := range forbidden {
			if strings.HasPrefix(dep, prefix) {
				t.Errorf("cmd/replay depends on %s (%s); the simulator must "+
					"only publish events", dep, what)
			}
		}
	}
}
