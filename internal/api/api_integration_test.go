//go:build integration

package api_test

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/hkocamandev/pitchlab/internal/api"
	"github.com/hkocamandev/pitchlab/internal/config"
	"github.com/hkocamandev/pitchlab/internal/db"
	"github.com/hkocamandev/pitchlab/internal/db/dbgen"
)

// The API is exercised against a real database rather than a mocked store.
// Most of what these endpoints do is shape a query and interpret its result;
// a mock would assert that the test author remembered the schema, which is
// the part most likely to be wrong.

func newTestServer(t *testing.T) (*httptest.Server, *db.Store) {
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
	srv := httptest.NewServer(api.New(cfg, store, log).Routes())
	t.Cleanup(srv.Close)

	return srv, store
}

func truncate(t *testing.T, store *db.Store) {
	t.Helper()
	_, err := store.Pool().Exec(context.Background(), `
		TRUNCATE performance_anomalies, pitch_predictions, pitch_measurements,
		         pitches, sessions, model_versions, devices, athletes
		RESTART IDENTITY CASCADE`)
	if err != nil {
		t.Fatalf("truncate: %v", err)
	}
}

func get(t *testing.T, srv *httptest.Server, path string) (*http.Response, []byte) {
	t.Helper()
	resp, err := srv.Client().Get(srv.URL + path)
	if err != nil {
		t.Fatalf("GET %s: %v", path, err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return resp, body
}

func post(t *testing.T, srv *httptest.Server, path string) (*http.Response, []byte) {
	t.Helper()
	resp, err := srv.Client().Post(srv.URL+path, "application/json", nil)
	if err != nil {
		t.Fatalf("POST %s: %v", path, err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return resp, body
}

func ptr[T any](v T) *T { return &v }

// --- fixtures --------------------------------------------------------------

type fixture struct {
	pitcher dbgen.Athlete
	batter  dbgen.Athlete
	device  dbgen.Device
	session dbgen.Session
	model   dbgen.ModelVersion
	pitches []dbgen.Pitch
}

func seed(t *testing.T, store *db.Store, nPitches int) fixture {
	t.Helper()
	ctx := context.Background()
	truncate(t, store)

	f := fixture{}
	var err error

	f.pitcher, err = store.UpsertAthlete(ctx, dbgen.UpsertAthleteParams{
		ID: db.NewID(), MlbamID: 669373, FullName: "Doe, John",
		Throws: ptr("R"), Bats: ptr("L"), PrimaryRole: "PITCHER",
	})
	if err != nil {
		t.Fatal(err)
	}
	f.batter, err = store.UpsertAthlete(ctx, dbgen.UpsertAthleteParams{
		ID: db.NewID(), MlbamID: 665742, FullName: "Roe, Alex",
		Throws: ptr("L"), Bats: ptr("L"), PrimaryRole: "BATTER",
	})
	if err != nil {
		t.Fatal(err)
	}
	f.device, err = store.UpsertDevice(ctx, dbgen.UpsertDeviceParams{
		ID: db.NewID(), DeviceKey: "replay-sim-01",
		DeviceKind: "REPLAY_SIMULATOR", CalibrationProfile: []byte(`{}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	f.session, err = store.UpsertSession(ctx, dbgen.UpsertSessionParams{
		ID: db.NewID(), ExternalSessionUid: "747123:669373",
		PitcherAthleteID: f.pitcher.ID, DeviceID: &f.device.ID,
		ExternalGameRef: ptr("747123"),
		StartedAt:       time.Now().UTC().Add(-2 * time.Hour).Truncate(time.Second),
		Status:          "ACTIVE", IsSynthetic: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	f.model, err = store.InsertModelVersion(ctx, dbgen.InsertModelVersionParams{
		ID: db.NewID(), Version: "pitch-outcome-v1.0.0", Variant: "pitching",
		Target: "outcome_5class", Algorithm: "lightgbm-multiclass",
		FeatureColumns: []byte(`["release_speed","pfx_z"]`),
		ClassLabels:    []byte(`["BALL","CALLED_STRIKE","SWINGING_STRIKE","FOUL","IN_PLAY"]`),
		RunValueTable:  []byte(`{"values":{}}`),
		ScoreMu:        -0.001, ScoreSigma: 0.043,
		TrainPeriodStart: time.Date(2023, 3, 30, 0, 0, 0, 0, time.UTC),
		TrainPeriodEnd:   time.Date(2024, 9, 29, 0, 0, 0, 0, time.UTC),
		DataSnapshot:     "statcast_full_20260911.parquet",
		Metrics:          []byte(`{"test_log_loss":0.9725}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.ActivateModelVersion(ctx, f.model.ID); err != nil {
		t.Fatal(err)
	}

	outcomes := []string{"BALL", "CALLED_STRIKE", "SWINGING_STRIKE", "FOUL", "IN_PLAY"}
	base := time.Now().UTC().Add(-2 * time.Hour).Truncate(time.Millisecond)

	for i := 0; i < nPitches; i++ {
		p, err := store.InsertPitch(ctx, dbgen.InsertPitchParams{
			ID: db.NewID(), ExternalPitchUid: fmt.Sprintf("747123:%d:1", i+1),
			SessionID: f.session.ID, PitcherAthleteID: f.pitcher.ID,
			BatterAthleteID: &f.batter.ID,
			AtBatNumber:     int16(i + 1), PitchNumber: 1,
			SessionPitchIndex: int32(i + 1),
			ThrownAt:          base.Add(time.Duration(i) * 20 * time.Second),
			Balls:             int16(i % 4), Strikes: int16(i % 3), OutsWhenUp: int16(i % 3),
			Inning: int16(i/8 + 1), Stand: "L", PThrows: "R",
			PitchType:        ptr([]string{"FF", "SL", "CH"}[i%3]),
			PitchName:        ptr("Slider"),
			ActualOutcome:    ptr(outcomes[i%len(outcomes)]),
			PredictionStatus: "OK",
		})
		if err != nil {
			t.Fatalf("insert pitch %d: %v", i, err)
		}
		f.pitches = append(f.pitches, p)

		speed := float32(88 + i%12)
		spin := float32(2200 + i*3)
		if _, err := store.UpsertPitchMeasurement(ctx, dbgen.UpsertPitchMeasurementParams{
			PitchID: p.ID, DeviceID: &f.device.ID,
			ReleaseSpeed: &speed, ReleaseSpinRate: &spin,
			PfxXArm: ptr(float32(0.63)), PfxZ: ptr(float32(1.42)),
			InZone: ptr(i%2 == 0), NormalizationVersion: 1, MeasurementQuality: "OK",
		}); err != nil {
			t.Fatal(err)
		}

		if _, err := store.UpsertPitchPrediction(ctx, dbgen.UpsertPitchPredictionParams{
			ID: db.NewID(), PitchID: p.ID, ModelVersionID: f.model.ID,
			Probabilities:  []byte(`{"BALL":0.18,"CALLED_STRIKE":0.05,"SWINGING_STRIKE":0.41,"FOUL":0.22,"IN_PLAY":0.14}`),
			PredictedClass: ptr("SWINGING_STRIKE"),
			ScoredQuantity: -0.0281, Score: float32(100 + i%20),
		}); err != nil {
			t.Fatal(err)
		}
	}

	if _, err := store.RecomputeSessionAggregates(ctx, f.session.ID); err != nil {
		t.Fatal(err)
	}
	return f
}

// --- health ---------------------------------------------------------------

func TestHealthzChecksNothing(t *testing.T) {
	// Liveness that probes the database turns a brief slowdown into an
	// outage: every instance reports unhealthy, the orchestrator restarts
	// them all at once, and the cold starts finish the job.
	srv, _ := newTestServer(t)

	resp, body := get(t, srv, "/healthz")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("got %d, want 200: %s", resp.StatusCode, body)
	}
}

func TestReadyzReportsPostgres(t *testing.T) {
	srv, _ := newTestServer(t)

	resp, body := get(t, srv, "/readyz")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("got %d, want 200: %s", resp.StatusCode, body)
	}

	var out struct {
		Status string `json:"status"`
		Checks map[string]struct {
			Status string `json:"status"`
		} `json:"checks"`
		Degraded []string `json:"degraded"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatal(err)
	}
	if out.Checks["postgres"].Status != "ok" {
		t.Fatalf("postgres should be ok: %s", body)
	}
	// Redis is not attached in this configuration, and that is not an error:
	// it is a cache, never the record of truth.
	if len(out.Degraded) != 0 {
		t.Errorf("nothing should be degraded: %v", out.Degraded)
	}
}

// --- athletes -------------------------------------------------------------

func TestGetAthleteReturnsTotals(t *testing.T) {
	srv, store := newTestServer(t)
	f := seed(t, store, 5)

	resp, body := get(t, srv, "/api/v1/athletes/"+f.pitcher.ID.String())
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("got %d: %s", resp.StatusCode, body)
	}

	var out api.AthleteDetailDTO
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatal(err)
	}
	if out.FullName != "Doe, John" {
		t.Errorf("name: %q", out.FullName)
	}
	if out.Totals.PitchCount != 5 || out.Totals.SessionCount != 1 {
		t.Errorf("totals: %+v", out.Totals)
	}
	if out.Totals.FirstPitchAt == nil || out.Totals.LastPitchAt == nil {
		t.Error("pitch timestamps should be populated when pitches exist")
	}
}

func TestGetAthleteWithNoPitchesOmitsTimestamps(t *testing.T) {
	// min()/max() over an empty set are NULL. Reporting the zero time would
	// claim the athlete pitched in year 1.
	srv, store := newTestServer(t)
	truncate(t, store)

	a, err := store.UpsertAthlete(context.Background(), dbgen.UpsertAthleteParams{
		ID: db.NewID(), MlbamID: 1, FullName: "New, Player", PrimaryRole: "UNKNOWN",
	})
	if err != nil {
		t.Fatal(err)
	}

	resp, body := get(t, srv, "/api/v1/athletes/"+a.ID.String())
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("got %d: %s", resp.StatusCode, body)
	}

	var out api.AthleteDetailDTO
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatal(err)
	}
	if out.Totals.FirstPitchAt != nil || out.Totals.LastPitchAt != nil {
		t.Fatalf("expected null timestamps, got %+v", out.Totals)
	}
}

func TestGetUnknownAthleteIs404(t *testing.T) {
	srv, store := newTestServer(t)
	truncate(t, store)

	resp, body := get(t, srv, "/api/v1/athletes/"+uuid.New().String())
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("got %d: %s", resp.StatusCode, body)
	}
	assertProblem(t, body, http.StatusNotFound)
}

func TestMalformedUUIDIs400NotServerError(t *testing.T) {
	srv, store := newTestServer(t)
	truncate(t, store)

	resp, body := get(t, srv, "/api/v1/athletes/not-a-uuid")
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("got %d: %s", resp.StatusCode, body)
	}
	assertProblem(t, body, http.StatusBadRequest)
}

func TestAthleteAnalyticsSuppressesAveragesWithoutData(t *testing.T) {
	srv, store := newTestServer(t)
	truncate(t, store)

	a, err := store.UpsertAthlete(context.Background(), dbgen.UpsertAthleteParams{
		ID: db.NewID(), MlbamID: 1, FullName: "New, Player", PrimaryRole: "PITCHER",
	})
	if err != nil {
		t.Fatal(err)
	}

	_, body := get(t, srv, "/api/v1/athletes/"+a.ID.String()+"/analytics")
	var out api.AnalyticsDTO
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatal(err)
	}
	if out.Summary.PitchCount != 0 {
		t.Fatalf("expected no pitches, got %d", out.Summary.PitchCount)
	}
	// SQL returns 0 for an empty group. A zero average is not a measurement.
	if out.Summary.AvgReleaseSpeed != nil || out.Summary.WhiffRate != nil {
		t.Fatalf("averages must be null, not zero: %+v", out.Summary)
	}
}

func TestAthleteAnalyticsComputesArsenalAndWhiffRate(t *testing.T) {
	srv, store := newTestServer(t)
	f := seed(t, store, 15)

	resp, body := get(t, srv, "/api/v1/athletes/"+f.pitcher.ID.String()+"/analytics")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("got %d: %s", resp.StatusCode, body)
	}

	var out api.AnalyticsDTO
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatal(err)
	}
	if out.Summary.PitchCount != 15 {
		t.Errorf("pitch count: %d", out.Summary.PitchCount)
	}
	if len(out.PitchTypeDistribution) != 3 {
		t.Errorf("expected 3 pitch types, got %d", len(out.PitchTypeDistribution))
	}

	var usage float64
	for _, d := range out.PitchTypeDistribution {
		usage += d.UsagePct
	}
	if usage < 0.999 || usage > 1.001 {
		t.Errorf("usage percentages should sum to 1, got %.4f", usage)
	}

	// The fixture cycles five outcomes evenly, so of the three swing
	// outcomes exactly one is a whiff.
	if out.Summary.WhiffRate == nil {
		t.Fatal("whiff rate missing")
	}
	if got := *out.Summary.WhiffRate; got < 0.32 || got > 0.34 {
		t.Errorf("whiff rate %.4f, want ~1/3 given the fixture", got)
	}

	if cc := resp.Header.Get("Cache-Control"); cc == "" {
		t.Error("analytics should advertise cacheability")
	}
}

// --- pitches --------------------------------------------------------------

func TestPitchSearchRequiresAPitcher(t *testing.T) {
	// Without the filter this is a sequential scan over millions of rows.
	// Making the expensive query impossible to express beats optimizing it
	// after someone finds it in production.
	srv, store := newTestServer(t)
	seed(t, store, 3)

	resp, body := get(t, srv, "/api/v1/pitches")
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("got %d, want 400: %s", resp.StatusCode, body)
	}

	var problem struct {
		Errors []struct {
			Field string `json:"field"`
		} `json:"errors"`
	}
	if err := json.Unmarshal(body, &problem); err != nil {
		t.Fatal(err)
	}
	if len(problem.Errors) == 0 || problem.Errors[0].Field != "pitcher_id" {
		t.Fatalf("the error should name the missing filter: %s", body)
	}
}

func TestSessionTimelineIsOrderedAndPaginated(t *testing.T) {
	srv, store := newTestServer(t)
	f := seed(t, store, 25)

	var seen []int32
	path := "/api/v1/sessions/" + f.session.ID.String() + "/pitches?limit=10"

	for page := 0; page < 5; page++ {
		resp, body := get(t, srv, path)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("got %d: %s", resp.StatusCode, body)
		}
		var out struct {
			Data []api.PitchDTO `json:"data"`
			Page struct {
				NextCursor string `json:"next_cursor"`
				HasMore    bool   `json:"has_more"`
			} `json:"page"`
		}
		if err := json.Unmarshal(body, &out); err != nil {
			t.Fatal(err)
		}
		for _, p := range out.Data {
			seen = append(seen, p.SessionPitchIndex)
		}
		if !out.Page.HasMore {
			break
		}
		path = "/api/v1/sessions/" + f.session.ID.String() +
			"/pitches?limit=10&cursor=" + out.Page.NextCursor
	}

	if len(seen) != 25 {
		t.Fatalf("paging returned %d pitches, want 25", len(seen))
	}
	for i, idx := range seen {
		if idx != int32(i+1) {
			t.Fatalf("at position %d got index %d; paging skipped or repeated", i, idx)
		}
	}
}

func TestPitchIncludesPredictionAndGroundTruth(t *testing.T) {
	// The dashboard shows the prediction next to what actually happened.
	// Presenting only the prediction would be the dishonest version.
	srv, store := newTestServer(t)
	f := seed(t, store, 1)

	resp, body := get(t, srv, "/api/v1/pitches/"+f.pitches[0].ID.String())
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("got %d: %s", resp.StatusCode, body)
	}

	var out api.PitchDTO
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatal(err)
	}
	if out.ActualOutcome == nil {
		t.Error("ground truth missing")
	}
	if len(out.Predictions) != 1 {
		t.Fatalf("expected 1 prediction, got %d", len(out.Predictions))
	}
	p := out.Predictions[0]
	if p.Probabilities["SWINGING_STRIKE"] == 0 {
		t.Error("probability vector missing")
	}
	if p.ModelVersion == "" {
		t.Error("a prediction without its model version is uninterpretable")
	}
	if out.Measurement == nil || out.Measurement.ReleaseSpeed == nil {
		t.Error("measurement missing")
	}
}

func TestIncludeParameterTrimsTheResponse(t *testing.T) {
	srv, store := newTestServer(t)
	f := seed(t, store, 3)

	_, body := get(t, srv,
		"/api/v1/sessions/"+f.session.ID.String()+"/pitches?include=measurement")

	var out struct {
		Data []api.PitchDTO `json:"data"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatal(err)
	}
	if len(out.Data) == 0 {
		t.Fatal("no pitches")
	}
	if out.Data[0].Measurement == nil {
		t.Error("measurement was requested and should be present")
	}
	if len(out.Data[0].Predictions) != 0 {
		t.Error("predictions were not requested and should be omitted")
	}
}

// --- sessions -------------------------------------------------------------

func TestSessionDetailExposesSyntheticFlag(t *testing.T) {
	// A viewer must be able to tell replayed data from real data without
	// reading the source.
	srv, store := newTestServer(t)
	f := seed(t, store, 4)

	resp, body := get(t, srv, "/api/v1/sessions/"+f.session.ID.String())
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("got %d: %s", resp.StatusCode, body)
	}

	var out api.SessionDetailDTO
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatal(err)
	}
	if !out.IsSynthetic {
		t.Error("replayed session must be marked synthetic")
	}
	if out.Pitcher == nil || out.Pitcher.FullName == "" {
		t.Error("pitcher reference missing")
	}
	if out.Metrics.PitchCount != 4 {
		t.Errorf("pitch count %d, want 4", out.Metrics.PitchCount)
	}
	if len(out.Metrics.OutcomeDistribution) == 0 {
		t.Error("outcome distribution missing")
	}
}

func TestCloseSessionIsIdempotentAndDistinguishesTheSecondCall(t *testing.T) {
	srv, store := newTestServer(t)
	f := seed(t, store, 4)

	resp, body := post(t, srv, "/api/v1/sessions/"+f.session.ID.String()+"/close")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("first close: got %d: %s", resp.StatusCode, body)
	}
	var closed api.SessionDTO
	if err := json.Unmarshal(body, &closed); err != nil {
		t.Fatal(err)
	}
	if closed.Status != "COMPLETED" || closed.EndedAt == nil {
		t.Fatalf("session not closed: %+v", closed)
	}

	// A repeat is a conflict, not a silent success and not a 404: the caller
	// deserves to know the difference between "gone" and "already done".
	resp, body = post(t, srv, "/api/v1/sessions/"+f.session.ID.String()+"/close")
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("second close: got %d, want 409: %s", resp.StatusCode, body)
	}

	resp, _ = post(t, srv, "/api/v1/sessions/"+uuid.New().String()+"/close")
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("unknown session: got %d, want 404", resp.StatusCode)
	}
}

