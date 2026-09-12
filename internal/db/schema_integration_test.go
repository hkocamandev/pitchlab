//go:build integration

package db_test

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hkocamandev/pitchlab/internal/db"
	"github.com/hkocamandev/pitchlab/internal/db/dbgen"
)

// --------------------------------------------------------------------------
// Idempotency
//
// The pipeline's correctness rests on these. Kafka delivers at least once, so
// every one of these paths will be exercised in production.
// --------------------------------------------------------------------------

func TestInsertPitchIsIdempotent(t *testing.T) {
	s := newTestStore(t)
	truncateAll(t, s.Pool())
	ctx := context.Background()

	pitcher := makeAthlete(t, s, 669373, "Doe, John")
	batter := makeAthlete(t, s, 665742, "Roe, Alex")
	device := makeDevice(t, s)
	sess := makeSession(t, s, pitcher.ID, device.ID, "747123:669373")

	params := pitchParams(sess.ID, pitcher.ID, &batter.ID, "747123:41:3", 1)

	first, err := s.InsertPitch(ctx, params)
	if err != nil {
		t.Fatalf("first insert: %v", err)
	}

	// Same pitch, fresh row id, as a redelivery would produce.
	retry := params
	retry.ID = db.NewID()
	_, err = s.InsertPitch(ctx, retry)
	if !db.IsNoRows(err) {
		t.Fatalf("second insert should report no rows, got %v", err)
	}

	var n int
	if err := s.Pool().QueryRow(ctx, `SELECT count(*) FROM pitches`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("expected 1 pitch after redelivery, got %d", n)
	}

	stored, err := s.GetPitchByExternalUID(ctx, "747123:41:3")
	if err != nil {
		t.Fatal(err)
	}
	if stored.ID != first.ID {
		t.Fatal("redelivery replaced the original row instead of being ignored")
	}
}

func TestInsertPitchIsIdempotentUnderConcurrency(t *testing.T) {
	// The reason idempotency lives in a database constraint rather than in a
	// select-then-insert: two processor instances can race, and only the
	// database can resolve that atomically.
	s := newTestStore(t)
	truncateAll(t, s.Pool())
	ctx := context.Background()

	pitcher := makeAthlete(t, s, 669373, "Doe, John")
	device := makeDevice(t, s)
	sess := makeSession(t, s, pitcher.ID, device.ID, "747123:669373")

	const writers = 16
	var wg sync.WaitGroup
	inserted := make(chan bool, writers)

	for i := 0; i < writers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			p := pitchParams(sess.ID, pitcher.ID, nil, "747123:41:3", 1)
			_, err := s.InsertPitch(ctx, p)
			switch {
			case err == nil:
				inserted <- true
			case db.IsNoRows(err):
				inserted <- false
			default:
				t.Errorf("unexpected error: %v", err)
			}
		}()
	}
	wg.Wait()
	close(inserted)

	wins := 0
	for ok := range inserted {
		if ok {
			wins++
		}
	}
	if wins != 1 {
		t.Fatalf("exactly one writer should win, got %d", wins)
	}

	var n int
	if err := s.Pool().QueryRow(ctx, `SELECT count(*) FROM pitches`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("expected 1 row after %d concurrent writers, got %d", writers, n)
	}
}

func TestUpsertAthleteConvergesOnOneRow(t *testing.T) {
	s := newTestStore(t)
	truncateAll(t, s.Pool())
	ctx := context.Background()

	first := makeAthlete(t, s, 669373, "Doe, John")
	again, err := s.UpsertAthlete(ctx, dbgen.UpsertAthleteParams{
		ID: db.NewID(), MlbamID: 669373, FullName: "Doe, Johnathan",
		Throws: ptr("R"), PrimaryRole: "PITCHER",
	})
	if err != nil {
		t.Fatal(err)
	}
	if again.ID != first.ID {
		t.Fatal("upsert created a second athlete instead of converging")
	}
	if again.FullName != "Doe, Johnathan" {
		t.Fatalf("name should refresh, got %q", again.FullName)
	}
}

