// Command seed registers the baseline rows the pipeline expects to exist:
// the replay simulator as a measurement device, the trained model versions
// whose cards live under models/, and the first user account -- without which
// a freshly provisioned database has no way in.
//
// It is idempotent, so running it against an already-seeded database is a
// no-op rather than an error.
//
//	go run ./cmd/seed
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"time"

	"github.com/hkocamandev/pitchlab/internal/auth"
	"github.com/hkocamandev/pitchlab/internal/db"
	"github.com/hkocamandev/pitchlab/internal/db/dbgen"
)

// modelCard mirrors the JSON the training pipeline writes. Only the fields
// the database needs are decoded; the rest of the card stays in the file.
type modelCard struct {
	Version        string          `json:"version"`
	Variant        string          `json:"variant"`
	Target         string          `json:"target"`
	Algorithm      string          `json:"algorithm"`
	FeatureColumns []string        `json:"feature_columns"`
	ClassLabels    []string        `json:"class_labels"`
	RunValueTable  json.RawMessage `json:"run_value_table"`
	ScoreScaler    struct {
		Mu    float32 `json:"mu"`
		Sigma float32 `json:"sigma"`
	} `json:"score_scaler"`
	Metrics json.RawMessage `json:"metrics"`
	GitSHA  string          `json:"git_sha"`
}

func main() {
	var (
		dsn       = flag.String("database-url", os.Getenv("DATABASE_URL"), "PostgreSQL connection string")
		modelsDir = flag.String("models", "models", "directory holding model artifact folders")
		snapshot  = flag.String("snapshot", "statcast_full_20260911.parquet", "data snapshot the models were trained on")

		adminUser = flag.String("admin-user", env("PITCHLAB_ADMIN_USER", "admin"),
			"username of the initial administrator")
		adminPass = flag.String("admin-password", os.Getenv("PITCHLAB_ADMIN_PASSWORD"),
			"password for the initial administrator; generated and printed once if empty")
		skipAdmin = flag.Bool("skip-admin", false, "do not touch the users table")
	)
	flag.Parse()

	log := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))

	if *dsn == "" {
		*dsn = "postgres://pitchlab:pitchlab@localhost:5432/pitchlab?sslmode=disable"
	}

	ctx := context.Background()
	pool, err := db.NewPool(ctx, db.DefaultPoolConfig(*dsn))
	if err != nil {
		log.Error("connect", "error", err)
		os.Exit(1)
	}
	defer pool.Close()

	store := db.NewStore(pool)

	if err := seedDevice(ctx, store, log); err != nil {
		log.Error("seed device", "error", err)
		os.Exit(1)
	}
	if err := seedModels(ctx, store, log, *modelsDir, *snapshot); err != nil {
		log.Error("seed models", "error", err)
		os.Exit(1)
	}
	if !*skipAdmin {
		if err := seedAdmin(ctx, store, log, *adminUser, *adminPass); err != nil {
			log.Error("seed admin", "error", err)
			os.Exit(1)
		}
	}

	log.Info("seed complete")
}

func seedDevice(ctx context.Context, store *db.Store, log *slog.Logger) error {
	// The simulator registers as a device so that replayed rows are
	// distinguishable from real ones in the database, not merely by
	// convention. The calibration profile records which measurement regime
	// the replayed data comes from.
	profile, _ := json.Marshal(map[string]any{
		"plate_measurement_point": "front",
		"zone_source":             "operator",
		"regime":                  "2023-2025",
	})

	device, err := store.UpsertDevice(ctx, dbgen.UpsertDeviceParams{
		ID:                 db.NewID(),
		DeviceKey:          "replay-sim-01",
		DeviceKind:         string(deviceKindReplay),
		DisplayName:        strPtr("Replay Simulator"),
		CalibrationProfile: profile,
	})
	if err != nil {
		return err
	}
	log.Info("device ready", "device_key", device.DeviceKey, "id", device.ID)
	return nil
}

const deviceKindReplay = "REPLAY_SIMULATOR"

