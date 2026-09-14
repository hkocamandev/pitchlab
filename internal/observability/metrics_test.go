package observability

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	dto "github.com/prometheus/client_model/go"
)

// exercise drives every metric once, so the registry contains real series
// rather than empty declarations. A cardinality test against unused metrics
// would pass on a registry that exports nothing.
func exercise(m *Metrics) {
	m.HTTPDuration.WithLabelValues("GET", "/api/v1/athletes/{athleteId}", "200").Observe(0.01)
	m.HTTPTotal.WithLabelValues("GET", "/api/v1/athletes/{athleteId}", "200").Inc()
	m.HTTPInFlight.Inc()

	onSent, onDropped, onSlow := m.WebSocketHooks()
	onSent("pitch.analyzed")
	onDropped("session.metrics")
	onSlow(1)

	m.CacheHook()("analytics_read", "hit")
	m.DBQueryDuration.WithLabelValues("GetAthleteSummary").Observe(0.004)

	onProcessed, onDLQ := m.ConsumerHooks()
	onProcessed("pitchlab.pitch.raw.v1", "ok", 1, 12*time.Millisecond)
	onDLQ("pitchlab.pitch.raw.v1", "SCHEMA_VALIDATION")
	m.LagHook()("pitchlab.pitch.raw.v1", 2, 41)
	m.ProducerHooks()("pitchlab.pitch.analyzed.v1")

	onDup, onPersisted, onPrediction := m.PipelineHooks()
	onDup()
	onPersisted(3 * time.Millisecond)
	onPrediction("pitching", 8*time.Millisecond, nil)
	onPrediction("stuff", 9*time.Millisecond, context.DeadlineExceeded)

	m.PredictionStoredHook()("pitching", "pitch-outcome-v1.0.0")
	m.AnomalyHook()("release_speed", "HIGH")
}

func gather(t *testing.T, m *Metrics) []*dto.MetricFamily {
	t.Helper()
	families, err := m.Registry().Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}
	return families
}

// --- T10.3 cardinality ----------------------------------------------------

func TestNoMetricCarriesAnIdentifierLabel(t *testing.T) {
	m := New()
	m.TrackWebSocketConnections(func() int64 { return 3 })
	m.TrackBreaker(func() string { return "closed" })
	exercise(m)

	forbidden := map[string]bool{}
	for _, name := range ForbiddenLabels {
		forbidden[name] = true
	}

	for _, family := range gather(t, m) {
		for _, metric := range family.GetMetric() {
			for _, label := range metric.GetLabel() {
				// One label per session is one time series per outing. A
				// replay at 800x would create millions within a minute, and
				// the metrics stack would be taken down by the system it is
				// supposed to be watching.
				if forbidden[label.GetName()] {
					t.Errorf("%s carries the forbidden label %q",
						family.GetName(), label.GetName())
				}
			}
		}
	}
}

var uuidLike = regexp.MustCompile(
	`[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}`)

func TestNoLabelValueLooksLikeAnIdentifier(t *testing.T) {
	m := New()
	exercise(m)

	// The stronger half of the rule. A label *named* innocently can still
	// carry an identifier -- "route" would, if it were the requested path
	// instead of the registered pattern.
	for _, family := range gather(t, m) {
		for _, metric := range family.GetMetric() {
			for _, label := range metric.GetLabel() {
				if uuidLike.MatchString(label.GetValue()) {
					t.Errorf("%s has label %s=%q, which contains an identifier",
						family.GetName(), label.GetName(), label.GetValue())
				}
			}
		}
	}
}

func TestSeriesCountStaysBounded(t *testing.T) {
	m := New()

	// Five hundred requests across the same handful of routes must not
	// produce five hundred series.
	for i := 0; i < 500; i++ {
		m.HTTPTotal.WithLabelValues("GET", "/api/v1/athletes/{athleteId}", "200").Inc()
		m.HTTPTotal.WithLabelValues("GET", "/api/v1/sessions/{sessionId}", "200").Inc()
	}

	var series int
	for _, family := range gather(t, m) {
		if family.GetName() == "http_requests_total" {
			series = len(family.GetMetric())
		}
	}
	if series != 2 {
		t.Fatalf("http_requests_total has %d series, want 2", series)
	}
}

// --- T10.2 metric presence ------------------------------------------------