func TestUpsertAthletePromotesToTwoWay(t *testing.T) {
	// Roles are relationships, not identities: the same person can pitch and
	// bat, which is exactly why Pitcher and Batter are not separate tables.
	s := newTestStore(t)
	truncateAll(t, s.Pool())
	ctx := context.Background()

	makeAthlete(t, s, 660271, "Two Way")
	updated, err := s.UpsertAthlete(ctx, dbgen.UpsertAthleteParams{
		ID: db.NewID(), MlbamID: 660271, FullName: "Two Way",
		PrimaryRole: "BATTER",
	})
	if err != nil {
		t.Fatal(err)
	}
	if updated.PrimaryRole != "TWO_WAY" {
		t.Fatalf("expected TWO_WAY, got %q", updated.PrimaryRole)
	}
}

func TestAnomalyIdempotencyHandlesNullPitchType(t *testing.T) {
	// NULL <> NULL in a plain unique constraint, so the rows with no pitch
	// type are precisely the ones a naive constraint would fail to protect.
	// The index uses COALESCE(pitch_type, '') for that reason.
	s := newTestStore(t)
	truncateAll(t, s.Pool())
	ctx := context.Background()

	pitcher := makeAthlete(t, s, 669373, "Doe, John")
	device := makeDevice(t, s)
	sess := makeSession(t, s, pitcher.ID, device.ID, "747123:669373")

	params := dbgen.InsertAnomalyParams{
		ID: db.NewID(), AthleteID: pitcher.ID, SessionID: sess.ID,
		Metric: "release_speed", PitchType: nil,
		ObservedValue: 92.4, BaselineMedian: 94.2, RobustSigma: 0.53,
		ZRobust: -3.4, Direction: "DECREASE", Severity: "HIGH",
		WindowSessions: 10, DetectorVersion: "robust-mad-v1",
	}

	if _, err := s.InsertAnomaly(ctx, params); err != nil {
		t.Fatalf("first insert: %v", err)
	}
	retry := params
	retry.ID = db.NewID()
	if _, err := s.InsertAnomaly(ctx, retry); !db.IsNoRows(err) {
		t.Fatalf("duplicate with NULL pitch_type should be rejected, got %v", err)
	}

	var n int
	if err := s.Pool().QueryRow(ctx,
		`SELECT count(*) FROM performance_anomalies`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("expected 1 anomaly, got %d", n)
	}
}

// --------------------------------------------------------------------------
// Constraints
//
// Device data is not trusted. A fault must be stopped at write time, not
// discovered later in a training set.
// --------------------------------------------------------------------------

func TestPitchRulebookConstraints(t *testing.T) {
	s := newTestStore(t)
	truncateAll(t, s.Pool())
	ctx := context.Background()

	pitcher := makeAthlete(t, s, 669373, "Doe, John")
	device := makeDevice(t, s)
	sess := makeSession(t, s, pitcher.ID, device.ID, "747123:669373")

	cases := []struct {
		name   string
		mutate func(*dbgen.InsertPitchParams)
		want   string
	}{
		{"four balls is a walk, not a count", func(p *dbgen.InsertPitchParams) { p.Balls = 4 }, "ck_pitches_balls"},
		{"three strikes is a strikeout", func(p *dbgen.InsertPitchParams) { p.Strikes = 3 }, "ck_pitches_strikes"},
		{"three outs ends the inning", func(p *dbgen.InsertPitchParams) { p.OutsWhenUp = 3 }, "ck_pitches_outs"},
		{"inning is one-based", func(p *dbgen.InsertPitchParams) { p.Inning = 0 }, "ck_pitches_inning"},
		{"handedness is L or R", func(p *dbgen.InsertPitchParams) { p.Stand = "X" }, "ck_pitches_stand"},
		{"outcome must match the class set", func(p *dbgen.InsertPitchParams) {
			p.ActualOutcome = ptr("swinging_strike")
		}, "ck_pitches_outcome"},
	}

	for i, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := pitchParams(sess.ID, pitcher.ID, nil, "bad:"+tc.want, int32(i+1))
			p.AtBatNumber = int16(i + 1)
			tc.mutate(&p)
			_, err := s.InsertPitch(ctx, p)
			if err == nil {
				t.Fatal("expected the database to reject this row")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("expected %s, got %v", tc.want, err)
			}
		})
	}
}

