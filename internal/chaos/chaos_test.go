//go:build chaos

// Package chaos breaks the system on purpose.
//
// These tests stop real containers while real traffic is flowing, which is why
// they sit behind their own build tag: they take minutes, they disturb shared
// infrastructure, and running them next to the unit suite would make the unit
// suite unreliable for reasons that have nothing to do with the code under
// test.
//
//	make test-chaos
//
// What they are for is the claims the design makes and the ordinary tests
// cannot check. "Redis is not critical", "the processor recovers", "no pitch is
// lost when a consumer dies mid-flight" are statements about behaviour under
// failure, and the only honest way to check them is to cause the failure.
package chaos

import (
	"context"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/hkocamandev/pitchlab/internal/db"
	"github.com/hkocamandev/pitchlab/internal/events"
	pkafka "github.com/hkocamandev/pitchlab/internal/kafka"
)

// --- container control -----------------------------------------------------

// compose runs a docker compose command against the project stack.
func compose(t *testing.T, args ...string) {
	t.Helper()

	cmd := exec.Command("docker", append([]string{"compose"}, args...)...)
	cmd.Dir = repoRoot(t)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("docker compose %s: %v\n%s", strings.Join(args, " "), err, out)
	}
}

func repoRoot(t *testing.T) string {
	t.Helper()
	wd, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	return wd + "/../.."
}

// stopService stops a container and guarantees it comes back.
//
// Registered as a cleanup before the stop, not after: if the test fails
// between the two, the container still restarts. A chaos test that leaves the
// stack broken has cost more than it found.
func stopService(t *testing.T, service string) {
	t.Helper()
	t.Cleanup(func() {
		compose(t, "start", service)
		waitHealthy(t, service, 60*time.Second)
	})
	compose(t, "stop", service)
}

func waitHealthy(t *testing.T, service string, within time.Duration) {
	t.Helper()

	container := "pitchlab-" + service
	deadline := time.Now().Add(within)

	for time.Now().Before(deadline) {
		out, err := exec.Command("docker", "inspect",
			"-f", "{{.State.Health.Status}}", container).Output()
		if err == nil && strings.TrimSpace(string(out)) == "healthy" {
			return
		}
		time.Sleep(time.Second)
	}
	t.Fatalf("%s did not become healthy within %s", container, within)
}

// --- helpers ---------------------------------------------------------------

func databaseURL(t *testing.T) string {
	t.Helper()
	url := os.Getenv("PITCHLAB_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("PITCHLAB_TEST_DATABASE_URL is not set")
	}
	return url
}

