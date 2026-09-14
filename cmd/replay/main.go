// Command replay turns a tape of historical pitches into a live stream of
// device events.
//
// This is what makes the rest of the system demonstrable. Real tracking
// hardware is not available outside a stadium, so the simulator stands in for
// it -- and stands in faithfully, which is the part that matters:
//
//   - it publishes to Kafka and nothing else. The binary does not import the
//     database, the cache or the inference client, and a test enforces that by
//     inspecting the dependency graph. If it wrote to PostgreSQL directly, the
//     Kafka to processor to inference to storage backbone would never execute
//     during a demo, and normalization and idempotency would end up
//     implemented twice.
//
//   - it emits raw, un-normalized readings. Handedness mirroring and zone
//     normalization are domain rules; a camera does not know them.
//
//   - it cannot emit an outcome measurement, because the event has nowhere to
//     put one. Imitating the device faithfully is what makes the leakage
//     defense structural instead of a matter of remembering.
//
//     go run ./cmd/replay --tape data/processed/replay_tape_2025_8.ndjson --speed 30
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/hkocamandev/pitchlab/internal/events"
	pkafka "github.com/hkocamandev/pitchlab/internal/kafka"
	"github.com/hkocamandev/pitchlab/internal/replay"
)

func main() {
	if err := run(); err != nil {
		if errors.Is(err, context.Canceled) {
			return
		}
		slog.Error("fatal", "error", err)
		os.Exit(1)
	}
}

func run() error {
	var (
		tapePath = flag.String("tape", "", "NDJSON replay tape (required)")
		brokers  = flag.String("brokers", env("KAFKA_BROKERS", "localhost:9094"),
			"comma-separated broker addresses")
		topic = flag.String("topic", events.TopicPitchRaw, "topic to publish to")
		speed = flag.Float64("speed", 30,
			"replay speed multiplier; 1 is real time, 0 is no waiting at all")
		seed = flag.Int64("seed", 42,
			"seed for the synthetic clock; the same tape and seed replay identically")
		sessions = flag.Int("sessions", 0, "replay at most this many outings (0 = all)")
		pitcher  = flag.String("pitcher", "", "replay only this pitcher's MLBAM id")
		game     = flag.String("game", "", "replay only this game")
		dryRun   = flag.Bool("dry-run", false,
			"build the events and report the pacing without publishing")
		version = flag.String("version", env("PITCHLAB_VERSION", "dev"), "version tag")
	)
	flag.Parse()

	if *tapePath == "" {
		flag.Usage()
		return errors.New("--tape is required; generate one with " +
			"ml/scripts/export_replay_tape.py")
	}

	log := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{
		ReplaceAttr: func(_ []string, a slog.Attr) slog.Attr {
			if a.Key == slog.TimeKey {
				if t, ok := a.Value.Any().(time.Time); ok {
					return slog.String("ts", t.UTC().Format(time.RFC3339Nano))
				}
			}
			return a
		},
	})).With("service", "pitchlab-replay", "version", *version)

	tape, err := replay.LoadTape(*tapePath)
	if err != nil {
		return err
	}
	tape = tape.Filter(*sessions, *pitcher, *game)
	if len(tape.Sessions) == 0 {
		return errors.New("no outing matched the filters")
	}

	log.Info("tape loaded",
		"path", *tapePath,
		"sessions", len(tape.Sessions),
		"pitches", tape.PitchCount(),
		"speed", *speed,
		"seed", *seed,
		"dry_run", *dryRun)

	ctx, stop := signal.NotifyContext(context.Background(),
		syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	emitter, closeEmitter, err := buildEmitter(*dryRun, *brokers, *topic, log)
	if err != nil {
		return err
	}
	defer closeEmitter()

	sim := replay.New(replay.Config{
		Speed:    *speed,
		Seed:     *seed,
		Producer: "pitchlab-replay/" + *version,
	}, emitter)

	// Report each outing as it starts and finishes rather than every pitch:
	// at 30x a line per pitch scrolls faster than anyone can read.
	var lastSession string
	sim.OnProgress(func(session string, index, total int, at time.Time) {
		if session != lastSession {
			lastSession = session
			log.Info("outing started", "session", session, "pitches", total)
		}
		if index == total {
			log.Info("outing finished", "session", session, "pitches", total)
		}
	})

	stats, err := sim.Run(ctx, tape)

	log.Info("replay finished",
		"sessions", stats.Sessions,
		"pitches", stats.Pitches,
		"elapsed", stats.Elapsed.Round(time.Millisecond),
		"simulated_span", stats.SimulatedSpan.Round(time.Second),
		// The ratio is the simplest evidence the pacing did what was asked.
		"effective_speed", effectiveSpeed(stats))

	if err != nil && !errors.Is(err, context.Canceled) {
		return err
	}
	return nil
}

func buildEmitter(
	dryRun bool, brokers, topic string, log *slog.Logger,
) (replay.Emitter, func(), error) {
	if dryRun {
		return replay.EmitterFunc(func(_ context.Context, env *events.Envelope) error {
			log.Info("would publish",
				"topic", topic,
				"key", env.PartitionKey,
				"occurred_at", env.OccurredAt.Format(time.RFC3339),
				"correlation_id", env.CorrelationID.String())
			return nil
		}), func() {}, nil
	}

	producer := pkafka.NewProducer(
		pkafka.DefaultProducerConfig(strings.Split(brokers, ",")), log)

	emit := replay.EmitterFunc(func(ctx context.Context, env *events.Envelope) error {
		return producer.Publish(ctx, topic, env)
	})

	return emit, func() {
		if err := producer.Close(); err != nil {
			log.Warn("close producer", "error", err)
		}
	}, nil
}

func effectiveSpeed(s replay.Stats) string {
	if s.Elapsed <= 0 || s.SimulatedSpan <= 0 {
		return "n/a"
	}
	return fmt.Sprintf("%.1fx", s.SimulatedSpan.Seconds()/s.Elapsed.Seconds())
}

func env(key, fallback string) string {
	if v, ok := os.LookupEnv(key); ok && v != "" {
		return v
	}
	return fallback
}
