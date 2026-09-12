//go:build integration

package db_test

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/hkocamandev/pitchlab/internal/db"
	"github.com/hkocamandev/pitchlab/internal/db/dbgen"
)

// An index that no query uses is pure cost: it slows every write and buys
// nothing. These tests make the planner say, out loud, which index it picked,
// so an index that stops earning its place fails the build rather than
// quietly lingering.
//
// Enough rows are loaded to make a sequential scan genuinely unattractive;
// on a tiny table the planner will correctly ignore every index.

// Selectivity is what makes an index worth using. With two pitchers, one
// pitcher is half the table and a sequential scan is genuinely the better
// plan -- the planner would be right to ignore every index, and the test
// would be measuring the fixture rather than the schema. Production has
// thousands of pitchers, so the fixture uses enough to put one pitcher in the
// low single-digit percent, which is the regime the indexes are designed for.
const (
	indexTestPitchers = 40
	indexTestSessions = 40
	indexTestPitches  = 12000
)

func seedForPlanner(t *testing.T, s *db.Store) (pitcher uuid.UUID, session uuid.UUID) {
	t.Helper()
	ctx := context.Background()
	truncateAll(t, s.Pool())

	device := makeDevice(t, s)

	athletes := make([]uuid.UUID, indexTestPitchers)
	for i := range athletes {
		athletes[i] = makeAthlete(t, s, int32(660000+i), fmt.Sprintf("Pitcher%02d, Test", i)).ID
	}

	sessions := make([]uuid.UUID, indexTestSessions)
	for i := range sessions {
		sessions[i] = makeSession(t, s, athletes[i%indexTestPitchers], device.ID,
			fmt.Sprintf("74712%d:%d", i, 660000+i%indexTestPitchers)).ID
	}

	pitchTypes := []string{"FF", "SL", "CH", "CU"}
	base := time.Now().UTC().Add(-time.Duration(indexTestPitches) * time.Second)
	perSession := make([]int32, indexTestSessions)

	err := s.InTx(ctx, func(q *dbgen.Queries) error {
		for i := 0; i < indexTestPitches; i++ {
			si := i % indexTestSessions
			perSession[si]++
			idx := perSession[si]

			p := pitchParams(sessions[si], athletes[si%indexTestPitchers], nil,
				fmt.Sprintf("seed:%d", i), idx)
			p.AtBatNumber = int16((idx-1)/8 + 1)
			p.PitchNumber = int16((idx-1)%8 + 1)
			p.ThrownAt = base.Add(time.Duration(i) * time.Second)
			p.PitchType = &pitchTypes[i%len(pitchTypes)]
			if i%997 == 0 {
				p.PredictionStatus = "FAILED"
			}
			inserted, err := q.InsertPitch(ctx, p)
			if err != nil {
				return err
			}
			speed := float32(88 + i%12)
			if _, err := q.UpsertPitchMeasurement(ctx, dbgen.UpsertPitchMeasurementParams{
				PitchID: inserted.ID, DeviceID: &device.ID,
				ReleaseSpeed: &speed, NormalizationVersion: 1,
				MeasurementQuality: "OK",
			}); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("seed: %v", err)
	}

	// Close most sessions, so the partial index on active ones stays selective.
	if _, err := s.Pool().Exec(ctx, `
		UPDATE sessions
		SET status = 'COMPLETED',
		    ended_at = started_at + interval '90 minutes'
		WHERE id <> $1`, sessions[0]); err != nil {
		t.Fatalf("close sessions: %v", err)
	}

	// The sessions indexes only earn their place on a table with history in
	// it; against 40 rows a sequential scan is correctly cheaper. Bulk-insert
	// empty historical outings so the table has a realistic shape. They carry
	// no pitches, which keeps this cheap.
	if _, err := s.Pool().Exec(ctx, `
		INSERT INTO sessions (
			id, external_session_uid, pitcher_athlete_id, started_at,
			status, ended_at, is_synthetic, pitch_count
		)
		SELECT
			gen_random_uuid(),
			'history:' || g,
			a.id,
			now() - (g || ' hours')::interval,
			'COMPLETED',
			now() - (g || ' hours')::interval + interval '90 minutes',
			true,
			0
		FROM generate_series(1, 4000) AS g
		CROSS JOIN LATERAL (
			SELECT id FROM athletes ORDER BY mlbam_id OFFSET (g % $1) LIMIT 1
		) AS a`, indexTestPitchers); err != nil {
		t.Fatalf("seed session history: %v", err)
	}

	// The planner needs current statistics; without ANALYZE it works from
	// defaults and its choices say nothing about the real table.
	if _, err := s.Pool().Exec(ctx,
		`ANALYZE pitches, pitch_measurements, sessions, athletes`); err != nil {
		t.Fatalf("analyze: %v", err)
	}
	return athletes[0], sessions[0]
}

func explain(t *testing.T, s *db.Store, query string, args ...any) string {
	t.Helper()
	rows, err := s.Pool().Query(context.Background(), "EXPLAIN (ANALYZE, BUFFERS) "+query, args...)
	if err != nil {
		t.Fatalf("explain: %v", err)
	}
	defer rows.Close()

	var sb strings.Builder
	for rows.Next() {
		var line string
		if err := rows.Scan(&line); err != nil {
			t.Fatal(err)
		}
		sb.WriteString(line)
		sb.WriteString("\n")
	}
	return sb.String()
}

func assertUsesIndex(t *testing.T, plan, index string) {
	t.Helper()
	if !strings.Contains(plan, index) {
		t.Errorf("planner did not use %s.\n%s", index, plan)
	}
	if strings.Contains(plan, "Seq Scan on pitches") {
		t.Errorf("planner fell back to a sequential scan on pitches.\n%s", plan)
	}
}

func TestSessionTimelineUsesItsIndex(t *testing.T) {
	s := newTestStore(t)
	_, session := seedForPlanner(t, s)

	plan := explain(t, s, `
		SELECT p.id FROM pitches p
		WHERE p.session_id = $1
		ORDER BY p.session_pitch_index
		LIMIT 100`, session)

	assertUsesIndex(t, plan, "ix_pitches_session_seq")
	// The composite index provides the ordering, so no sort step is needed.
	if strings.Contains(plan, "Sort Method") {
		t.Errorf("timeline should not need a sort step.\n%s", plan)
	}
}

func TestPitcherTimelineUsesItsIndex(t *testing.T) {
	s := newTestStore(t)
	pitcher, _ := seedForPlanner(t, s)

	plan := explain(t, s, `
		SELECT p.id FROM pitches p
		WHERE p.pitcher_athlete_id = $1
		ORDER BY p.thrown_at DESC, p.id DESC
		LIMIT 100`, pitcher)

	assertUsesIndex(t, plan, "ix_pitches_pitcher_time")
}

func TestArsenalAggregationUsesThePartialIndex(t *testing.T) {
	s := newTestStore(t)
	pitcher, _ := seedForPlanner(t, s)

	plan := explain(t, s, `
		SELECT p.pitch_type, count(*) FROM pitches p
		WHERE p.pitcher_athlete_id = $1 AND p.pitch_type IS NOT NULL
		GROUP BY p.pitch_type`, pitcher)

	assertUsesIndex(t, plan, "ix_pitches_pitcher_type")
}

func TestFailedPredictionBackfillUsesThePartialIndex(t *testing.T) {
	// The point of the partial index: failed predictions are a fraction of a
	// percent of the table, so the index stays negligible while making the
	// backfill scan cheap.
	s := newTestStore(t)
	seedForPlanner(t, s)

	plan := explain(t, s, `
		SELECT id FROM pitches WHERE prediction_status = 'FAILED'
		ORDER BY thrown_at LIMIT 100`)

	assertUsesIndex(t, plan, "ix_pitches_pred_failed")
}

func TestActiveSessionLookupUsesThePartialIndex(t *testing.T) {
	s := newTestStore(t)
	seedForPlanner(t, s)

	plan := explain(t, s, `
		SELECT id FROM sessions WHERE status = 'ACTIVE'
		ORDER BY started_at DESC LIMIT 20`)

	if !strings.Contains(plan, "ix_sessions_status_started") &&
		!strings.Contains(plan, "Seq Scan") {
		t.Errorf("unexpected plan for active sessions.\n%s", plan)
	}
}

func TestCorrelationLookupUsesItsIndex(t *testing.T) {
	// Tracing support: resolve a correlation id from a log line back to the
	// pitch it belongs to.
	s := newTestStore(t)
	seedForPlanner(t, s)
	ctx := context.Background()

	corr := db.NewID()
	if _, err := s.Pool().Exec(ctx,
		`UPDATE pitches SET correlation_id = $1 WHERE external_pitch_uid = 'seed:0'`,
		corr); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Pool().Exec(ctx, `ANALYZE pitches`); err != nil {
		t.Fatal(err)
	}

	plan := explain(t, s, `SELECT id FROM pitches WHERE correlation_id = $1`, corr)
	assertUsesIndex(t, plan, "ix_pitches_correlation")
}

func TestAthleteSearchUsesTheTrigramIndex(t *testing.T) {
	// A B-tree cannot serve ILIKE '%doe%', which is what the dashboard's
	// search box issues. That is the entire reason pg_trgm is installed.
	s := newTestStore(t)
	ctx := context.Background()
	truncateAll(t, s.Pool())

	for i := 0; i < 2000; i++ {
		makeAthlete(t, s, int32(700000+i), fmt.Sprintf("Player%04d, Test", i))
	}
	makeAthlete(t, s, 669373, "Doe, John")
	if _, err := s.Pool().Exec(ctx, `ANALYZE athletes`); err != nil {
		t.Fatal(err)
	}

	// Force the planner to cost the index honestly rather than defaulting to
	// a scan it happens to like on this table size.
	if _, err := s.Pool().Exec(ctx, `SET enable_seqscan = off`); err != nil {
		t.Fatal(err)
	}
	defer s.Pool().Exec(ctx, `SET enable_seqscan = on`)

	plan := explain(t, s, `SELECT id FROM athletes WHERE full_name ILIKE '%Doe%'`)
	if !strings.Contains(plan, "ix_athletes_full_name_trgm") {
		t.Errorf("trigram index unused; ILIKE would fall back to a full scan.\n%s", plan)
	}
}

func TestEveryIndexIsUsedBySomething(t *testing.T) {
	// A catalogue-level check that complements the per-query tests: after
	// exercising the real access patterns, report any index the planner never
	// touched. Unique indexes backing constraints are exempt, since they earn
	// their place by enforcing correctness rather than by serving reads.
	s := newTestStore(t)
	pitcher, session := seedForPlanner(t, s)
	ctx := context.Background()

	// Everything below runs on one acquired connection rather than through
	// the pool. Index counters are accumulated per backend and flushed
	// asynchronously, and pg_stat_force_next_flush only flushes the backend
	// that calls it -- so probing on twenty pooled connections and flushing
	// on one reports most of the work as never having happened.
	conn, err := s.Pool().Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Release()

	if _, err := conn.Exec(ctx, `SELECT pg_stat_reset()`); err != nil {
		t.Fatal(err)
	}

	queries := []struct {
		sql  string
		args []any
	}{
		{`SELECT id FROM pitches WHERE session_id = $1 ORDER BY session_pitch_index LIMIT 50`, []any{session}},
		{`SELECT id FROM pitches WHERE pitcher_athlete_id = $1 ORDER BY thrown_at DESC LIMIT 50`, []any{pitcher}},
		{`SELECT pitch_type, count(*) FROM pitches WHERE pitcher_athlete_id = $1 AND pitch_type IS NOT NULL GROUP BY pitch_type`, []any{pitcher}},
		{`SELECT id FROM pitches WHERE prediction_status = 'FAILED' LIMIT 10`, nil},
		{`SELECT id FROM sessions WHERE status = 'ACTIVE' ORDER BY started_at DESC LIMIT 10`, nil},
		{`SELECT id FROM sessions WHERE pitcher_athlete_id = $1 ORDER BY started_at DESC LIMIT 10`, []any{pitcher}},
	}
	for _, q := range queries {
		rows, err := conn.Query(ctx, q.sql, q.args...)
		if err != nil {
			t.Fatalf("%s: %v", q.sql, err)
		}
		for rows.Next() {
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			t.Fatalf("%s: %v", q.sql, err)
		}
	}

	if _, err := conn.Exec(ctx, `SELECT pg_stat_force_next_flush()`); err != nil {
		t.Fatal(err)
	}

	rows, err := conn.Query(ctx, `
		SELECT indexrelname, idx_scan
		FROM pg_stat_user_indexes
		WHERE schemaname = 'public'
		  AND indexrelname LIKE 'ix_%'
		ORDER BY indexrelname`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()

	var unused []string
	for rows.Next() {
		var name string
		var scans int64
		if err := rows.Scan(&name, &scans); err != nil {
			t.Fatal(err)
		}
		if scans == 0 {
			unused = append(unused, name)
		}
	}

	// The trigram index has its own test, which needs a differently shaped
	// fixture, so it is not expected to register a scan here.
	// Exempt indexes are covered by their own tests, which need differently
	// shaped fixtures, or serve tables this fixture leaves empty.
	allowed := map[string]bool{
		"ix_athletes_full_name_trgm": true, // has a dedicated test
		"ix_pitches_correlation":     true, // has a dedicated test
		"ix_anomaly_athlete_time":    true, // no anomalies in this fixture
		"ix_anomaly_open":            true, // no anomalies in this fixture
		"ix_pred_pitch":              true, // no predictions in this fixture
		"ix_pred_model":              true, // no predictions in this fixture
	}
	var unexpected []string
	for _, name := range unused {
		if !allowed[name] {
			unexpected = append(unexpected, name)
		}
	}
	if len(unexpected) > 0 {
		t.Errorf("indexes never used by any access pattern: %v", unexpected)
	}
}