func TestEveryDesignedMetricIsExported(t *testing.T) {
	m := New()
	m.TrackWebSocketConnections(func() int64 { return 0 })
	m.TrackBreaker(func() string { return "closed" })
	exercise(m)

	body := scrape(t, m)

	// Checked by name rather than by counting, so a metric that is renamed
	// breaks the dashboard query and this test at the same time -- instead of
	// only the dashboard, silently.
	want := []string{
		"http_request_duration_seconds",
		"http_requests_total",
		"http_requests_in_flight",
		"websocket_connections_active",
		"websocket_messages_sent_total",
		"websocket_messages_dropped_total",
		"websocket_slow_consumers",
		"cache_operations_total",
		"db_query_duration_seconds",
		"kafka_messages_consumed_total",
		"kafka_consumer_lag",
		"kafka_message_processing_duration_seconds",
		"kafka_produce_errors_total",
		"kafka_dlq_messages_total",
		"pitch_persist_duration_seconds",
		"pitch_duplicates_skipped_total",
		"predictions_generated_total",
		"anomalies_detected_total",
		"ml_inference_duration_seconds",
		"ml_inference_errors_total",
		"ml_circuit_breaker_state",
		// Runtime metrics, which answer "is this process healthy" with no
		// application code at all. The goroutine count is what would show a
		// WebSocket leak.
		"go_goroutines",
	}

	for _, name := range want {
		if !strings.Contains(body, name) {
			t.Errorf("/metrics does not export %s", name)
		}
	}
}

func TestProcessMetricsAreExportedWhereTheyExist(t *testing.T) {
	m := New()
	body := scrape(t, m)

	// The process collector reads /proc, so it produces nothing on macOS and
	// everything in a Linux container. Asserting unconditionally would fail
	// on a developer machine and asserting nothing would let a real
	// regression through, so the platform difference is stated rather than
	// papered over: where the collector works, these have to be there.
	if !strings.Contains(body, "process_") {
		t.Skip("process metrics are unavailable on this platform")
	}
	for _, name := range []string{
		"process_resident_memory_bytes",
		"process_open_fds",
	} {
		if !strings.Contains(body, name) {
			t.Errorf("/metrics does not export %s", name)
		}
	}
}

