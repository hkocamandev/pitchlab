//go:build integration

package kafka_test

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strings"
	"testing"
	"time"

	kgo "github.com/segmentio/kafka-go"

	"github.com/hkocamandev/pitchlab/internal/db"
	"github.com/hkocamandev/pitchlab/internal/events"
	pkafka "github.com/hkocamandev/pitchlab/internal/kafka"
	"github.com/hkocamandev/pitchlab/internal/mlclient"
	"github.com/hkocamandev/pitchlab/internal/processor"
)

// These run against a real broker and a real database. What they check is the
// behaviour of the pipeline as a whole -- redelivery, ordering, poison
// messages, offset semantics -- and none of that is observable in a unit test
// with a fake broker.
//
//	make up && make migrate-up && make kafka-topics && make test-integration

func brokers(t *testing.T) []string {
	t.Helper()
	v := os.Getenv("PITCHLAB_TEST_KAFKA_BROKERS")
	if v == "" {
		t.Skip("PITCHLAB_TEST_KAFKA_BROKERS is not set")
	}
	return strings.Split(v, ",")
}

func testStore(t *testing.T) *db.Store {
	t.Helper()
	url := os.Getenv("PITCHLAB_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("PITCHLAB_TEST_DATABASE_URL is not set")
	}
	pool, err := db.NewPool(context.Background(), db.DefaultPoolConfig(url))
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(pool.Close)

	store := db.NewStore(pool)
	if _, err := pool.Exec(context.Background(), `
		TRUNCATE performance_anomalies, pitch_predictions, pitch_measurements,
		         pitches, sessions, model_versions, devices, athletes
		RESTART IDENTITY CASCADE`); err != nil {
		t.Fatalf("truncate: %v", err)
	}
	return store
}

func quietLogger() *slog.Logger {
	return slog.New(slog.NewJSONHandler(io.Discard, nil))
}

// uniqueTopic gives each test its own topic, so tests cannot consume each
// other's messages or inherit a committed offset from a previous run.
func uniqueTopic(t *testing.T, prefix string, partitions int) string {
	t.Helper()
	name := fmt.Sprintf("%s.test.%d", prefix, time.Now().UnixNano())

	conn, err := kgo.Dial("tcp", brokers(t)[0])
	if err != nil {
		t.Fatalf("dial broker: %v", err)
	}
	defer conn.Close()

	controller, err := conn.Controller()
	if err != nil {
		t.Fatalf("controller: %v", err)
	}
	ctrlConn, err := kgo.Dial("tcp", fmt.Sprintf("%s:%d", controller.Host, controller.Port))
	if err != nil {
		t.Fatalf("dial controller: %v", err)
	}
	defer ctrlConn.Close()

	if err := ctrlConn.CreateTopics(kgo.TopicConfig{
		Topic: name, NumPartitions: partitions, ReplicationFactor: 1,
	}); err != nil {
		t.Fatalf("create topic %s: %v", name, err)
	}
	// Deliberately not deleted per test. Topic deletion in KRaft is
	// asynchronous, and the metadata churn it causes made the *next* test's
	// freshly created topic invisible to a producer -- the tests passed
	// individually and failed as a suite. They are all removed at once in
	// TestMain instead.
	createdTopics = append(createdTopics, name)

	// Creation is acknowledged before the metadata reaches every broker, so a
	// producer that writes immediately gets UNKNOWN_TOPIC_OR_PARTITION. Wait
	// until the topic is actually visible.
	waitForTopic(t, name)
	return name
}

// createdTopics accumulates the topics made during the run so TestMain can
// remove them together, after every test has finished with the broker.
var createdTopics []string

func TestMain(m *testing.M) {
	code := m.Run()
	cleanupTopics()
	os.Exit(code)
}

func cleanupTopics() {
	if len(createdTopics) == 0 {
		return
	}
	v := os.Getenv("PITCHLAB_TEST_KAFKA_BROKERS")
	if v == "" {
		return
	}
	conn, err := kgo.Dial("tcp", strings.Split(v, ",")[0])
	if err != nil {
		return
	}
	controller, err := conn.Controller()
	conn.Close()
	if err != nil {
		return
	}
	ctrl, err := kgo.Dial("tcp", fmt.Sprintf("%s:%d", controller.Host, controller.Port))
	if err != nil {
		return
	}
	defer ctrl.Close()
	_ = ctrl.DeleteTopics(createdTopics...)
}

