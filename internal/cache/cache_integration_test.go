//go:build integration

package cache

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
)

// redisURL reports where the test Redis lives, or skips.
func redisURL(t *testing.T) string {
	t.Helper()
	url := os.Getenv("PITCHLAB_TEST_REDIS_URL")
	if url == "" {
		t.Skip("PITCHLAB_TEST_REDIS_URL is not set")
	}
	return url
}

func newTestCache(t *testing.T) *Cache {
	t.Helper()

	c, err := New(DefaultConfig(redisURL(t)), testLogger())
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	if err := c.Ping(context.Background()); err != nil {
		t.Skipf("redis is not reachable: %v", err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c
}

// --- T8.1 cache-aside -----------------------------------------------------

type summary struct {
	PitchCount int     `json:"pitch_count"`
	AvgSpeed   float64 `json:"avg_speed"`
}

func TestAnalyticsRoundTrip(t *testing.T) {
	c := newTestCache(t)
	ctx := context.Background()
	athlete := uuid.New()

	gen := c.Generation(ctx, athlete)
	variant := Variant("from=", "to=", "include=trend")

	var got summary
	if c.GetAnalytics(ctx, athlete, gen, variant, &got) {
		t.Fatal("an athlete nobody has cached produced a hit")
	}

	want := summary{PitchCount: 114, AvgSpeed: 94.6}
	c.SetAnalytics(ctx, athlete, gen, variant, want)

	if !c.GetAnalytics(ctx, athlete, gen, variant, &got) {
		t.Fatal("the second read missed")
	}
	if got != want {
		t.Fatalf("read back %+v, want %+v", got, want)
	}
}

// --- T8.2 TTL -------------------------------------------------------------

func TestCachedEntriesExpire(t *testing.T) {
	c := newTestCache(t)
	ctx := context.Background()
	athlete := uuid.New()
	variant := Variant("ttl")

	c.SetAnalytics(ctx, athlete, 0, variant, summary{PitchCount: 1})

	ttl, err := c.rdb.TTL(ctx, analyticsKey(athlete, 0, variant)).Result()
	if err != nil {
		t.Fatalf("ttl: %v", err)
	}
	// A rollup with no expiry would outlive the deploy that produced it. The
	// TTL is the safety net for an invalidation that never arrives.
	if ttl <= 0 || ttl > SummaryTTL {
		t.Fatalf("ttl is %v, want 0 < ttl <= %v", ttl, SummaryTTL)
	}
}

// --- T8.3 invalidation ----------------------------------------------------

func TestNewPitchInvalidatesEveryCachedVariant(t *testing.T) {
	c := newTestCache(t)
	ctx := context.Background()
	athlete := uuid.New()

	gen := c.Generation(ctx, athlete)

	// Two query shapes cached at once, which is the normal state: the page
	// asks for a window and a set of sections, and different visitors ask for
	// different ones.
	week := Variant("from=2025-05-01", "to=2025-05-08", "include=")
	season := Variant("from=", "to=", "include=distribution,trend")
	c.SetAnalytics(ctx, athlete, gen, week, summary{PitchCount: 20})
	c.SetAnalytics(ctx, athlete, gen, season, summary{PitchCount: 2000})

	var got summary
	if !c.GetAnalytics(ctx, athlete, gen, week, &got) {
		t.Fatal("the entry was not cached")
	}

	// A new pitch arrives for this athlete.
	c.InvalidateAthlete(ctx, athlete)

	next := c.Generation(ctx, athlete)
	if next == gen {
		t.Fatalf("the generation did not move: %d", next)
	}

	// Both variants are now unaddressable. Deleting a single key, as the
	// original design described, would have invalidated one and left the other
	// serving stale numbers while looking like it had worked.
	if c.GetAnalytics(ctx, athlete, next, week, &got) {
		t.Fatal("a stale weekly rollup survived invalidation")
	}
	if c.GetAnalytics(ctx, athlete, next, season, &got) {
		t.Fatal("a stale seasonal rollup survived invalidation")
	}
}

// --- T8.4 versioned keys --------------------------------------------------

func TestSchemaVersionSeparatesCacheEntries(t *testing.T) {
	c := newTestCache(t)
	ctx := context.Background()
	athlete := uuid.New()
	variant := Variant("schema")

	// An entry left behind by a build that cached a different shape. This is
	// what a deploy looks like from the cache's point of view.
	previous := fmt.Sprintf("%sathlete:%s:analytics:s%d:g%d:%s",
		Prefix, athlete, SchemaVersion-1, 0, variant)
	if err := c.rdb.Set(ctx, previous,
		`{"pitch_count":999}`, SummaryTTL).Err(); err != nil {
		t.Fatalf("seed previous version: %v", err)
	}
	t.Cleanup(func() { c.rdb.Del(context.Background(), previous) })

	// The current build does not see it. No cache flush, and no ordering
	// requirement between the deploy and the cache -- the old entries simply
	// become unreachable and expire.
	var got summary
	if c.GetAnalytics(ctx, athlete, 0, variant, &got) {
		t.Fatalf("an entry from schema version %d was read by version %d: %+v",
			SchemaVersion-1, SchemaVersion, got)
	}

	c.SetAnalytics(ctx, athlete, 0, variant, summary{PitchCount: 7})
	if !c.GetAnalytics(ctx, athlete, 0, variant, &got) || got.PitchCount != 7 {
		t.Fatalf("read back %+v, want pitch_count 7", got)
	}
}

// --- T8.8 live state ------------------------------------------------------

func TestLiveCountersAccumulate(t *testing.T) {
	c := newTestCache(t)
	ctx := context.Background()
	session := uuid.New()
	t.Cleanup(func() { c.DropLiveSession(context.Background(), session) })

	speed := func(v float64) *float64 { return &v }

	c.RecordPitch(ctx, session, PitchObservation{
		ReleaseSpeed: speed(95), Score: speed(110),
		IsWhiff: true, ThrownAt: time.Now(), Balls: 1, Strikes: 2,
	})
	c.RecordPitch(ctx, session, PitchObservation{
		ReleaseSpeed: speed(97), Score: speed(90),
		ThrownAt: time.Now(), Balls: 2, Strikes: 2,
	})
	// A pitch the device did not measure. It counts toward the pitch total and
	// must not count toward the averages.
	c.RecordPitch(ctx, session, PitchObservation{ThrownAt: time.Now()})

	live, ok := c.LiveSession(ctx, session)
	if !ok {
		t.Fatal("the live counters were not readable")
	}

	if live.PitchCount != 3 {
		t.Fatalf("pitch_count = %d, want 3", live.PitchCount)
	}
	if live.WhiffCount != 1 {
		t.Fatalf("whiff_count = %d, want 1", live.WhiffCount)
	}
	if live.AvgReleaseSpeed == nil || *live.AvgReleaseSpeed != 96 {
		t.Fatalf("avg speed = %v, want 96 (the unmeasured pitch must not count)",
			live.AvgReleaseSpeed)
	}
	if live.AvgReleaseSpin != nil {
		t.Fatalf("avg spin = %v, want absent", live.AvgReleaseSpin)
	}
	if live.AvgPitchScore == nil || *live.AvgPitchScore != 100 {
		t.Fatalf("avg score = %v, want 100", live.AvgPitchScore)
	}
	if live.LastPitchAt == nil {
		t.Fatal("last_pitch_at was not recorded")
	}
}

func TestLiveStateExpires(t *testing.T) {
	c := newTestCache(t)
	ctx := context.Background()
	session := uuid.New()
	t.Cleanup(func() { c.DropLiveSession(context.Background(), session) })

	c.RecordPitch(ctx, session, PitchObservation{ThrownAt: time.Now()})

	ttl, err := c.rdb.TTL(ctx, liveKey(session)).Result()
	if err != nil {
		t.Fatalf("ttl: %v", err)
	}
	// Without an expiry, every outing ever replayed would stay in memory.
	// Nothing cleans these up; the TTL is the cleanup.
	if ttl <= 0 || ttl > LiveTTL {
		t.Fatalf("ttl is %v, want 0 < ttl <= %v", ttl, LiveTTL)
	}
}

func TestDroppingLiveStateMakesTheCacheMiss(t *testing.T) {
	c := newTestCache(t)
	ctx := context.Background()
	session := uuid.New()

	c.RecordPitch(ctx, session, PitchObservation{ThrownAt: time.Now()})
	if _, ok := c.LiveSession(ctx, session); !ok {
		t.Fatal("the counters were not recorded")
	}

	c.DropLiveSession(ctx, session)

	// A miss here is the signal to rebuild from PostgreSQL, which is what
	// makes the counters safe to throw away at all.
	if _, ok := c.LiveSession(ctx, session); ok {
		t.Fatal("the counters survived being dropped")
	}
}

// --- fallback -------------------------------------------------------------

func TestReadsAfterFlushMissRatherThanFail(t *testing.T) {
	c := newTestCache(t)
	ctx := context.Background()
	session := uuid.New()

	c.RecordPitch(ctx, session, PitchObservation{ThrownAt: time.Now()})

	// The operational equivalent of someone clearing the cache underneath a
	// running system. It has to look like a cold cache, not like a failure.
	if err := c.rdb.Del(ctx, liveKey(session)).Err(); err != nil {
		t.Fatalf("del: %v", err)
	}

	if _, ok := c.LiveSession(ctx, session); ok {
		t.Fatal("a flushed key still reported a hit")
	}
}
