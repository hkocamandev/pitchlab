//go:build integration

package api_test

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"github.com/hkocamandev/pitchlab/internal/api"
	"github.com/hkocamandev/pitchlab/internal/cache"
	"github.com/hkocamandev/pitchlab/internal/config"
	"github.com/hkocamandev/pitchlab/internal/db"
	"github.com/hkocamandev/pitchlab/internal/httpx"
)

// unreachableRedis points at a port nothing listens on.
//
// Standing in for `docker compose stop redis` without stopping the container
// every other test in the suite is using. From the client's side the two are
// the same thing: a connection that cannot be established.
const unreachableRedis = "redis://127.0.0.1:1/0"

func newCachedTestServer(
	t *testing.T, redisURL string,
) (*httptest.Server, *db.Store, *cache.Cache) {
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

	cfg := config.API{
		Addr:            ":0",
		DatabaseURL:     url,
		RedisURL:        redisURL,
		AllowedOrigins:  []string{"http://localhost:5173"},
		DefaultPageSize: 50,
		MaxPageSize:     500,
		ReadTimeout:     10 * time.Second,
		LogLevel:        "error",
		LogFormat:       "json",
		Version:         "test",
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("test config invalid: %v", err)
	}

	log := slog.New(slog.NewJSONHandler(io.Discard, nil))

	redis, err := cache.New(cache.DefaultConfig(redisURL), log)
	if err != nil {
		t.Fatalf("cache: %v", err)
	}
	t.Cleanup(func() { _ = redis.Close() })

	srv := httptest.NewServer(api.New(cfg, store, log).WithCache(redis).Routes())
	t.Cleanup(srv.Close)

	return srv, store, redis
}

func liveRedisURL(t *testing.T) string {
	t.Helper()
	url := os.Getenv("PITCHLAB_TEST_REDIS_URL")
	if url == "" {
		t.Skip("PITCHLAB_TEST_REDIS_URL is not set")
	}
	return url
}

// --- T8.1 cache-aside through the API -------------------------------------

func TestAnalyticsIsServedFromCacheOnTheSecondRequest(t *testing.T) {
	srv, store, redis := newCachedTestServer(t, liveRedisURL(t))
	truncate(t, store)
	f := seed(t, store, 12)

	ctx := context.Background()
	// A previous run of this test leaves a generation behind; moving it makes
	// this run's first request a genuine miss.
	redis.InvalidateAthlete(ctx, f.pitcher.ID)

	path := "/api/v1/athletes/" + f.pitcher.ID.String() + "/analytics"

	resp, _ := get(t, srv, path)
	if got := resp.Header.Get(httpx.HeaderCacheState); got != "MISS" {
		t.Fatalf("first request reported %q, want MISS", got)
	}

	resp, second := get(t, srv, path)
	if got := resp.Header.Get(httpx.HeaderCacheState); got != "HIT" {
		t.Fatalf("second request reported %q, want HIT", got)
	}

	// A hit has to be the same answer, not merely a fast one.
	_, first := get(t, srv, path)
	if string(first) != string(second) {
		t.Fatal("the cached response differs from the computed one")
	}
}