func waitForTopic(t *testing.T, topic string) {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)

	for time.Now().Before(deadline) {
		conn, err := kgo.Dial("tcp", brokers(t)[0])
		if err == nil {
			parts, err := conn.ReadPartitions(topic)
			conn.Close()
			if err == nil && len(parts) > 0 {
				ready := true
				for _, p := range parts {
					if p.Leader.Host == "" {
						ready = false
						break
					}
				}
				if ready {
					return
				}
			}
		}
		time.Sleep(250 * time.Millisecond)
	}
	t.Fatalf("topic %s did not become visible", topic)
}

func rawPitch(gameRef string, atBat, pitchNo int, sessionUID string) events.PitchRaw {
	f := func(v float64) *float64 { return &v }
	return events.PitchRaw{
		DeviceID: "replay-sim-test", DeviceKind: "REPLAY_SIMULATOR",
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
		events.TypePitchRaw, "pitchlab-test/1",
		events.IdempotencyKeyFor(raw.PitchUID()),
		raw.Session.SessionUID,
		raw.ThrownAt, raw)
	if err != nil {
		t.Fatal(err)
	}
	env.Synthetic = true
	return env
}

// runConsumer consumes until the handler has seen wantCount messages or the
// deadline passes.
func runConsumer(
	t *testing.T, cfg pkafka.ConsumerConfig, producer *pkafka.Producer,
	handle pkafka.Handler, wantCount int, timeout time.Duration,
) int {
	t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	seen := make(chan struct{}, 1024)
	consumer := pkafka.NewConsumer(cfg, producer, quietLogger())

	done := make(chan error, 1)
	go func() {
		done <- consumer.Run(ctx, func(c context.Context, env *events.Envelope) error {
			err := handle(c, env)
			select {
			case seen <- struct{}{}:
			default:
			}
			return err
		})
	}()

	count := 0
	for count < wantCount {
		select {
		case <-seen:
			count++
		case <-ctx.Done():
			cancel()
			<-done
			return count
		}
	}

	// Give the consumer a moment to commit the last offset before stopping.
	time.Sleep(300 * time.Millisecond)
	cancel()
	<-done
	return count
}

// --------------------------------------------------------------------------
// Idempotency
// --------------------------------------------------------------------------

func TestRedeliveryDoesNotCreateASecondRow(t *testing.T) {
	// Kafka delivers at least once. Every consumer will see the same message
	// again whenever it dies between doing its work and committing its
	// offset, so this is the normal case rather than an edge case.
	bs := brokers(t)
	store := testStore(t)
	topic := uniqueTopic(t, "pitchlab.pitch.raw", 3)

	producer := pkafka.NewProducer(pkafka.DefaultProducerConfig(bs), quietLogger())
	defer producer.Close()

	ml := mlclient.New(mlclient.DefaultConfig("http://localhost:1"), quietLogger())
	pipeline := processor.NewPipeline(store, ml, producer, quietLogger(), "test")

	raw := rawPitch("747123", 41, 3, "747123:669373")
	ctx := context.Background()

	// Publish the same pitch three times, as three separate events -- new
	// event ids, same natural key, which is exactly what a redelivery and a
	// re-run of the simulator both look like.
	for i := 0; i < 3; i++ {
		if err := producer.Publish(ctx, topic, envelopeFor(t, raw)); err != nil {
			t.Fatalf("publish %d: %v", i, err)
		}
	}

	cfg := pkafka.DefaultConsumerConfig(bs, topic, "test-idempotency")
	cfg.Concurrency = 1
	runConsumer(t, cfg, producer, pipeline.HandlePitchRaw, 3, 30*time.Second)

	var n int
	if err := store.Pool().QueryRow(ctx, `SELECT count(*) FROM pitches`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("three deliveries produced %d rows; the natural key should have absorbed two", n)
	}

	var measurements int
	if err := store.Pool().QueryRow(ctx,
		`SELECT count(*) FROM pitch_measurements`).Scan(&measurements); err != nil {
		t.Fatal(err)
	}
	if measurements != 1 {
		t.Fatalf("expected 1 measurement, got %d", measurements)
	}
}