func seedModels(ctx context.Context, store *db.Store, log *slog.Logger, dir, snapshot string) error {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			log.Warn("no models directory; skipping model seed", "dir", dir)
			return nil
		}
		return err
	}

	seeded := 0
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		path := filepath.Join(dir, e.Name(), "model_card.json")
		raw, err := os.ReadFile(path)
		if err != nil {
			log.Warn("skipping directory without a model card", "dir", e.Name())
			continue
		}

		var card modelCard
		if err := json.Unmarshal(raw, &card); err != nil {
			return fmt.Errorf("%s: %w", path, err)
		}

		featureCols, err := json.Marshal(card.FeatureColumns)
		if err != nil {
			return err
		}

		params := dbgen.InsertModelVersionParams{
			ID:               db.NewID(),
			Version:          card.Version,
			Variant:          card.Variant,
			Target:           card.Target,
			Algorithm:        card.Algorithm,
			FeatureColumns:   featureCols,
			ScoreMu:          card.ScoreScaler.Mu,
			ScoreSigma:       card.ScoreScaler.Sigma,
			TrainPeriodStart: time.Date(2023, 3, 30, 0, 0, 0, 0, time.UTC),
			TrainPeriodEnd:   time.Date(2024, 9, 29, 0, 0, 0, 0, time.UTC),
			ValPeriodStart:   timePtr(time.Date(2025, 3, 18, 0, 0, 0, 0, time.UTC)),
			ValPeriodEnd:     timePtr(time.Date(2025, 6, 30, 0, 0, 0, 0, time.UTC)),
			TestPeriodStart:  timePtr(time.Date(2025, 7, 1, 0, 0, 0, 0, time.UTC)),
			TestPeriodEnd:    timePtr(time.Date(2025, 9, 28, 0, 0, 0, 0, time.UTC)),
			DataSnapshot:     snapshot,
			Metrics:          orEmptyJSON(card.Metrics),
		}
		if card.GitSHA != "" {
			params.GitSha = strPtr(card.GitSHA)
		}
		// Only the 5-class model carries the artifacts that make its score
		// reproducible; a CHECK constraint enforces the pairing.
		if card.Target == "outcome_5class" {
			labels, err := json.Marshal(card.ClassLabels)
			if err != nil {
				return err
			}
			params.ClassLabels = labels
			params.RunValueTable = orEmptyJSON(card.RunValueTable)
		}

		mv, err := store.InsertModelVersion(ctx, params)
		if err != nil {
			return fmt.Errorf("insert %s/%s: %w", card.Version, card.Variant, err)
		}
		if _, err := store.ActivateModelVersion(ctx, mv.ID); err != nil {
			return fmt.Errorf("activate %s/%s: %w", card.Version, card.Variant, err)
		}

		log.Info("model version active",
			"version", mv.Version, "variant", mv.Variant, "target", mv.Target,
			"features", len(card.FeatureColumns))
		seeded++
	}

	if seeded == 0 {
		log.Warn("no model cards found", "dir", dir)
	}
	return nil
}

// seedAdmin creates or rotates the initial administrator.
//
// Two things here are deliberate and worth stating.
//
// First, there is no default password. A well-known one ships to production
// far more often than anybody expects, and "admin/admin" behind a login page
// is worse than no login page, because it looks protected. With no password
// supplied a random one is generated and printed once; it exists nowhere else,
// so losing it means running this command again.
//
// Second, re-running rotates the password rather than failing. That makes this
// the recovery path for a lost credential, which is otherwise a database
// surgery job.
func seedAdmin(
	ctx context.Context, store *db.Store, log *slog.Logger,
	username, password string,
) error {
	generated := password == ""
	if generated {
		var err error
		if password, err = auth.GenerateSecret(); err != nil {
			return fmt.Errorf("generate password: %w", err)
		}
	}

	hash, err := auth.HashPassword(password)
	if err != nil {
		return fmt.Errorf("hash password: %w", err)
	}

	user, err := store.UpsertUser(ctx, dbgen.UpsertUserParams{
		ID:           db.NewID(),
		Username:     username,
		DisplayName:  "Administrator",
		PasswordHash: hash,
		Role:         "admin",
	})
	if err != nil {
		return err
	}

	log.Info("admin ready", "username", user.Username, "id", user.ID)

	if generated {
		// Written to stdout, not to the log. A password in a structured log
		// line is a password in the log aggregator, retained for as long as
		// the retention policy says -- which is exactly where it must not be.
		fmt.Printf("\n  generated password for %q: %s\n"+
			"  this is shown once; store it now\n\n", user.Username, password)
	}
	return nil
}

func env(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func strPtr(s string) *string        { return &s }
func timePtr(t time.Time) *time.Time { return &t }

func orEmptyJSON(raw json.RawMessage) []byte {
	if len(raw) == 0 {
		return []byte(`{}`)
	}
	return raw
}