func TestDifferentWindowsDoNotShareACacheEntry(t *testing.T) {
	srv, store, redis := newCachedTestServer(t, liveRedisURL(t))
	truncate(t, store)
	f := seed(t, store, 12)
	redis.InvalidateAthlete(context.Background(), f.pitcher.ID)

	base := "/api/v1/athletes/" + f.pitcher.ID.String() + "/analytics"

	// Populate the unfiltered entry.
	get(t, srv, base)
	get(t, srv, base)

	// A window in which this athlete threw nothing. If the two shared a key,
	// this would be answered with the full-history numbers.
	narrow := base + "?from=1990-01-01T00:00:00Z&to=1990-01-02T00:00:00Z"
	resp, body := get(t, srv, narrow)
	if got := resp.Header.Get(httpx.HeaderCacheState); got != "MISS" {
		t.Fatalf("a different window reported %q, want MISS", got)
	}

	var payload struct {
		Summary struct {
			PitchCount int64 `json:"pitch_count"`
		} `json:"summary"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if payload.Summary.PitchCount != 0 {
		t.Fatalf("a window with no pitches reported %d; entries were shared",
			payload.Summary.PitchCount)
	}
}

// --- T8.3 invalidation ----------------------------------------------------

func TestANewPitchInvalidatesTheCachedRollup(t *testing.T) {
	srv, store, redis := newCachedTestServer(t, liveRedisURL(t))
	truncate(t, store)
	f := seed(t, store, 12)

	ctx := context.Background()
	redis.InvalidateAthlete(ctx, f.pitcher.ID)

	path := "/api/v1/athletes/" + f.pitcher.ID.String() + "/analytics"
	get(t, srv, path)

	resp, _ := get(t, srv, path)
	if got := resp.Header.Get(httpx.HeaderCacheState); got != "HIT" {
		t.Fatalf("the rollup was not cached: %q", got)
	}

	// What the processor does when it stores a pitch for this athlete.
	redis.InvalidateAthlete(ctx, f.pitcher.ID)

	resp, _ = get(t, srv, path)
	if got := resp.Header.Get(httpx.HeaderCacheState); got != "MISS" {
		t.Fatalf("a stale rollup was served after invalidation: %q", got)
	}
}

// --- T8.5 Redis down ------------------------------------------------------

// The phase does not close without this test.
//
// It is the concrete form of the claim that Redis is an accelerator and not a
// dependency: with Redis unreachable, every endpoint answers exactly as it
// would with a cold cache, and the only thing that changes is how long it
// takes.
func TestEveryEndpointStillAnswersWithRedisDown(t *testing.T) {
	srv, store, _ := newCachedTestServer(t, unreachableRedis)
	truncate(t, store)
	f := seed(t, store, 12)

	athlete := f.pitcher.ID.String()
	session := f.session.ID.String()
	pitch := f.pitches[0].ID.String()

	paths := []string{
		"/api/v1/athletes",
		"/api/v1/athletes/" + athlete,
		"/api/v1/athletes/" + athlete + "/analytics",
		"/api/v1/athletes/" + athlete + "/analytics?include=distribution,trend",
		"/api/v1/athletes/" + athlete + "/sessions",
		"/api/v1/athletes/" + athlete + "/anomalies",
		// This endpoint requires a pitcher or an explicit ACTIVE filter; that
		// is a routing rule, not a cache concern.
		"/api/v1/sessions?status=ACTIVE",
		"/api/v1/sessions/" + session,
		"/api/v1/sessions/" + session + "/pitches",
		"/api/v1/pitches?pitcher_id=" + athlete,
		"/api/v1/pitches/" + pitch,
		"/api/v1/pitches/" + pitch + "/predictions",
		"/api/v1/models",
	}

	for _, path := range paths {
		resp, body := get(t, srv, path)
		if resp.StatusCode != http.StatusOK {
			t.Errorf("GET %s returned %d with Redis down: %s",
				path, resp.StatusCode, body)
		}
	}
}

func TestAnalyticsIsCorrectWithRedisDown(t *testing.T) {
	srv, store, _ := newCachedTestServer(t, unreachableRedis)
	truncate(t, store)
	f := seed(t, store, 12)

	resp, body := get(t, srv, "/api/v1/athletes/"+f.pitcher.ID.String()+"/analytics")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d: %s", resp.StatusCode, body)
	}

	// Reported as a miss, because that is what it is. The endpoint degrades
	// into "always recompute", which is the behaviour it had before the cache
	// existed.
	if got := resp.Header.Get(httpx.HeaderCacheState); got != "MISS" {
		t.Fatalf("cache state is %q with Redis down, want MISS", got)
	}

	var payload struct {
		Summary struct {
			PitchCount int64 `json:"pitch_count"`
		} `json:"summary"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if payload.Summary.PitchCount != 12 {
		t.Fatalf("pitch_count = %d, want 12", payload.Summary.PitchCount)
	}
}

// --- T8.7 readiness -------------------------------------------------------

func TestReadyzReportsRedisAsDegradedNotUnready(t *testing.T) {
	srv, store, _ := newCachedTestServer(t, unreachableRedis)
	truncate(t, store)

	resp, body := get(t, srv, "/readyz")

	// 200, not 503. Reporting the instance as unready would take it out of
	// rotation over a cache outage and turn a slowdown into an outage.
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d with Redis down: %s", resp.StatusCode, body)
	}

	var payload struct {
		Status   string   `json:"status"`
		Degraded []string `json:"degraded"`
		Checks   map[string]struct {
			Status string `json:"status"`
		} `json:"checks"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		t.Fatalf("decode: %v", err)
	}

	if payload.Status != "degraded" {
		t.Fatalf("status is %q, want degraded", payload.Status)
	}
	if len(payload.Degraded) != 1 || payload.Degraded[0] != "redis" {
		t.Fatalf("degraded = %v, want [redis]", payload.Degraded)
	}
	// PostgreSQL is the dependency that decides readiness, and it is fine.
	if payload.Checks["postgres"].Status != "ok" {
		t.Fatalf("postgres reported %q", payload.Checks["postgres"].Status)
	}
}

func TestReadyzIsHealthyWithRedisUp(t *testing.T) {
	srv, store, _ := newCachedTestServer(t, liveRedisURL(t))
	truncate(t, store)

	resp, body := get(t, srv, "/readyz")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d: %s", resp.StatusCode, body)
	}

	var payload struct {
		Status   string   `json:"status"`
		Degraded []string `json:"degraded"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if payload.Status != "ok" || len(payload.Degraded) != 0 {
		t.Fatalf("status %q degraded %v with Redis reachable",
			payload.Status, payload.Degraded)
	}
}