// --- models ---------------------------------------------------------------

func TestModelsEndpointPublishesTheModelCard(t *testing.T) {
	// Publishing the card is what lets the dashboard footer say which model
	// produced a score and how it scored against its baseline, instead of
	// presenting predictions as certainties.
	srv, store := newTestServer(t)
	seed(t, store, 1)

	resp, body := get(t, srv, "/api/v1/models?active=true")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("got %d: %s", resp.StatusCode, body)
	}

	var out struct {
		Data []api.ModelVersionDTO `json:"data"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatal(err)
	}
	if len(out.Data) != 1 {
		t.Fatalf("expected 1 active model, got %d", len(out.Data))
	}
	m := out.Data[0]
	if m.FeatureCount != 2 {
		t.Errorf("feature count %d", m.FeatureCount)
	}
	if len(m.ClassLabels) != 5 {
		t.Errorf("class labels: %v", m.ClassLabels)
	}
	if m.DataSnapshot == "" || m.TrainPeriod.Start == nil {
		t.Error("provenance missing from the model card")
	}
	if len(m.Metrics) == 0 {
		t.Error("metrics missing; the card should be self-describing")
	}
}

// --- anomalies ------------------------------------------------------------

func TestAcknowledgeAnomalyIsIdempotentAndDistinguishesTheSecondCall(t *testing.T) {
	srv, store := newTestServer(t)
	f := seed(t, store, 2)
	ctx := context.Background()

	anomaly, err := store.InsertAnomaly(ctx, dbgen.InsertAnomalyParams{
		ID: db.NewID(), AthleteID: f.pitcher.ID, SessionID: f.session.ID,
		Metric: "release_speed", PitchType: ptr("FF"),
		ObservedValue: 92.4, BaselineMedian: 94.2, RobustSigma: 0.53,
		ZRobust: -3.4, Direction: "DECREASE", Severity: "HIGH",
		WindowSessions: 10, DetectorVersion: "robust-mad-v1",
	})
	if err != nil {
		t.Fatal(err)
	}

	resp, body := post(t, srv, "/api/v1/anomalies/"+anomaly.ID.String()+"/acknowledge")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("got %d: %s", resp.StatusCode, body)
	}
	var out api.AnomalyDTO
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatal(err)
	}
	if out.AcknowledgedAt == nil {
		t.Fatal("acknowledgement not recorded")
	}

	resp, _ = post(t, srv, "/api/v1/anomalies/"+anomaly.ID.String()+"/acknowledge")
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("second acknowledge: got %d, want 409", resp.StatusCode)
	}
}

func TestAnomalyListCarriesItsOwnBaseline(t *testing.T) {
	// The baseline is stored with the finding so "why was this flagged" is
	// answerable from the row alone; recomputing it later gives a different
	// answer because the trailing window has moved.
	srv, store := newTestServer(t)
	f := seed(t, store, 2)

	if _, err := store.InsertAnomaly(context.Background(), dbgen.InsertAnomalyParams{
		ID: db.NewID(), AthleteID: f.pitcher.ID, SessionID: f.session.ID,
		Metric: "release_speed", PitchType: ptr("FF"),
		ObservedValue: 92.4, BaselineMedian: 94.2, RobustSigma: 0.53,
		ZRobust: -3.4, Direction: "DECREASE", Severity: "HIGH",
		WindowSessions: 10, DetectorVersion: "robust-mad-v1",
	}); err != nil {
		t.Fatal(err)
	}

	_, body := get(t, srv, "/api/v1/athletes/"+f.pitcher.ID.String()+"/anomalies")
	var out struct {
		Data []api.AnomalyDTO `json:"data"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatal(err)
	}
	if len(out.Data) != 1 {
		t.Fatalf("expected 1 anomaly, got %d", len(out.Data))
	}
	a := out.Data[0]
	if a.BaselineMedian == 0 || a.RobustSigma == 0 || a.ZRobust == 0 {
		t.Fatalf("the finding must carry its baseline: %+v", a)
	}
	if a.DetectorVersion == "" {
		t.Error("detector version missing")
	}
}

