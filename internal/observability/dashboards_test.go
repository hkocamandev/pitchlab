package observability

import (
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// The dashboards are committed JSON, which means they can go stale silently:
// a renamed metric breaks a panel and nothing fails until someone opens
// Grafana and sees an empty graph. These tests close that gap by checking the
// queries against the registry the services actually export.

type dashboard struct {
	UID    string  `json:"uid"`
	Title  string  `json:"title"`
	Panels []panel `json:"panels"`
}

type panel struct {
	Title       string `json:"title"`
	Description string `json:"description"`
	Targets     []struct {
		Expr string `json:"expr"`
	} `json:"targets"`
}

func loadDashboards(t *testing.T) map[string]dashboard {
	t.Helper()

	dir := filepath.Join("..", "..", "deploy", "grafana", "dashboards")
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read dashboards: %v", err)
	}

	out := map[string]dashboard{}
	for _, e := range entries {
		if !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		raw, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			t.Fatalf("read %s: %v", e.Name(), err)
		}
		var d dashboard
		if err := json.Unmarshal(raw, &d); err != nil {
			t.Fatalf("parse %s: %v", e.Name(), err)
		}
		out[e.Name()] = d
	}
	return out
}

func TestAllFiveDashboardsArePresentAndValid(t *testing.T) {
	dashboards := loadDashboards(t)

	want := []string{
		"cache.json", "event-pipeline.json", "live-channel.json",
		"model-serving.json", "system-health.json",
	}
	got := make([]string, 0, len(dashboards))
	for name := range dashboards {
		got = append(got, name)
	}
	sort.Strings(got)

	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("dashboards are %v, want %v", got, want)
	}

	seen := map[string]string{}
	for name, d := range dashboards {
		if d.UID == "" || d.Title == "" {
			t.Errorf("%s has no uid or title", name)
		}
		// A duplicate uid makes provisioning overwrite one dashboard with
		// another, which looks like the file was ignored.
		if prev, ok := seen[d.UID]; ok {
			t.Errorf("%s and %s share the uid %q", prev, name, d.UID)
		}
		seen[d.UID] = name

		if len(d.Panels) == 0 {
			t.Errorf("%s has no panels", name)
		}
	}
}

// metricName pulls the metric names out of a PromQL expression.
var metricRef = regexp.MustCompile(`\b([a-z_][a-z0-9_]*)(?:_bucket|_count|_sum)?\b`)

// promQLKeywords are not metric names.
var promQLKeywords = map[string]bool{
	"sum": true, "rate": true, "by": true, "le": true, "increase": true,
	"histogram_quantile": true, "topk": true, "clamp_min": true, "avg": true,
	"scalar": true, "vector": true,
	"max": true, "min": true, "count": true, "job": true, "and": true,
	"or": true, "without": true, "on": true, "group_left": true, "irate": true,
	"topic": true, "partition": true, "route": true, "result": true,
	"reason": true, "operation": true, "message_type": true, "status": true,
	"error_class": true, "model_variant": true, "model_version": true,
	"predicted_class": true, "feature_name": true, "query_name": true,
}

// --- T10.6 every panel queries something that exists ----------------------

func TestEveryPanelQueriesAnExportedMetric(t *testing.T) {
	m := New()
	m.TrackWebSocketConnections(func() int64 { return 0 })
	m.TrackBreaker(func() string { return "closed" })
	exercise(m)

	exported := map[string]bool{}
	for _, family := range gather(t, m) {
		exported[family.GetName()] = true
	}
	// Exported by the inference service, which is a separate registry in a
	// separate language. Listed explicitly so a rename there is still caught
	// here rather than only by an empty Grafana panel.
	for _, name := range []string{
		"ml_predict_duration_seconds", "ml_predictions_total",
		"ml_predicted_class_total", "ml_feature_null_ratio",
		"ml_model_load_timestamp_seconds", "ml_explain_duration_seconds",
		"ml_request_errors_total",
	} {
		exported[name] = true
	}
	// Prometheus itself provides this one.
	exported["up"] = true
	// The process collector produces these on Linux, where the services run in
	// containers, and nothing on macOS. They are listed rather than gathered
	// so the test asserts the same thing on both.
	for _, name := range []string{
		"process_resident_memory_bytes", "process_open_fds", "process_cpu_seconds_total",
	} {
		exported[name] = true
	}

	for name, d := range loadDashboards(t) {
		for _, p := range d.Panels {
			if len(p.Targets) == 0 {
				t.Errorf("%s: panel %q has no query, so it can only render empty",
					name, p.Title)
			}
			for _, target := range p.Targets {
				for _, ref := range metricsIn(target.Expr) {
					if !exported[ref] {
						t.Errorf("%s: panel %q queries %q, which nothing exports",
							name, p.Title, ref)
					}
				}
			}
		}
	}
}

func metricsIn(expr string) []string {
	var out []string
	// Label values are quoted; stripping them first keeps a value like "5.."
	// or a topic name from being read as a metric.
	stripped := regexp.MustCompile(`"[^"]*"`).ReplaceAllString(expr, `""`)

	for _, match := range metricRef.FindAllStringSubmatch(stripped, -1) {
		name := match[1]
		if promQLKeywords[name] || name == "" {
			continue
		}
		// Histogram suffixes are queried, not exported.
		name = strings.TrimSuffix(name, "_bucket")
		out = append(out, name)
	}
	return out
}

func TestDriftPanelsExistAndSayWhatTheyAreFor(t *testing.T) {
	dashboards := loadDashboards(t)

	serving, ok := dashboards["model-serving.json"]
	if !ok {
		t.Fatal("the model serving dashboard is missing")
	}

	// These two panels are the answer to "how would we notice the 2026 rule
	// change in production". A dashboard that has them but does not say so
	// leaves the next person to work it out from the query.
	var found int
	for _, p := range serving.Panels {
		if strings.Contains(strings.ToUpper(p.Description), "DRIFT INDICATOR") {
			found++
		}
	}
	if found < 2 {
		t.Fatalf("model serving has %d panels labelled as drift indicators, want 2", found)
	}
}

func TestPanelsAreDocumented(t *testing.T) {
	// Not every panel needs prose, but a dashboard where nothing is explained
	// is a dashboard only its author can read.
	for name, d := range loadDashboards(t) {
		var documented int
		for _, p := range d.Panels {
			if strings.TrimSpace(p.Description) != "" {
				documented++
			}
		}
		if documented == 0 {
			t.Errorf("%s explains none of its %d panels", name, len(d.Panels))
		}
	}
}
