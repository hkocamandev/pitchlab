//go:build integration

package db_test

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/hkocamandev/pitchlab/internal/db"
	"github.com/hkocamandev/pitchlab/internal/db/dbgen"
)

// These tests run against a real PostgreSQL instance, because what they check
// is the database's behaviour -- unique constraints under concurrency, CHECK
// enforcement, partial indexes, planner choices. A mock would only assert
// that the test author remembered the schema correctly.
//
//	make up && make migrate-up && make test-integration

func newTestStore(t *testing.T) *db.Store {
	t.Helper()

	url := os.Getenv("PITCHLAB_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("PITCHLAB_TEST_DATABASE_URL is not set")
	}

	ctx := context.Background()
	pool, err := db.NewPool(ctx, db.DefaultPoolConfig(url))
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(pool.Close)
	return db.NewStore(pool)
}

// truncateAll resets the schema between tests. CASCADE follows the foreign
// keys, and RESTART IDENTITY is harmless here since all keys are UUIDs.
func truncateAll(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	_, err := pool.Exec(context.Background(), `
		TRUNCATE performance_anomalies, pitch_predictions, pitch_measurements,
		         pitches, sessions, model_versions, devices, athletes
		RESTART IDENTITY CASCADE`)
	if err != nil {
		t.Fatalf("truncate: %v", err)
	}
}

func ptr[T any](v T) *T { return &v }

// --- fixtures --------------------------------------------------------------

func makeAthlete(t *testing.T, s *db.Store, mlbamID int32, name string) dbgen.Athlete {
	t.Helper()
	a, err := s.UpsertAthlete(context.Background(), dbgen.UpsertAthleteParams{
		ID:          db.NewID(),
		MlbamID:     mlbamID,
		FullName:    name,
		Throws:      ptr("R"),
		Bats:        ptr("L"),
		PrimaryRole: "PITCHER",
	})
	if err != nil {
		t.Fatalf("upsert athlete: %v", err)
	}
	return a
}

func makeDevice(t *testing.T, s *db.Store) dbgen.Device {
	t.Helper()
	d, err := s.UpsertDevice(context.Background(), dbgen.UpsertDeviceParams{
		ID:                 db.NewID(),
		DeviceKey:          "replay-sim-test",
		DeviceKind:         "REPLAY_SIMULATOR",
		DisplayName:        ptr("Replay Simulator (test)"),
		CalibrationProfile: []byte(`{"regime":"2023-2025"}`),
	})
	if err != nil {
		t.Fatalf("upsert device: %v", err)
	}
	return d
}

func makeSession(t *testing.T, s *db.Store, pitcher uuid.UUID, device uuid.UUID, uid string) dbgen.Session {
	t.Helper()
	sess, err := s.UpsertSession(context.Background(), dbgen.UpsertSessionParams{
		ID:                 db.NewID(),
		ExternalSessionUid: uid,
		PitcherAthleteID:   pitcher,
		DeviceID:           &device,
		ExternalGameRef:    ptr("747123"),
		StartedAt:          time.Now().UTC().Add(-2 * time.Hour).Truncate(time.Second),
		Status:             "ACTIVE",
		IsSynthetic:        true,
	})
	if err != nil {
		t.Fatalf("upsert session: %v", err)
	}
	return sess
}

// pitchParams builds a valid pitch. Tests override individual fields to probe
// a single constraint at a time.
func pitchParams(sessionID, pitcher uuid.UUID, batter *uuid.UUID, uid string, idx int32) dbgen.InsertPitchParams {
	return dbgen.InsertPitchParams{
		ID:                db.NewID(),
		ExternalPitchUid:  uid,
		SessionID:         sessionID,
		PitcherAthleteID:  pitcher,
		BatterAthleteID:   batter,
		AtBatNumber:       int16(idx),
		PitchNumber:       1,
		SessionPitchIndex: idx,
		ThrownAt:          time.Now().UTC().Truncate(time.Millisecond),
		Balls:             1,
		Strikes:           2,
		OutsWhenUp:        1,
		Inning:            5,
		InningTopbot:      ptr("Top"),
		Stand:             "L",
		PThrows:           "R",
		On1b:              true,
		BatScore:          ptr(int16(2)),
		FldScore:          ptr(int16(3)),
		PitchType:         ptr("SL"),
		PitchName:         ptr("Slider"),
		ActualOutcome:     ptr("SWINGING_STRIKE"),
		PredictionStatus:  "PENDING",
	}
}