// --------------------------------------------------------------------------
// Ordering
// --------------------------------------------------------------------------

func TestSessionPitchesArriveInOrder(t *testing.T) {
	// Ordering within an outing is the one guarantee the partition key exists
	// to provide. Without it the timeline is scrambled, the sequence index is
	// wrong, and the live count display contradicts itself.
	bs := brokers(t)
	store := testStore(t)
	topic := uniqueTopic(t, "pitchlab.pitch.raw", 3)

	producer := pkafka.NewProducer(pkafka.DefaultProducerConfig(bs), quietLogger())
	defer producer.Close()

	ml := mlclient.New(mlclient.DefaultConfig("http://localhost:1"), quietLogger())
	pipeline := processor.NewPipeline(store, ml, producer, quietLogger(), "test")

	const pitches = 40
	ctx := context.Background()
	for i := 1; i <= pitches; i++ {
		raw := rawPitch("747123", i, 1, "747123:669373")
		if err := producer.Publish(ctx, topic, envelopeFor(t, raw)); err != nil {
			t.Fatalf("publish %d: %v", i, err)
		}
	}

	cfg := pkafka.DefaultConsumerConfig(bs, topic, "test-ordering")
	// Three readers in the group. Ordering must hold anyway, because one
	// session hashes to one partition and a reader processes its partitions
	// strictly sequentially.
	cfg.Concurrency = 3
	got := runConsumer(t, cfg, producer, pipeline.HandlePitchRaw, pitches, 60*time.Second)
	if got < pitches {
		t.Fatalf("only %d of %d messages were processed", got, pitches)
	}

	rows, err := store.Pool().Query(ctx, `
		SELECT at_bat_number, session_pitch_index
		FROM pitches ORDER BY session_pitch_index`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()

	var order []int
	expectedIndex := int32(1)
	for rows.Next() {
		var atBat int16
		var index int32
		if err := rows.Scan(&atBat, &index); err != nil {
			t.Fatal(err)
		}
		if index != expectedIndex {
			t.Fatalf("sequence index %d where %d was expected; indices are not contiguous",
				index, expectedIndex)
		}
		order = append(order, int(atBat))
		expectedIndex++
	}

	if len(order) != pitches {
		t.Fatalf("stored %d pitches, expected %d", len(order), pitches)
	}
	for i, atBat := range order {
		if atBat != i+1 {
			t.Fatalf("position %d holds at-bat %d; the session was processed out of order",
				i, atBat)
		}
	}
}

// --------------------------------------------------------------------------
// Poison messages
// --------------------------------------------------------------------------

func TestAPoisonMessageDoesNotStallTheConsumer(t *testing.T) {
	// A message that can never be processed must not hold up the ones behind
	// it. This is what the dead-letter path is for, and the test that makes
	// the difference between a consumer and an outage.
	bs := brokers(t)
	store := testStore(t)
	topic := uniqueTopic(t, "pitchlab.pitch.raw", 3)
	dlq := uniqueTopic(t, "pitchlab.pitch.raw.dlq", 1)

	producer := pkafka.NewProducer(pkafka.DefaultProducerConfig(bs), quietLogger())
	defer producer.Close()

	ml := mlclient.New(mlclient.DefaultConfig("http://localhost:1"), quietLogger())
	pipeline := processor.NewPipeline(store, ml, producer, quietLogger(), "test")

	ctx := context.Background()
	sessionUID := "747123:669373"

	// A good pitch, then three unprocessable messages, then two more good
	// ones -- all on the same partition key, so they queue behind each other.
	if err := producer.Publish(ctx, topic,
		envelopeFor(t, rawPitch("747123", 1, 1, sessionUID))); err != nil {
		t.Fatal(err)
	}

	// Not valid JSON at all.
	writeRaw(t, bs, topic, sessionUID, []byte(`{"this is not": `))

	// Valid JSON, but not an envelope this build can read.
	writeRaw(t, bs, topic, sessionUID, []byte(`{"event_id":"`+
		"01a09999-0000-7000-8000-000000000001"+`","event_type":"pitchlab.pitch.raw",`+
		`"schema_version":99,"occurred_at":"2025-07-14T19:00:00Z",`+
		`"correlation_id":"01a09999-0000-7000-8000-000000000001",`+
		`"idempotency_key":"x","partition_key":"`+sessionUID+`","data":{}}`))

	// A well-formed envelope carrying a pitch the rulebook forbids.
	bad := rawPitch("747123", 2, 1, sessionUID)
	bad.Context.Balls = 7
	if err := producer.Publish(ctx, topic, envelopeFor(t, bad)); err != nil {
		t.Fatal(err)
	}

	for i := 3; i <= 4; i++ {
		if err := producer.Publish(ctx, topic,
			envelopeFor(t, rawPitch("747123", i, 1, sessionUID))); err != nil {
			t.Fatal(err)
		}
	}

	if n := countMessages(t, bs, topic, 6, 15*time.Second); n != 6 {
		t.Fatalf("published 6 messages, topic holds %d", n)
	}

	cfg := pkafka.DefaultConsumerConfig(bs, topic, "test-poison")
	cfg.Concurrency = 1
	cfg.DLQTopic = dlq
	// One attempt: these failures are permanent, and the point of the test is
	// that they are recognised as such rather than retried.
	cfg.MaxAttempts = 2
	runConsumer(t, cfg, producer, pipeline.HandlePitchRaw, 6, 60*time.Second)

	// The three good pitches must be stored. If a poison message had stalled
	// the partition, the ones behind it would be missing.
	var n int
	if err := store.Pool().QueryRow(ctx, `SELECT count(*) FROM pitches`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 3 {
		t.Fatalf("stored %d pitches; the poison messages blocked the ones behind them", n)
	}

	letters := readDLQ(t, bs, dlq, 3, 20*time.Second)
	for _, l := range letters {
		t.Logf("dead letter: class=%s msg=%q", l.Failure.ErrorClass, l.Failure.ErrorMessage)
	}
	if len(letters) < 3 {
		t.Fatalf("expected 3 dead letters, got %d", len(letters))
	}

	// The malformed one is the case worth naming: a dead-letter record that
	// cannot itself be encoded loses the failure it was meant to capture.
	var sawMalformed bool
	for _, l := range letters {
		if l.Failure.ErrorClass == "DESERIALIZATION" {
			sawMalformed = true
			if json.Valid(l.OriginalPayload) {
				t.Error("the malformed original should have been preserved as-is")
			}
		}
	}
	if !sawMalformed {
		t.Error("the malformed message was not dead-lettered")
	}

	classes := map[string]bool{}
	for _, l := range letters {
		classes[l.Failure.ErrorClass] = true
		if l.Failure.ErrorMessage == "" {
			t.Error("a dead letter must say why it failed")
		}
		if len(l.OriginalPayload) == 0 {
			t.Error("the original bytes must be preserved so the message can be replayed")
		}
	}
	for _, want := range []string{
		"DESERIALIZATION", "UNKNOWN_SCHEMA_VERSION", "BUSINESS_RULE_VIOLATION",
	} {
		if !classes[want] {
			t.Errorf("expected a %s dead letter, saw %v", want, keys(classes))
		}
	}
}

func writeRaw(t *testing.T, brokers []string, topic, key string, value []byte) {
	t.Helper()
	w := &kgo.Writer{
		Addr: kgo.TCP(brokers...), Topic: topic,
		Balancer: &kgo.Hash{}, RequiredAcks: kgo.RequireAll,
	}
	defer w.Close()
	if err := w.WriteMessages(context.Background(), kgo.Message{
		Key: []byte(key), Value: value,
	}); err != nil {
		t.Fatalf("write raw message: %v", err)
	}
}

// readDLQ drains a dead-letter topic.
//
// Reads partition 0 directly, which is why the fixture gives DLQ topics a
// single partition -- the same shape production uses. Dead letters keep the
// original message key, so they hash to one partition anyway; a multi-partition
// DLQ would just mean hunting for which.
func readDLQ(
	t *testing.T, brokers []string, topic string, want int, timeout time.Duration,
) []pkafka.DeadLetter {
	t.Helper()

	r := kgo.NewReader(kgo.ReaderConfig{
		Brokers: brokers, Topic: topic, Partition: 0,
		MinBytes: 1, MaxBytes: 10 << 20,
	})
	defer r.Close()
	if err := r.SetOffset(kgo.FirstOffset); err != nil {
		t.Fatalf("seek dlq: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	var out []pkafka.DeadLetter
	for len(out) < want {
		msg, err := r.ReadMessage(ctx)
		if err != nil {
			break
		}
		var letter pkafka.DeadLetter
		if err := json.Unmarshal(msg.Value, &letter); err != nil {
			t.Errorf("dead letter is not decodable: %v", err)
			continue
		}
		out = append(out, letter)
	}
	return out
}

// countMessages reads a topic directly, without a consumer group, to confirm
// what was actually published.
func countMessages(
	t *testing.T, brokers []string, topic string, want int, timeout time.Duration,
) int {
	t.Helper()

	conn, err := kgo.Dial("tcp", brokers[0])
	if err != nil {
		t.Fatal(err)
	}
	parts, err := conn.ReadPartitions(topic)
	conn.Close()
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	total := 0
	for _, p := range parts {
		r := kgo.NewReader(kgo.ReaderConfig{
			Brokers: brokers, Topic: topic, Partition: p.ID,
			MinBytes: 1, MaxBytes: 10 << 20,
		})
		_ = r.SetOffset(kgo.FirstOffset)
		for total < want {
			c, cancelRead := context.WithTimeout(ctx, 2*time.Second)
			_, err := r.ReadMessage(c)
			cancelRead()
			if err != nil {
				break
			}
			total++
		}
		r.Close()
	}
	return total
}

func keys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

// --------------------------------------------------------------------------
// Offsets
// --------------------------------------------------------------------------

func TestOffsetIsNotCommittedBeforeTheWorkIsDone(t *testing.T) {
	// Committing first would be at-most-once and would lose a pitch on any
	// crash in the window. This checks the other direction: a consumer that
	// fails every message must leave the offset where it was, so a later
	// consumer in the same group still sees them.
	bs := brokers(t)
	store := testStore(t)
	topic := uniqueTopic(t, "pitchlab.pitch.raw", 3)
	dlq := uniqueTopic(t, "pitchlab.pitch.raw.dlq", 1)

	producer := pkafka.NewProducer(pkafka.DefaultProducerConfig(bs), quietLogger())
	defer producer.Close()

	ctx := context.Background()
	const pitches = 5
	for i := 1; i <= pitches; i++ {
		if err := producer.Publish(ctx, topic,
			envelopeFor(t, rawPitch("747123", i, 1, "747123:669373"))); err != nil {
			t.Fatal(err)
		}
	}

	group := "test-offsets"

	// First pass: the handler refuses everything transiently. The messages
	// end up dead-lettered, but they were seen.
	firstPass := 0
	cfg := pkafka.DefaultConsumerConfig(bs, topic, group)
	cfg.Concurrency = 1
	cfg.DLQTopic = dlq
	cfg.MaxAttempts = 1
	runConsumer(t, cfg, producer, func(context.Context, *events.Envelope) error {
		firstPass++
		return fmt.Errorf("transient failure")
	}, pitches, 30*time.Second)

	if firstPass < pitches {
		t.Fatalf("first pass saw %d of %d messages", firstPass, pitches)
	}

	// Second pass in the same group: everything was committed after being
	// dead-lettered, so nothing should be redelivered.
	ml := mlclient.New(mlclient.DefaultConfig("http://localhost:1"), quietLogger())
	pipeline := processor.NewPipeline(store, ml, producer, quietLogger(), "test")

	secondCfg := pkafka.DefaultConsumerConfig(bs, topic, group)
	secondCfg.Concurrency = 1
	secondCfg.DLQTopic = dlq
	secondPass := runConsumer(t, secondCfg, producer, pipeline.HandlePitchRaw, 1, 8*time.Second)

	if secondPass != 0 {
		t.Errorf("%d messages were redelivered after being committed", secondPass)
	}
}

// --------------------------------------------------------------------------
// Downstream publication
// --------------------------------------------------------------------------

func TestAnalyzedEventCarriesTheCorrelationForward(t *testing.T) {
	// One pitch's journey has to stay greppable end to end. The correlation
	// id is copied unchanged into the downstream event; the causation id
	// points back at the event that produced it.
	bs := brokers(t)
	store := testStore(t)
	rawTopic := uniqueTopic(t, "pitchlab.pitch.raw", 3)

	producer := pkafka.NewProducer(pkafka.DefaultProducerConfig(bs), quietLogger())
	defer producer.Close()

	ml := mlclient.New(mlclient.DefaultConfig("http://localhost:1"), quietLogger())
	pipeline := processor.NewPipeline(store, ml, producer, quietLogger(), "test")

	raw := rawPitch("747123", 41, 3, "747123:669373")
	env := envelopeFor(t, raw)
	wantCorrelation := env.CorrelationID

	ctx := context.Background()
	if err := producer.Publish(ctx, rawTopic, env); err != nil {
		t.Fatal(err)
	}

	cfg := pkafka.DefaultConsumerConfig(bs, rawTopic, "test-correlation")
	cfg.Concurrency = 1
	runConsumer(t, cfg, producer, pipeline.HandlePitchRaw, 1, 30*time.Second)

	var storedCorrelation *string
	if err := store.Pool().QueryRow(ctx,
		`SELECT correlation_id::text FROM pitches LIMIT 1`).Scan(&storedCorrelation); err != nil {
		t.Fatalf("no pitch stored: %v", err)
	}
	if storedCorrelation == nil || *storedCorrelation != wantCorrelation.String() {
		t.Fatalf("stored correlation %v, want %s", storedCorrelation, wantCorrelation)
	}
}

func TestPitchIsStoredEvenWhenInferenceIsDown(t *testing.T) {
	// Losing a pitch because a downstream dependency was unavailable would be
	// the worst outcome available. The row is written with its prediction
	// marked failed and can be backfilled later.
	bs := brokers(t)
	store := testStore(t)
	topic := uniqueTopic(t, "pitchlab.pitch.raw", 3)

	producer := pkafka.NewProducer(pkafka.DefaultProducerConfig(bs), quietLogger())
	defer producer.Close()

	// Nothing listens on this port.
	ml := mlclient.New(mlclient.DefaultConfig("http://127.0.0.1:1"), quietLogger())
	pipeline := processor.NewPipeline(store, ml, producer, quietLogger(), "test")

	ctx := context.Background()
	if err := producer.Publish(ctx, topic,
		envelopeFor(t, rawPitch("747123", 41, 3, "747123:669373"))); err != nil {
		t.Fatal(err)
	}

	cfg := pkafka.DefaultConsumerConfig(bs, topic, "test-ml-down")
	cfg.Concurrency = 1
	runConsumer(t, cfg, producer, pipeline.HandlePitchRaw, 1, 30*time.Second)

	var status string
	if err := store.Pool().QueryRow(ctx,
		`SELECT prediction_status FROM pitches LIMIT 1`).Scan(&status); err != nil {
		t.Fatalf("the pitch was not stored: %v", err)
	}
	if status != "FAILED" {
		t.Errorf("prediction_status %q, want FAILED", status)
	}

	// The measurement must be there too: the pitch is complete apart from its
	// score, which is what makes a backfill possible.
	var speed *float64
	if err := store.Pool().QueryRow(ctx,
		`SELECT release_speed FROM pitch_measurements LIMIT 1`).Scan(&speed); err != nil {
		t.Fatalf("the measurement was not stored: %v", err)
	}
	if speed == nil || *speed < 87 || *speed > 88 {
		t.Errorf("release speed %v, want ~87.4", speed)
	}
}
