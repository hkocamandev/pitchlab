package cache

import (
	"context"
	"io"
	"log/slog"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
)

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard,
		&slog.HandlerOptions{Level: slog.LevelError}))
}

// A nil cache is the one configuration every caller gets for free, so it is
// the one that has to be exercised hardest: it is what runs when Redis is not
// configured at all, and it must never panic or report a hit.
func TestNilCacheIsUsableAndAlwaysMisses(t *testing.T) {
	var c *Cache
	ctx := context.Background()
	id := uuid.New()

	c.RecordPitch(ctx, id, PitchObservation{})
	c.DropLiveSession(ctx, id)
	c.InvalidateAthlete(ctx, id)
	c.SetAnalytics(ctx, id, 1, "v", map[string]string{"a": "b"})

	if _, ok := c.LiveSession(ctx, id); ok {
		t.Fatal("a nil cache reported a live session")
	}

	var target map[string]string
	if c.GetAnalytics(ctx, id, 1, "v", &target) {
		t.Fatal("a nil cache reported an analytics hit")
	}
	if gen := c.Generation(ctx, id); gen != 0 {
		t.Fatalf("a nil cache reported generation %d", gen)
	}
	if err := c.Ping(ctx); err == nil {
		t.Fatal("a nil cache reported itself reachable")
	}
	if err := c.Close(); err != nil {
		t.Fatalf("closing a nil cache failed: %v", err)
	}
}

// An unreachable Redis has to behave exactly like a nil one. This is the
// property the whole design rests on, and it is cheap to assert without a
// server: a port nothing is listening on is an unreachable Redis.
func TestUnreachableRedisBehavesLikeNoCache(t *testing.T) {
	c, err := New(DefaultConfig("redis://127.0.0.1:1/0"), testLogger())
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	defer func() { _ = c.Close() }()

	ctx := context.Background()
	id := uuid.New()

	var errors int
	c.WithMetrics(Metrics{OnOperation: func(_, result string) {
		if result == ResultError {
			errors++
		}
	}})

	c.RecordPitch(ctx, id, PitchObservation{})
	if _, ok := c.LiveSession(ctx, id); ok {
		t.Fatal("an unreachable Redis reported a live session")
	}

	var target map[string]string
	if c.GetAnalytics(ctx, id, 1, "v", &target) {
		t.Fatal("an unreachable Redis reported an analytics hit")
	}
	if err := c.Ping(ctx); err == nil {
		t.Fatal("an unreachable Redis reported itself reachable")
	}

	// The failures are counted rather than swallowed silently: an outage
	// should be visible in the metrics even though it is invisible to callers.
	if errors == 0 {
		t.Fatal("failures against an unreachable Redis were not recorded")
	}
}

func TestKeysAreNamespaced(t *testing.T) {
	id := uuid.New()

	for name, key := range map[string]string{
		"live":       liveKey(id),
		"generation": generationKey(id),
		"analytics":  analyticsKey(id, 3, "abc"),
	} {
		// The prefix is what makes the project's keys separable in a shared
		// Redis and what makes `SCAN pl:*` a complete answer.
		if !strings.HasPrefix(key, Prefix) {
			t.Errorf("%s key %q is not namespaced", name, key)
		}
		if !strings.Contains(key, id.String()) {
			t.Errorf("%s key %q does not identify its subject", name, key)
		}
	}
}

func TestAnalyticsKeyCarriesSchemaAndGeneration(t *testing.T) {
	id := uuid.New()

	// The schema version is in the key so that a deploy which changes the
	// cached shape misses every pre-deploy entry instead of decoding it.
	if analyticsKey(id, 1, "v") == analyticsKey(id, 2, "v") {
		t.Fatal("two generations share a key; invalidation would do nothing")
	}

	// Different query shapes must not share an entry, or a request for one
	// week would be answered with a year.
	if analyticsKey(id, 1, "a") == analyticsKey(id, 1, "b") {
		t.Fatal("two query variants share a key")
	}
}

func TestVariantIsStableAndOrderSensitive(t *testing.T) {
	a := Variant("from=2025-01-01", "to=2025-02-01", "include=trend")
	b := Variant("from=2025-01-01", "to=2025-02-01", "include=trend")
	if a != b {
		t.Fatal("the same parameters produced two different variants")
	}

	if Variant("from=2025-01-01", "to=") == Variant("from=", "to=2025-01-01") {
		// Without the separator, "from=2025-01-01"+"to=" and ""+"..." could
		// collide by concatenation. The delimiter is what prevents it.
		t.Fatal("two different parameter sets collided")
	}

	if len(a) != 16 {
		t.Fatalf("variant is %d characters; keys should not grow with input", len(a))
	}
}

func TestAverageIgnoresPitchesThatLackedTheField(t *testing.T) {
	// Two pitches carried a spin reading and summed to 5000. A third had none.
	// Averaging over three would report 1667 rpm, which is wrong and plausible
	// enough to survive review.
	got := average("5000", "2")
	if got == nil || *got != 2500 {
		t.Fatalf("average = %v, want 2500", got)
	}

	if average("0", "0") != nil {
		t.Fatal("an average over no observations should be absent, not zero")
	}
	if average("not a number", "2") != nil {
		t.Fatal("an unparseable sum should be absent, not zero")
	}
}

func TestGenerationTTLOutlivesCachedEntries(t *testing.T) {
	// If the generation counter could expire while entries from an older
	// generation were still alive, it would restart at 1 and address an entry
	// that is no longer current -- serving stale data with no way to notice.
	if GenerationTTL <= SummaryTTL {
		t.Fatalf("generation TTL %s does not outlive cached entries at %s",
			GenerationTTL, SummaryTTL)
	}
}

func TestCallsGiveUpAtTheDeadline(t *testing.T) {
	// A server that accepts the connection and then says nothing. This is the
	// dangerous failure, not a refused connection: without a deadline the call
	// would wait as long as the socket stays open, and the cache would turn
	// from a speedup into a tax charged on every request.
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer func() { _ = listener.Close() }()

	var (
		mu    sync.Mutex
		conns []net.Conn
	)
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			// Held open deliberately, and never answered.
			mu.Lock()
			conns = append(conns, conn)
			mu.Unlock()
		}
	}()
	t.Cleanup(func() {
		mu.Lock()
		defer mu.Unlock()
		for _, conn := range conns {
			_ = conn.Close()
		}
	})

	c, err := New(DefaultConfig("redis://"+listener.Addr().String()+"/0"), testLogger())
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	defer func() { _ = c.Close() }()

	start := time.Now()
	if _, ok := c.LiveSession(context.Background(), uuid.New()); ok {
		t.Fatal("a silent server produced a hit")
	}
	elapsed := time.Since(start)

	// One deadline, not three: the client is configured with no retries, so an
	// unresponsive Redis costs one timeout rather than three stacked inside a
	// single request.
	if elapsed > 3*Timeout {
		t.Fatalf("call took %v against a silent server; the deadline is %v",
			elapsed, Timeout)
	}
}