func TestMeasurementBoundsRejectSensorFaults(t *testing.T) {
	s := newTestStore(t)
	truncateAll(t, s.Pool())
	ctx := context.Background()

	pitcher := makeAthlete(t, s, 669373, "Doe, John")
	device := makeDevice(t, s)
	sess := makeSession(t, s, pitcher.ID, device.ID, "747123:669373")
	pitch, err := s.InsertPitch(ctx, pitchParams(sess.ID, pitcher.ID, nil, "747123:1:1", 1))
	if err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		name   string
		mutate func(*dbgen.UpsertPitchMeasurementParams)
		want   string
	}{
		{"250 mph is a sensor error", func(m *dbgen.UpsertPitchMeasurementParams) {
			m.ReleaseSpeed = ptr(float32(250))
		}, "ck_meas_speed"},
		{"spin axis is a bearing", func(m *dbgen.UpsertPitchMeasurementParams) {
			m.SpinAxis = ptr(float32(400))
		}, "ck_meas_axis"},
		{"nobody releases 20 feet out", func(m *dbgen.UpsertPitchMeasurementParams) {
			m.ReleaseExtension = ptr(float32(20))
		}, "ck_meas_ext"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m := dbgen.UpsertPitchMeasurementParams{
				PitchID: pitch.ID, DeviceID: &device.ID,
				ReleaseSpeed: ptr(float32(87.4)), SpinAxis: ptr(float32(128)),
				ReleaseExtension: ptr(float32(6.4)),
				NormalizationVersion: 1, MeasurementQuality: "OK",
			}
			tc.mutate(&m)
			if _, err := s.UpsertPitchMeasurement(ctx, m); err == nil ||
				!strings.Contains(err.Error(), tc.want) {
				t.Fatalf("expected %s, got %v", tc.want, err)
			}
		})
	}
}

func TestPredictionOutputShapeIsExclusive(t *testing.T) {
	// A prediction is either a class distribution or a whiff probability.
	// Both or neither means a bug upstream, and the constraint says so.
	s := newTestStore(t)
	truncateAll(t, s.Pool())
	ctx := context.Background()

	pitcher := makeAthlete(t, s, 669373, "Doe, John")
	device := makeDevice(t, s)
	sess := makeSession(t, s, pitcher.ID, device.ID, "747123:669373")
	pitch, err := s.InsertPitch(ctx, pitchParams(sess.ID, pitcher.ID, nil, "747123:1:1", 1))
	if err != nil {
		t.Fatal(err)
	}
	mv := makeModelVersion(t, s, "pitch-outcome-v1.0.0", "pitching", "outcome_5class")

	bad := dbgen.UpsertPitchPredictionParams{
		ID: db.NewID(), PitchID: pitch.ID, ModelVersionID: mv.ID,
		Probabilities:    []byte(`{"BALL":0.2}`),
		PredictedClass:   ptr("BALL"),
		WhiffProbability: ptr(float32(0.4)), // both shapes at once
		ScoredQuantity:   -0.02, Score: 110,
	}
	if _, err := s.UpsertPitchPrediction(ctx, bad); err == nil ||
		!strings.Contains(err.Error(), "ck_pred_output_shape") {
		t.Fatalf("expected ck_pred_output_shape, got %v", err)
	}
}

// --------------------------------------------------------------------------
// Model versioning
// --------------------------------------------------------------------------

func makeModelVersion(t *testing.T, s *db.Store, version, variant, target string) dbgen.ModelVersion {
	t.Helper()
	p := dbgen.InsertModelVersionParams{
		ID: db.NewID(), Version: version, Variant: variant, Target: target,
		Algorithm:      "lightgbm",
		FeatureColumns: []byte(`["release_speed"]`),
		ScoreMu:        -0.001, ScoreSigma: 0.04,
		TrainPeriodStart: time.Date(2023, 3, 30, 0, 0, 0, 0, time.UTC),
		TrainPeriodEnd:   time.Date(2024, 9, 29, 0, 0, 0, 0, time.UTC),
		DataSnapshot:     "statcast_full_20260911.parquet",
		Metrics:          []byte(`{}`),
	}
	if target == "outcome_5class" {
		p.ClassLabels = []byte(`["BALL","CALLED_STRIKE","SWINGING_STRIKE","FOUL","IN_PLAY"]`)
		p.RunValueTable = []byte(`{"values":{}}`)
	}
	mv, err := s.InsertModelVersion(context.Background(), p)
	if err != nil {
		t.Fatalf("insert model version: %v", err)
	}
	return mv
}