func openPool(t *testing.T, url string) *pgxpool.Pool {
	t.Helper()

	cfg := db.DefaultPoolConfig(url)
	cfg.ConnectTimeout = 3 * time.Second

	pool, err := db.NewPool(context.Background(), cfg)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

func countPitches(t *testing.T, pool *pgxpool.Pool) int64 {
	t.Helper()
	var n int64
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM pitches`).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	return n
}

func eventually(t *testing.T, within time.Duration, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(within)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(500 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

// --- scenario 1: PostgreSQL disappears mid-stream --------------------------

func TestPostgresOutageDoesNotLosePitches(t *testing.T) {
	// The claim under test is not "the pipeline survives a database outage" --
	// it cannot, the database is where pitches go. The claim is that nothing
	// is *lost*: whatever could not be written is recoverable afterwards.
	bs := brokers(t)
	url := databaseURL(t)

	pool := openPool(t, url)
	truncate(t, pool)
	_ = url

	topic := uniqueTopic(t, "chaos.pg", 1)
	dlq := topic + ".dlq"
	createTopic(t, dlq, 1)

	producer := newProducer(t, bs)
	pipeline := newPipeline(t, pool, producer, "http://localhost:1")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	stop := runConsumer(t, ctx, bs, topic, dlq, "chaos-pg", pipeline.HandlePitchRaw)
	defer stop()

	// Ten pitches with the database up.
	publishPitches(t, producer, topic, "900001", 1, 10)
	eventually(t, 30*time.Second, "the first ten pitches", func() bool {
		return countPitches(t, pool) == 10
	})

	// The database goes away. The pool is left open on purpose: pgxpool
	// reconnects lazily, and closing it here would be testing the test rather
	// than the system -- a closed pool never recovers, so "the pipeline
	// resumed" would be unprovable for the wrong reason.
	stopService(t, "postgres")

	publishPitches(t, producer, topic, "900001", 11, 20)

	// The retry budget is five attempts over roughly three seconds, which is
	// deliberately short: retries block the partition, and a long budget means
	// one unavailable dependency stalls every pitch behind it. A database
	// restart takes longer than that, so these pitches are dead-lettered
	// rather than retried until it returns.
	//
	// That is the designed trade, and it is only acceptable because the dead
	// letter is a durable record that can be replayed. "No loss" here means
	// recoverable, not uninterrupted.
	eventually(t, 60*time.Second, "the outage window to be dead-lettered", func() bool {
		return countTopic(t, bs, dlq) >= 10
	})

	compose(t, "start", "postgres")
	waitHealthy(t, "postgres", 60*time.Second)

	// The pipeline recovers on its own for anything published after the
	// database returns: the same pool, the same consumer, no restart.
	publishPitches(t, producer, topic, "900001", 21, 30)
	eventually(t, 90*time.Second, "the pipeline to resume", func() bool {
		return countPitches(t, pool) >= 20
	})

	stored := countPitches(t, pool)
	dead := countTopic(t, bs, dlq)

	t.Logf("stored=%d dead-lettered=%d published=30", stored, dead)

	// Every pitch is accounted for: written, or sitting in the dead-letter
	// queue waiting to be replayed. None vanished.
	if stored+int64(dead) < 30 {
		t.Fatalf("only %d of 30 pitches are accounted for (%d stored, %d dead-lettered)",
			stored+int64(dead), stored, dead)
	}
}

// --- scenario 2: Redis disappears mid-stream -------------------------------

func TestRedisOutageChangesNothingButSpeed(t *testing.T) {
	// Phase 8 proved this for the REST surface with an unreachable address.
	// This is the stronger version: a real container stopped while pitches are
	// being processed, with the cache attached to the pipeline.
	bs := brokers(t)
	pool := openPool(t, databaseURL(t))
	truncate(t, pool)

	redis := newCache(t)

	topic := uniqueTopic(t, "chaos.redis", 1)
	dlq := topic + ".dlq"
	createTopic(t, dlq, 1)

	producer := newProducer(t, bs)
	pipeline := newPipeline(t, pool, producer, "http://localhost:1").WithCache(redis)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	stop := runConsumer(t, ctx, bs, topic, dlq, "chaos-redis", pipeline.HandlePitchRaw)
	defer stop()

	publishPitches(t, producer, topic, "900002", 1, 5)
	eventually(t, 30*time.Second, "pitches with Redis up", func() bool {
		return countPitches(t, pool) == 5
	})

	stopService(t, "redis")

	publishPitches(t, producer, topic, "900002", 6, 15)

	// Every pitch is still written. The cache is an accelerator: with it gone
	// the live counters stop updating and nothing else changes.
	eventually(t, 60*time.Second, "pitches with Redis down", func() bool {
		return countPitches(t, pool) == 15
	})

	if dead := countTopic(t, bs, dlq); dead != 0 {
		t.Fatalf("%d pitches were dead-lettered because the cache was down", dead)
	}
}

// --- scenario 3: the inference service disappears --------------------------

func TestInferenceOutageStoresPitchesUnscored(t *testing.T) {
	// The pipeline is designed to keep the pitch and lose the score, never the
	// other way round: a score can be backfilled from data already in the
	// database, and a pitch that was never written cannot be recovered at all.
	bs := brokers(t)
	pool := openPool(t, databaseURL(t))
	truncate(t, pool)

	topic := uniqueTopic(t, "chaos.ml", 1)
	dlq := topic + ".dlq"
	createTopic(t, dlq, 1)

	producer := newProducer(t, bs)

	// Pointing at a port nothing listens on is the same thing to the client as
	// a stopped service, and it does not disturb an inference service a
	// developer may be running.
	pipeline := newPipeline(t, pool, producer, "http://127.0.0.1:1")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	stop := runConsumer(t, ctx, bs, topic, dlq, "chaos-ml", pipeline.HandlePitchRaw)
	defer stop()

	publishPitches(t, producer, topic, "900003", 1, 12)

	eventually(t, 60*time.Second, "pitches to be stored unscored", func() bool {
		return countPitches(t, pool) == 12
	})

	var failed int64
	row := pool.QueryRow(context.Background(),
		`SELECT count(*) FROM pitches WHERE prediction_status = 'FAILED'`)
	if err := row.Scan(&failed); err != nil {
		t.Fatalf("count: %v", err)
	}
	if failed != 12 {
		t.Fatalf("%d of 12 pitches are marked FAILED", failed)
	}

	// Marked, not silently zero-scored. A pitch stored with a fabricated score
	// would be indistinguishable from a real one.
	var predictions int64
	row = pool.QueryRow(context.Background(), `SELECT count(*) FROM pitch_predictions`)
	if err := row.Scan(&predictions); err != nil {
		t.Fatalf("count: %v", err)
	}
	if predictions != 0 {
		t.Fatalf("%d predictions were written with no inference service", predictions)
	}

	if dead := countTopic(t, bs, dlq); dead != 0 {
		t.Fatalf("%d pitches were dead-lettered because inference was down", dead)
	}
}

// --- scenario 4: the consumer dies between the work and the commit ---------

func TestConsumerKilledMidFlightLosesNothingAndDuplicatesNothing(t *testing.T) {
	// The window at-least-once delivery exists for: the work is durably done
	// and the process dies before the offset moves. On restart the broker
	// redelivers, and the natural key has to absorb it.
	//
	// The kill is a cancelled context rather than a signal, because that is
	// what a SIGTERM becomes inside the process -- and it can be aimed
	// precisely, between the write and the commit.
	bs := brokers(t)
	pool := openPool(t, databaseURL(t))
	truncate(t, pool)

	topic := uniqueTopic(t, "chaos.kill", 1)
	dlq := topic + ".dlq"
	createTopic(t, dlq, 1)

	producer := newProducer(t, bs)
	pipeline := newPipeline(t, pool, producer, "http://127.0.0.1:1")

	const group = "chaos-kill"

	publishPitches(t, producer, topic, "900004", 1, 6)

	// First consumer: killed the instant the sixth pitch has been handled,
	// which is as close to "after the work, before the commit" as the
	// consumer's own structure allows.
	ctx, cancel := context.WithCancel(context.Background())
	handled := make(chan struct{}, 16)

	stop := runConsumer(t, ctx, bs, topic, dlq, group,
		func(c context.Context, env *events.Envelope) error {
			err := pipeline.HandlePitchRaw(c, env)
			select {
			case handled <- struct{}{}:
			default:
			}
			return err
		})

	for i := 0; i < 6; i++ {
		select {
		case <-handled:
		case <-time.After(60 * time.Second):
			stop()
			cancel()
			t.Fatalf("only %d pitches were handled before the timeout", i)
		}
	}
	stop()
	cancel()

	first := countPitches(t, pool)
	t.Logf("before the kill: %d pitches stored", first)

	// Second consumer, same group: whatever was not committed comes back.
	ctx2, cancel2 := context.WithCancel(context.Background())
	defer cancel2()
	stop2 := runConsumer(t, ctx2, bs, topic, dlq, group, pipeline.HandlePitchRaw)
	defer stop2()

	publishPitches(t, producer, topic, "900004", 7, 10)

	eventually(t, 90*time.Second, "the remaining pitches", func() bool {
		return countPitches(t, pool) == 10
	})

	// Exactly ten rows for ten distinct pitches, however many times the broker
	// delivered them.
	if n := countPitches(t, pool); n != 10 {
		t.Fatalf("ten pitches produced %d rows", n)
	}
	if dead := countTopic(t, bs, dlq); dead != 0 {
		t.Fatalf("%d messages were dead-lettered by a clean restart", dead)
	}
}

// --- scenario 5: the broker disappears -------------------------------------

func TestKafkaOutageDoesNotKillTheConsumer(t *testing.T) {
	// Found by accident during load testing: a single failed poll returned an
	// error that propagated to main and exited the process. A broker
	// restarting, a rebalance, or a machine briefly out of sockets would take
	// the stream processor down and leave recovery to whatever was supposed to
	// restart it.
	//
	// The consumer now backs off and keeps asking, because the reader
	// reconnects on its own and only needs the loop to survive.
	bs := brokers(t)
	pool := openPool(t, databaseURL(t))
	truncate(t, pool)

	topic := uniqueTopic(t, "chaos.kafka", 1)
	dlq := topic + ".dlq"
	createTopic(t, dlq, 1)

	producer := newProducer(t, bs)
	pipeline := newPipeline(t, pool, producer, "http://127.0.0.1:1")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Run returns only when the consumer stops for good, so a process that
	// would have exited shows up here as a closed channel.
	cfg := pkafka.DefaultConsumerConfig(bs, topic, "chaos-kafka")
	cfg.Concurrency = 1
	cfg.DLQTopic = dlq

	consumer := pkafka.NewConsumer(cfg, producer, quietLogger())
	exited := make(chan error, 1)
	go func() { exited <- consumer.Run(ctx, pipeline.HandlePitchRaw) }()

	publishPitches(t, producer, topic, "900005", 1, 5)
	eventually(t, 60*time.Second, "pitches before the outage", func() bool {
		return countPitches(t, pool) == 5
	})

	stopService(t, "kafka")

	// Long enough for several failed polls.
	select {
	case err := <-exited:
		t.Fatalf("the consumer exited during a broker outage: %v", err)
	case <-time.After(20 * time.Second):
	}

	compose(t, "start", "kafka")
	waitHealthy(t, "kafka", 120*time.Second)

	// The producer's transport caches metadata, so give the cluster a moment
	// to be routable again before asking it to accept writes.
	time.Sleep(5 * time.Second)

	publishPitches(t, producer, topic, "900005", 6, 10)

	// Consumption resumes without a restart.
	eventually(t, 120*time.Second, "consumption to resume", func() bool {
		return countPitches(t, pool) >= 10
	})

	select {
	case err := <-exited:
		t.Fatalf("the consumer exited after recovering: %v", err)
	default:
	}
}
