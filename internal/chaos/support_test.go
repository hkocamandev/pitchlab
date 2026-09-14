//go:build chaos

package chaos

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	kgo "github.com/segmentio/kafka-go"

	"github.com/hkocamandev/pitchlab/internal/cache"
	"github.com/hkocamandev/pitchlab/internal/db"
	"github.com/hkocamandev/pitchlab/internal/events"
	pkafka "github.com/hkocamandev/pitchlab/internal/kafka"
	"github.com/hkocamandev/pitchlab/internal/mlclient"
	"github.com/hkocamandev/pitchlab/internal/processor"
)

// The pipeline is assembled inside the test rather than by starting the
// processor binary. Driving the components directly means the test controls
// when consumption starts and stops, which is the whole point of a scenario
// like "kill the consumer between the work and the commit".

func quietLogger() *slog.Logger {
	return slog.New(slog.NewJSONHandler(io.Discard, nil))
}

func brokers(t *testing.T) []string {
	t.Helper()
	v := os.Getenv("PITCHLAB_TEST_KAFKA_BROKERS")
	if v == "" {
		t.Skip("PITCHLAB_TEST_KAFKA_BROKERS is not set")
	}
	return strings.Split(v, ",")
}

func truncate(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	_, err := pool.Exec(context.Background(), `
		TRUNCATE performance_anomalies, pitch_predictions, pitch_measurements,
		         pitches, sessions, model_versions, devices, athletes
		RESTART IDENTITY CASCADE`)
	if err != nil {
		t.Fatalf("truncate: %v", err)
	}
}

func newProducer(t *testing.T, bs []string) *pkafka.Producer {
	t.Helper()
	p := pkafka.NewProducer(pkafka.DefaultProducerConfig(bs), quietLogger())
	t.Cleanup(func() { _ = p.Close() })
	return p
}

func newPipeline(
	t *testing.T, pool *pgxpool.Pool, producer *pkafka.Producer, mlURL string,
) *processor.Pipeline {
	t.Helper()
	ml := mlclient.New(mlclient.DefaultConfig(mlURL), quietLogger())
	return processor.NewPipeline(db.NewStore(pool), ml, producer, quietLogger(), "chaos")
}

// runConsumer starts a consumer and returns a function that stops it.
func runConsumer(
	t *testing.T, ctx context.Context, bs []string, topic, dlq, group string,
	handle pkafka.Handler,
) func() {
	t.Helper()

	cfg := pkafka.DefaultConsumerConfig(bs, topic, group)
	cfg.Concurrency = 1
	cfg.DLQTopic = dlq

	producer := newProducer(t, bs)
	consumer := pkafka.NewConsumer(cfg, producer, quietLogger())

	runCtx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = consumer.Run(runCtx, handle)
	}()

	stopped := false
	return func() {
		if stopped {
			return
		}
		stopped = true
		cancel()
		select {
		case <-done:
		case <-time.After(20 * time.Second):
			t.Log("consumer did not stop within 20s")
		}
	}
}

func createTopic(t *testing.T, name string, partitions int) {
	t.Helper()

	conn, err := kgo.Dial("tcp", brokers(t)[0])
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer func() { _ = conn.Close() }()

	controller, err := conn.Controller()
	if err != nil {
		t.Fatalf("controller: %v", err)
	}
	ctrl, err := kgo.Dial("tcp", fmt.Sprintf("%s:%d", controller.Host, controller.Port))
	if err != nil {
		t.Fatalf("dial controller: %v", err)
	}
	defer func() { _ = ctrl.Close() }()

	if err := ctrl.CreateTopics(kgo.TopicConfig{
		Topic: name, NumPartitions: partitions, ReplicationFactor: 1,
	}); err != nil {
		t.Fatalf("create topic %s: %v", name, err)
	}
	waitForTopic(t, name)
}

func uniqueTopic(t *testing.T, prefix string, partitions int) string {
	t.Helper()
	name := fmt.Sprintf("%s.%d", prefix, time.Now().UnixNano())
	createTopic(t, name, partitions)
	return name
}