// --- error contract -------------------------------------------------------

func TestEveryErrorIsAProblemDocumentWithARequestID(t *testing.T) {
	srv, store := newTestServer(t)
	truncate(t, store)

	cases := []struct {
		path string
		want int
	}{
		{"/api/v1/athletes/not-a-uuid", http.StatusBadRequest},
		{"/api/v1/athletes/" + uuid.New().String(), http.StatusNotFound},
		{"/api/v1/pitches", http.StatusBadRequest},
		{"/api/v1/athletes?limit=99999", http.StatusBadRequest},
		{"/api/v1/athletes?cursor=garbage!!", http.StatusBadRequest},
		{"/api/v1/sessions?status=DELETED", http.StatusBadRequest},
	}

	for _, tc := range cases {
		resp, body := get(t, srv, tc.path)
		if resp.StatusCode != tc.want {
			t.Errorf("%s: got %d, want %d: %s", tc.path, resp.StatusCode, tc.want, body)
			continue
		}
		if ct := resp.Header.Get("Content-Type"); ct != "application/problem+json" {
			t.Errorf("%s: content type %q", tc.path, ct)
		}
		if resp.Header.Get("X-Request-ID") == "" {
			t.Errorf("%s: response carries no request id", tc.path)
		}
		assertProblem(t, body, tc.want)
	}
}

func assertProblem(t *testing.T, body []byte, status int) {
	t.Helper()
	var p struct {
		Type      string `json:"type"`
		Title     string `json:"title"`
		Status    int    `json:"status"`
		RequestID string `json:"request_id"`
	}
	if err := json.Unmarshal(body, &p); err != nil {
		t.Fatalf("not a problem document: %v (%s)", err, body)
	}
	if p.Status != status {
		t.Errorf("problem status %d, want %d", p.Status, status)
	}
	if p.Type == "" || p.Title == "" {
		t.Errorf("problem document incomplete: %s", body)
	}
	if p.RequestID == "" {
		t.Errorf("problem document has no request id; a report could not be traced")
	}
}