func TestOutcomeModelMustCarryItsScoringArtifacts(t *testing.T) {
	// Without the run-value table and class labels, a stored score cannot be
	// traced back to the probabilities that produced it.
	s := newTestStore(t)
	truncateAll(t, s.Pool())

	_, err := s.InsertModelVersion(context.Background(), dbgen.InsertModelVersionParams{
		ID: db.NewID(), Version: "broken-v1", Variant: "pitching",
		Target: "outcome_5class", Algorithm: "lightgbm",
		FeatureColumns: []byte(`[]`), ScoreMu: 0, ScoreSigma: 1,
		TrainPeriodStart: time.Now(), TrainPeriodEnd: time.Now(),
		DataSnapshot: "x", Metrics: []byte(`{}`),
		// ClassLabels and RunValueTable deliberately omitted.
	})
	if err == nil || !strings.Contains(err.Error(), "ck_model_outcome_artifacts") {
		t.Fatalf("expected ck_model_outcome_artifacts, got %v", err)
	}
}

func TestOnlyOneModelVersionCanBeActivePerVariant(t *testing.T) {
	s := newTestStore(t)
	truncateAll(t, s.Pool())
	ctx := context.Background()

	v1 := makeModelVersion(t, s, "v1.0.0", "pitching", "outcome_5class")
	v2 := makeModelVersion(t, s, "v1.1.0", "pitching", "outcome_5class")

	if _, err := s.ActivateModelVersion(ctx, v1.ID); err != nil {
		t.Fatalf("activate v1: %v", err)
	}
	if _, err := s.ActivateModelVersion(ctx, v2.ID); err != nil {
		t.Fatalf("activate v2: %v", err)
	}

	active, err := s.GetActiveModelVersion(ctx, "pitching")
	if err != nil {
		t.Fatal(err)
	}
	if active.ID != v2.ID {
		t.Fatal("activation did not move to the newer version")
	}

	var n int
	if err := s.Pool().QueryRow(ctx,
		`SELECT count(*) FROM model_versions WHERE variant='pitching' AND is_active`,
	).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("expected exactly 1 active version, got %d", n)
	}
}

func TestModelVersionCannotBeDeletedWhilePredictionsReferenceIt(t *testing.T) {
	s := newTestStore(t)
	truncateAll(t, s.Pool())
	ctx := context.Background()

	pitcher := makeAthlete(t, s, 669373, "Doe, John")
	device := makeDevice(t, s)
	sess := makeSession(t, s, pitcher.ID, device.ID, "747123:669373")
	pitch, err := s.InsertPitch(ctx, pitchParams(sess.ID, pitcher.ID, nil, "747123:1:1", 1))
	if err != nil {
		t.Fatal(err)
	}
	mv := makeModelVersion(t, s, "v1.0.0", "stuff", "whiff_given_swing")

	if _, err := s.UpsertPitchPrediction(ctx, dbgen.UpsertPitchPredictionParams{
		ID: db.NewID(), PitchID: pitch.ID, ModelVersionID: mv.ID,
		WhiffProbability: ptr(float32(0.41)),
		ScoredQuantity:   -0.41, Score: 112.1,
	}); err != nil {
		t.Fatal(err)
	}

	_, err = s.Pool().Exec(ctx, `DELETE FROM model_versions WHERE id = $1`, mv.ID)
	if err == nil {
		t.Fatal("deleting a referenced model version must be refused")
	}
}

// --------------------------------------------------------------------------
// Cascades
// --------------------------------------------------------------------------

func TestDeletingASessionRemovesItsPitchesButNotItsAthlete(t *testing.T) {
	s := newTestStore(t)
	truncateAll(t, s.Pool())
	ctx := context.Background()

	pitcher := makeAthlete(t, s, 669373, "Doe, John")
	device := makeDevice(t, s)
	sess := makeSession(t, s, pitcher.ID, device.ID, "747123:669373")
	pitch, err := s.InsertPitch(ctx, pitchParams(sess.ID, pitcher.ID, nil, "747123:1:1", 1))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.UpsertPitchMeasurement(ctx, dbgen.UpsertPitchMeasurementParams{
		PitchID: pitch.ID, DeviceID: &device.ID,
		ReleaseSpeed: ptr(float32(87.4)), NormalizationVersion: 1,
		MeasurementQuality: "OK",
	}); err != nil {
		t.Fatal(err)
	}

	if _, err := s.Pool().Exec(ctx, `DELETE FROM sessions WHERE id = $1`, sess.ID); err != nil {
		t.Fatalf("session delete should cascade: %v", err)
	}

	for _, q := range []string{
		`SELECT count(*) FROM pitches`,
		`SELECT count(*) FROM pitch_measurements`,
	} {
		var n int
		if err := s.Pool().QueryRow(ctx, q).Scan(&n); err != nil {
			t.Fatal(err)
		}
		if n != 0 {
			t.Fatalf("%s: expected cascade to clear rows, got %d", q, n)
		}
	}

	// The athlete outlives the session: RESTRICT, not CASCADE.
	if _, err := s.GetAthlete(ctx, pitcher.ID); err != nil {
		t.Fatalf("athlete should survive session deletion: %v", err)
	}
}