func waitForTopic(t *testing.T, topic string) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)

	for time.Now().Before(deadline) {
		conn, err := kgo.Dial("tcp", brokers(t)[0])
		if err == nil {
			parts, err := conn.ReadPartitions(topic)
			_ = conn.Close()
			if err == nil && len(parts) > 0 && parts[0].Leader.Host != "" {
				return
			}
		}
		time.Sleep(500 * time.Millisecond)
	}
	t.Fatalf("topic %s did not become visible", topic)
}

// countTopic counts the messages currently in a topic.
func countTopic(t *testing.T, bs []string, topic string) int {
	t.Helper()

	reader := kgo.NewReader(kgo.ReaderConfig{
		Brokers: bs, Topic: topic, Partition: 0,
		MinBytes: 1, MaxBytes: 10 << 20,
	})
	defer func() { _ = reader.Close() }()

	if err := reader.SetOffset(kgo.FirstOffset); err != nil {
		t.Fatalf("seek: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	var n int
	for {
		if _, err := reader.ReadMessage(ctx); err != nil {
			return n
		}
		n++
	}
}

func rawPitch(gameRef string, atBat, pitchNo int, sessionUID string) events.PitchRaw {
	f := func(v float64) *float64 { return &v }
	return events.PitchRaw{
		DeviceID: "chaos-sim", DeviceKind: "REPLAY_SIMULATOR",
		ExternalGameRef: gameRef, AtBatNumber: atBat, PitchNumber: pitchNo,
		ThrownAt:       time.Now().UTC().Add(-time.Hour).Truncate(time.Millisecond),
		SourceGameDate: "2025-07-14",
		Session: events.SessionRef{
			SessionUID: sessionUID, PitcherMLBAMID: 669373,
			PitcherName: "Doe, John", PThrows: "R",
		},
		Batter: events.BatterRef{MLBAMID: 665742, Name: "Roe, Alex", Stand: "L"},
		Context: events.PitchContext{
			Balls: 1, Strikes: 2, OutsWhenUp: 1, Inning: 5, InningTopBot: "Top",
		},
		Measurement: events.RawMeasurement{
			PitchType: "SL", PitchName: "Slider",
			ReleaseSpeed: f(87.4), ReleaseSpinRate: f(2540), SpinAxis: f(128),
			ReleasePosX: f(-1.92), ReleasePosZ: f(5.83), ReleaseExtension: f(6.41),
			PfxX: f(0.71), PfxZ: f(0.12), PlateX: f(0.34), PlateZ: f(1.92),
			SzTop: f(3.41), SzBot: f(1.58),
		},
		GroundTruth: &events.GroundTruth{Outcome: "SWINGING_STRIKE"},
	}
}

func envelopeFor(t *testing.T, raw events.PitchRaw) *events.Envelope {
	t.Helper()
	env, err := events.NewEnvelope(
		events.TypePitchRaw, "pitchlab-chaos/1",
		events.IdempotencyKeyFor(raw.PitchUID()),
		raw.Session.SessionUID, raw.ThrownAt, raw)
	if err != nil {
		t.Fatalf("envelope: %v", err)
	}
	env.Synthetic = true
	return env
}

// publishPitches publishes pitches [from, to] of one outing.
func publishPitches(
	t *testing.T, producer *pkafka.Producer, topic, gameRef string, from, to int,
) {
	t.Helper()
	ctx := context.Background()

	for i := from; i <= to; i++ {
		raw := rawPitch(gameRef, i, 1, gameRef+":669373")
		if err := producer.Publish(ctx, topic, envelopeFor(t, raw)); err != nil {
			t.Fatalf("publish %d: %v", i, err)
		}
	}
}

func newCache(t *testing.T) *cache.Cache {
	t.Helper()

	url := os.Getenv("PITCHLAB_TEST_REDIS_URL")
	if url == "" {
		t.Skip("PITCHLAB_TEST_REDIS_URL is not set")
	}

	c, err := cache.New(cache.DefaultConfig(url), quietLogger())
	if err != nil {
		t.Fatalf("cache: %v", err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c
}