func scrape(t *testing.T, m *Metrics) string {
	t.Helper()
	rec := httptest.NewRecorder()
	m.Handler().ServeHTTP(rec, httptest.NewRequest("GET", "/metrics", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("scrape returned %d", rec.Code)
	}
	return rec.Body.String()
}

// --- gauges read from their source ----------------------------------------

func TestGaugesReadTheirSourceAtScrapeTime(t *testing.T) {
	m := New()

	connections := int64(0)
	state := "closed"
	m.TrackWebSocketConnections(func() int64 { return connections })
	m.TrackBreaker(func() string { return state })

	if got := valueOf(t, m, "websocket_connections_active"); got != 0 {
		t.Fatalf("connections = %v, want 0", got)
	}

	connections = 7
	state = "open"

	// A counted gauge would need an event to move. Reading the source means
	// the number cannot drift away from the truth, which is the failure mode
	// that makes connection gauges untrustworthy.
	if got := valueOf(t, m, "websocket_connections_active"); got != 7 {
		t.Fatalf("connections = %v, want 7", got)
	}
	if got := valueOf(t, m, "ml_circuit_breaker_state"); got != 2 {
		t.Fatalf("breaker = %v, want 2 (open)", got)
	}
}

func valueOf(t *testing.T, m *Metrics, name string) float64 {
	t.Helper()
	for _, family := range gather(t, m) {
		if family.GetName() != name {
			continue
		}
		metric := family.GetMetric()[0]
		if g := metric.GetGauge(); g != nil {
			return g.GetValue()
		}
	}
	t.Fatalf("metric %s not found", name)
	return 0
}

// --- error classification -------------------------------------------------

func TestErrorsAreClassifiedIntoAFixedVocabulary(t *testing.T) {
	cases := map[string]error{
		"timeout":      context.DeadlineExceeded,
		"canceled":     context.Canceled,
		"breaker_open": errors.New("circuit breaker is open"),
		"unreachable":  errors.New(`dial tcp 127.0.0.1:8000: connect: connection refused`),
		"rejected":     errors.New("inference returned status 400"),
		"server_error": errors.New("inference returned status 502"),
		"unknown":      errors.New("something nobody anticipated"),
		"none":         nil,
	}

	for want, err := range cases {
		if got := ClassifyMLError(err); got != want {
			t.Errorf("ClassifyMLError(%v) = %q, want %q", err, got, want)
		}
	}
}

func TestErrorTextNeverBecomesALabel(t *testing.T) {
	m := New()
	_, _, onPrediction := m.PipelineHooks()

	// An error message can contain a host, a port, an identifier or a whole
	// SQL statement. Using it as a label value is the same cardinality
	// mistake as labelling by session id, only harder to notice.
	for i := 0; i < 50; i++ {
		onPrediction("pitching", time.Millisecond,
			errors.New("call failed for pitch 0198f2a1-0000-7000-8000-00000000000"+
				string(rune('0'+i%10))))
	}

	for _, family := range gather(t, m) {
		if family.GetName() != "ml_inference_errors_total" {
			continue
		}
		if n := len(family.GetMetric()); n != 1 {
			t.Fatalf("50 distinct error messages produced %d series, want 1", n)
		}
	}
}

// --- query naming ---------------------------------------------------------

func TestQueryNameComesFromTheGeneratedHeader(t *testing.T) {
	sql := "-- name: GetAthleteSummary :one\nSELECT count(*) FROM pitches WHERE id = $1"
	if got := QueryName(sql); got != "GetAthleteSummary" {
		t.Fatalf("QueryName = %q", got)
	}

	// A query with no header is hand-written or built at runtime. Using its
	// text as the label would create a series per statement.
	if got := QueryName("SELECT 1"); got != "unnamed" {
		t.Fatalf("QueryName without a header = %q, want unnamed", got)
	}
}

func TestQueryNamesStayBounded(t *testing.T) {
	m := New()
	tracer := NewQueryTracer(m)

	for i := 0; i < 100; i++ {
		ctx := tracer.TraceQueryStart(context.Background(), nil,
			pgx.TraceQueryStartData{
				SQL: "-- name: GetPitch :one\nSELECT * FROM pitches WHERE id = $1",
			})
		tracer.TraceQueryEnd(ctx, nil, pgx.TraceQueryEndData{})
	}

	for _, family := range gather(t, m) {
		if family.GetName() != "db_query_duration_seconds" {
			continue
		}
		if n := len(family.GetMetric()); n != 1 {
			t.Fatalf("100 executions of one query produced %d series, want 1", n)
		}
	}
}

// --- HTTP instrumentation -------------------------------------------------

func TestInstrumentRouteLabelsByPatternAndStatus(t *testing.T) {
	m := New()

	handler := m.InstrumentRoute("/api/v1/athletes/{athleteId}",
		http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusNotFound)
		}))

	for _, id := range []string{
		"0198f2a1-0000-7000-8000-000000000001",
		"0198f2a1-0000-7000-8000-000000000002",
		"0198f2a1-0000-7000-8000-000000000003",
	} {
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, httptest.NewRequest("GET", "/api/v1/athletes/"+id, nil))
	}

	var series int
	var total float64
	for _, family := range gather(t, m) {
		if family.GetName() != "http_requests_total" {
			continue
		}
		series = len(family.GetMetric())
		for _, metric := range family.GetMetric() {
			total += metric.GetCounter().GetValue()
			for _, label := range metric.GetLabel() {
				if label.GetName() == "route" && label.GetValue() != "/api/v1/athletes/{athleteId}" {
					t.Errorf("route label is %q; it must be the pattern, not the path",
						label.GetValue())
				}
				if label.GetName() == "status" && label.GetValue() != "404" {
					t.Errorf("status label is %q, want 404", label.GetValue())
				}
			}
		}
	}

	// Three different athletes, one time series.
	if series != 1 {
		t.Fatalf("three paths produced %d series, want 1", series)
	}
	if total != 3 {
		t.Fatalf("counted %v requests, want 3", total)
	}
}

func TestInstrumentedResponseCanBeHijacked(t *testing.T) {
	// The WebSocket upgrade takes the connection over, and the upgrade library
	// type-asserts http.Hijacker directly. A metrics wrapper that does not
	// implement it would fail every handshake with a 500 -- a bug this
	// codebase has already paid for once, in the access-log wrapper.
	m := New()

	var hijackable bool
	handler := m.InstrumentRoute("/ws/sessions/{sessionId}",
		http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			_, hijackable = w.(http.Hijacker)
		}))

	srv := httptest.NewServer(handler)
	defer srv.Close()

	resp, err := http.Get(srv.URL)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if !hijackable {
		t.Fatal("the instrumented response writer cannot be hijacked")
	}
}

func TestInFlightReturnsToZero(t *testing.T) {
	m := New()
	handler := m.InstrumentRoute("/api/v1/models",
		http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			if got := valueOfGauge(m, "http_requests_in_flight"); got != 1 {
				t.Errorf("in-flight during a request = %v, want 1", got)
			}
			w.WriteHeader(http.StatusOK)
		}))

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest("GET", "/api/v1/models", nil))

	// A gauge that only goes up is how a dashboard ends up showing permanent
	// load on an idle service.
	if got := valueOfGauge(m, "http_requests_in_flight"); got != 0 {
		t.Fatalf("in-flight after the request = %v, want 0", got)
	}
}

func valueOfGauge(m *Metrics, name string) float64 {
	families, err := m.Registry().Gather()
	if err != nil {
		return -1
	}
	for _, family := range families {
		if family.GetName() == name && len(family.GetMetric()) > 0 {
			return family.GetMetric()[0].GetGauge().GetValue()
		}
	}
	return -1
}