// --------------------------------------------------------------------------
// Transactions
// --------------------------------------------------------------------------

func TestInTxRollsBackEverythingOnError(t *testing.T) {
	// The processor writes athlete, session, pitch, measurement and
	// predictions as one unit. A partial write would leave a pitch with no
	// measurement, which downstream code has no way to interpret.
	s := newTestStore(t)
	truncateAll(t, s.Pool())
	ctx := context.Background()

	pitcher := makeAthlete(t, s, 669373, "Doe, John")
	device := makeDevice(t, s)
	sess := makeSession(t, s, pitcher.ID, device.ID, "747123:669373")

	wantErr := context.Canceled
	err := s.InTx(ctx, func(q *dbgen.Queries) error {
		if _, err := q.InsertPitch(ctx, pitchParams(sess.ID, pitcher.ID, nil, "747123:1:1", 1)); err != nil {
			return err
		}
		return wantErr
	})
	if err != wantErr {
		t.Fatalf("expected the callback error to surface, got %v", err)
	}

	var n int
	if err := s.Pool().QueryRow(ctx, `SELECT count(*) FROM pitches`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("rollback left %d rows behind", n)
	}
}

// --------------------------------------------------------------------------
// Session state machine
// --------------------------------------------------------------------------

func TestCloseSessionIsIdempotentAndReportsIt(t *testing.T) {
	s := newTestStore(t)
	truncateAll(t, s.Pool())
	ctx := context.Background()

	pitcher := makeAthlete(t, s, 669373, "Doe, John")
	device := makeDevice(t, s)
	sess := makeSession(t, s, pitcher.ID, device.ID, "747123:669373")

	closed, err := s.CloseSession(ctx, sess.ID)
	if err != nil {
		t.Fatalf("close: %v", err)
	}
	if closed.Status != "COMPLETED" || closed.EndedAt == nil {
		t.Fatalf("close did not complete the session: %+v", closed)
	}

	// A second close returns no row, which is how the handler tells
	// "already closed" apart from "not found".
	if _, err := s.CloseSession(ctx, sess.ID); !db.IsNoRows(err) {
		t.Fatalf("second close should report no rows, got %v", err)
	}
}

func TestRecomputeSessionAggregatesConvergesOnTruth(t *testing.T) {
	s := newTestStore(t)
	truncateAll(t, s.Pool())
	ctx := context.Background()

	pitcher := makeAthlete(t, s, 669373, "Doe, John")
	device := makeDevice(t, s)
	sess := makeSession(t, s, pitcher.ID, device.ID, "747123:669373")

	speeds := []float32{95.0, 93.0, 97.0}
	for i, sp := range speeds {
		p, err := s.InsertPitch(ctx, pitchParams(
			sess.ID, pitcher.ID, nil, "747123:"+string(rune('1'+i))+":1", int32(i+1)))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := s.UpsertPitchMeasurement(ctx, dbgen.UpsertPitchMeasurementParams{
			PitchID: p.ID, DeviceID: &device.ID, ReleaseSpeed: &sp,
			NormalizationVersion: 1, MeasurementQuality: "OK",
		}); err != nil {
			t.Fatal(err)
		}
	}

	updated, err := s.RecomputeSessionAggregates(ctx, sess.ID)
	if err != nil {
		t.Fatal(err)
	}
	if updated.PitchCount != 3 {
		t.Fatalf("expected 3 pitches, got %d", updated.PitchCount)
	}
	if updated.AvgReleaseSpeed == nil || *updated.AvgReleaseSpeed < 94.9 || *updated.AvgReleaseSpeed > 95.1 {
		t.Fatalf("expected mean 95.0, got %v", updated.AvgReleaseSpeed)
	}

	// Recomputing from source rather than incrementing means a repeated call
	// converges instead of drifting.
	again, err := s.RecomputeSessionAggregates(ctx, sess.ID)
	if err != nil {
		t.Fatal(err)
	}
	if again.PitchCount != 3 {
		t.Fatalf("recompute is not idempotent: got %d", again.PitchCount)
	}
}
