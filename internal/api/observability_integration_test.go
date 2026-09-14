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
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/hkocamandev/pitchlab/internal/api"
	"github.com/hkocamandev/pitchlab/internal/config"
	"github.com/hkocamandev/pitchlab/internal/db"
	"github.com/hkocamandev/pitchlab/internal/observability"
)

func newObservableServer(
	t *testing.T,
) (*httptest.Server, *observability.Metrics, *pgxpool.Pool) {
	t.Helper()

	databaseURL := os.Getenv("PITCHLAB_TEST_DATABASE_URL")
	if databaseURL == "" {
		t.Skip("PITCHLAB_TEST_DATABASE_URL is not set")
	}

	metrics := observability.New()

	poolCfg := db.DefaultPoolConfig(databaseURL)
	poolCfg.Tracer = observability.NewQueryTracer(metrics)
	poolCfg.ConnectTimeout = time.Second

	pool, err := db.NewPool(context.Background(), poolCfg)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(pool.Close)

	cfg := config.API{
		Addr:            ":0",
		DatabaseURL:     databaseURL,
		AllowedOrigins:  []string{"http://localhost:5173"},
		DefaultPageSize: 50,
		MaxPageSize:     500,
		ReadTimeout:     10 * time.Second,
		LogLevel:        "error",
		LogFormat:       "json",
		Version:         "test",
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("config: %v", err)
	}

	log := slog.New(slog.NewJSONHandler(io.Discard, nil))
	srv := httptest.NewServer(
		api.New(cfg, db.NewStore(pool), log).WithMetrics(metrics).Routes())
	t.Cleanup(srv.Close)

	return srv, metrics, pool
}

// loseTheDatabase simulates PostgreSQL going away after the process started.
//
// Closing the pool rather than pointing at a dead port, because the pool
// verifies its connection at startup on purpose -- a service configured
// against an unreachable database should fail loudly at boot. The interesting
// case is the one that happens in production: a process that started fine and
// then lost its database.
func loseTheDatabase(pool *pgxpool.Pool) { pool.Close() }

// --- T10.2 the scrape endpoint --------------------------------------------

func TestMetricsEndpointReflectsRealTraffic(t *testing.T) {
	srv, _, _ := newObservableServer(t)

	for i := 0; i < 3; i++ {
		get(t, srv, "/api/v1/models")
	}

	_, body := get(t, srv, "/metrics")
	text := string(body)

	if !strings.Contains(text, `route="/api/v1/models"`) {
		t.Fatalf("the scrape does not mention the route that was called:\n%s", text)
	}
	// The query tracer is wired through the pool, so a request that touched
	// the database has to leave a query timing behind.
	if !strings.Contains(text, "db_query_duration_seconds") {
		t.Fatal("no database query timings were recorded")
	}
	if !strings.Contains(text, `query_name="ListModelVersions"`) &&
		!strings.Contains(text, "query_name=") {
		t.Fatal("query timings carry no query name")
	}
}

// --- T10.3 cardinality, end to end ----------------------------------------

func TestRealTrafficCreatesNoPerEntitySeries(t *testing.T) {
	srv, store := newTestServer(t)
	truncate(t, store)
	f := seed(t, store, 8)

	// The same server, rebuilt with metrics attached so the requests below
	// are the ones being measured.
	observable, _, _ := newObservableServer(t)
	_ = srv

	athlete := f.pitcher.ID.String()
	for _, path := range []string{
		"/api/v1/athletes/" + athlete,
		"/api/v1/athletes/" + athlete + "/analytics",
		"/api/v1/sessions/" + f.session.ID.String(),
		"/api/v1/pitches/" + f.pitches[0].ID.String(),
	} {
		get(t, observable, path)
	}

	_, body := get(t, observable, "/metrics")

	// The identifiers are in the paths that were requested. If any of them
	// reached a label, they would be in the scrape -- and each would be its
	// own time series forever.
	for _, id := range []string{athlete, f.session.ID.String(), f.pitches[0].ID.String()} {
		if strings.Contains(string(body), id) {
			t.Fatalf("the scrape contains the identifier %s", id)
		}
	}
}

// --- T10.4 liveness is independent ----------------------------------------

func TestHealthzAnswersWithTheDatabaseGone(t *testing.T) {
	// The scenario this separation exists for: PostgreSQL becomes
	// unreachable, every instance's liveness probe fails, the orchestrator
	// restarts all of them at once, and the cold starts turn a recoverable
	// blip into an outage.
	//
	// Liveness answers "is this process wedged". That is all it should answer.
	srv, _, pool := newObservableServer(t)
	loseTheDatabase(pool)

	start := time.Now()
	resp, body := get(t, srv, "/healthz")
	elapsed := time.Since(start)

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("healthz returned %d with the database gone: %s",
			resp.StatusCode, body)
	}
	// Fast as well as successful: a liveness probe that waits on a timeout is
	// a liveness probe that trips on its own deadline.
	if elapsed > time.Second {
		t.Fatalf("healthz took %v; it must not touch a dependency", elapsed)
	}
}

// --- T10.5 readiness distinguishes required from optional -----------------

func TestReadyzIsUnavailableWithTheDatabaseGone(t *testing.T) {
	srv, _, pool := newObservableServer(t)
	loseTheDatabase(pool)

	resp, body := get(t, srv, "/readyz")

	// 503, so the instance leaves the load balancer -- but it is not
	// restarted, and it returns on its own when PostgreSQL does.
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("readyz returned %d with the database gone: %s",
			resp.StatusCode, body)
	}

	var out struct {
		Status string `json:"status"`
		Checks map[string]struct {
			Status string `json:"status"`
		} `json:"checks"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if out.Status != "unavailable" {
		t.Fatalf("status is %q, want unavailable", out.Status)
	}
	if out.Checks["postgres"].Status != "down" {
		t.Fatalf("postgres reported %q", out.Checks["postgres"].Status)
	}
}
