package mlclient

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

func testLogger() *slog.Logger {
	return slog.New(slog.NewJSONHandler(io.Discard, nil))
}

func okResponse() PredictResponse {
	cls := "SWINGING_STRIKE"
	return PredictResponse{
		ModelVersion: "pitch-outcome-v1.0.0",
		Variant:      "pitching",
		Target:       "outcome_5class",
		Predictions: []Prediction{{
			PitchID: "p0",
			Probabilities: map[string]float64{
				"BALL": 0.18, "CALLED_STRIKE": 0.05,
				"SWINGING_STRIKE": 0.41, "FOUL": 0.22, "IN_PLAY": 0.14,
			},
			PredictedClass: &cls,
			ScoredQuantity: -0.0281,
			Score:          118.4,
		}},
		InferenceMs: 7.2,
	}
}

func samplePitch() PitchFeatures {
	return PitchFeatures{
		PitchID:    "p0",
		CountState: "1-2",
		Features:   map[string]any{"release_speed": 87.4, "pfx_z": 0.12},
	}
}

func TestPredictParsesAResponse(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/predict" || r.Method != http.MethodPost {
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
		}
		_ = json.NewEncoder(w).Encode(okResponse())
	}))
	defer srv.Close()

	c := New(DefaultConfig(srv.URL), testLogger())
	out, err := c.Predict(context.Background(), VariantPitching, []PitchFeatures{samplePitch()})
	if err != nil {
		t.Fatal(err)
	}
	if len(out.Predictions) != 1 || out.Predictions[0].Score != 118.4 {
		t.Fatalf("unexpected response: %+v", out)
	}
	if out.ModelVersion == "" {
		t.Error("model version missing; a stored prediction would be uninterpretable")
	}
}

func TestCorrelationIDIsForwarded(t *testing.T) {
	// One pitch's journey has to stay greppable across both services.
	var got string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Get("X-Correlation-ID")
		_ = json.NewEncoder(w).Encode(okResponse())
	}))
	defer srv.Close()

	c := New(DefaultConfig(srv.URL), testLogger())
	ctx := WithCorrelationID(context.Background(), "01JX7KABC")
	if _, err := c.Predict(ctx, VariantPitching, []PitchFeatures{samplePitch()}); err != nil {
		t.Fatal(err)
	}
	if got != "01JX7KABC" {
		t.Fatalf("correlation id not forwarded, got %q", got)
	}
}

func TestFeatureContractViolationIsItsOwnError(t *testing.T) {
	// A contract violation means training and serving disagree about what a
	// feature vector looks like. Retrying cannot fix that, so the caller
	// needs to be able to tell it apart from a transient failure.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/problem+json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{
			"title":"Feature contract violation",
			"detail":"payload does not match the contract",
			"errors":[{"field":"pitches[0].features","message":"unknown feature: realease_speed"}]
		}`))
	}))
	defer srv.Close()

	c := New(DefaultConfig(srv.URL), testLogger())
	_, err := c.Predict(context.Background(), VariantPitching, []PitchFeatures{samplePitch()})

	if !errors.Is(err, ErrFeatureContract) {
		t.Fatalf("got %v, want ErrFeatureContract", err)
	}
	// The specific field belongs in the message; that is what makes the
	// deployment problem diagnosable.
	if !contains(err.Error(), "realease_speed") {
		t.Errorf("the offending feature should appear in the error: %v", err)
	}
}

func TestServerErrorSurfacesWithItsStatus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"detail":"boom"}`))
	}))
	defer srv.Close()

	c := New(DefaultConfig(srv.URL), testLogger())
	_, err := c.Predict(context.Background(), VariantPitching, []PitchFeatures{samplePitch()})
	if err == nil {
		t.Fatal("expected an error")
	}
	if errors.Is(err, ErrFeatureContract) {
		t.Fatal("a 500 is transient and must not be reported as a contract violation")
	}
}

func TestTimeoutIsShort(t *testing.T) {
	// A long timeout here would hold a Kafka partition hostage to a hung
	// service: inference takes milliseconds, so waiting seconds is already
	// proof that something is wrong.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(300 * time.Millisecond)
		_ = json.NewEncoder(w).Encode(okResponse())
	}))
	defer srv.Close()

	cfg := DefaultConfig(srv.URL)
	cfg.Timeout = 50 * time.Millisecond
	c := New(cfg, testLogger())

	start := time.Now()
	_, err := c.Predict(context.Background(), VariantPitching, []PitchFeatures{samplePitch()})
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("expected a timeout")
	}
	if elapsed > 200*time.Millisecond {
		t.Fatalf("gave up after %v; the timeout was not enforced", elapsed)
	}
}

func TestBreakerStopsCallingADeadService(t *testing.T) {
	// This is the behaviour that keeps the pipeline moving. Without it, every
	// message pays the full retry budget to rediscover the same outage.
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	cfg := DefaultConfig(srv.URL)
	cfg.Breaker = BreakerConfig{
		FailureThreshold: 3, OpenDuration: time.Minute, HalfOpenSuccesses: 1,
	}
	c := New(cfg, testLogger())

	for i := 0; i < 10; i++ {
		_, _ = c.Predict(context.Background(), VariantPitching, []PitchFeatures{samplePitch()})
	}

	if got := calls.Load(); got != 3 {
		t.Fatalf("service was called %d times; the breaker should have stopped at 3", got)
	}
	if c.BreakerState() != StateOpen {
		t.Fatal("breaker should be open")
	}

	_, err := c.Predict(context.Background(), VariantPitching, []PitchFeatures{samplePitch()})
	if !errors.Is(err, ErrCircuitOpen) {
		t.Fatalf("got %v, want ErrCircuitOpen", err)
	}
}

func TestClientRecoversWhenTheServiceComesBack(t *testing.T) {
	var failing atomic.Bool
	failing.Store(true)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if failing.Load() {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		_ = json.NewEncoder(w).Encode(okResponse())
	}))
	defer srv.Close()

	cfg := DefaultConfig(srv.URL)
	cfg.Breaker = BreakerConfig{
		FailureThreshold: 2, OpenDuration: 50 * time.Millisecond, HalfOpenSuccesses: 1,
	}
	c := New(cfg, testLogger())

	for i := 0; i < 2; i++ {
		_, _ = c.Predict(context.Background(), VariantPitching, []PitchFeatures{samplePitch()})
	}
	if c.BreakerState() != StateOpen {
		t.Fatal("expected the breaker to trip")
	}

	failing.Store(false)
	time.Sleep(60 * time.Millisecond)

	if _, err := c.Predict(context.Background(), VariantPitching, []PitchFeatures{samplePitch()}); err != nil {
		t.Fatalf("the probe should have succeeded: %v", err)
	}
	if c.BreakerState() != StateClosed {
		t.Fatalf("breaker should have closed, got %s", c.BreakerState())
	}
}

func TestEmptyBatchDoesNotCallTheService(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
	}))
	defer srv.Close()

	c := New(DefaultConfig(srv.URL), testLogger())
	if _, err := c.Predict(context.Background(), VariantPitching, nil); err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 0 {
		t.Fatal("an empty batch should not reach the network")
	}
}

func TestOversizedBatchIsRejectedLocally(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("an oversized batch should not reach the service")
	}))
	defer srv.Close()

	cfg := DefaultConfig(srv.URL)
	cfg.MaxBatch = 2
	c := New(cfg, testLogger())

	_, err := c.Predict(context.Background(), VariantPitching,
		[]PitchFeatures{samplePitch(), samplePitch(), samplePitch()})
	if err == nil {
		t.Fatal("expected rejection")
	}
}

func TestExplainRequestsAttribution(t *testing.T) {
	var body predictRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&body)
		_ = json.NewEncoder(w).Encode(okResponse())
	}))
	defer srv.Close()

	c := New(DefaultConfig(srv.URL), testLogger())
	if _, err := c.Explain(context.Background(), VariantPitching, samplePitch(), 3); err != nil {
		t.Fatal(err)
	}
	if !body.Explain || body.TopN != 3 {
		t.Fatalf("explain flags not sent: %+v", body)
	}
}

func TestPredictDoesNotRequestAttribution(t *testing.T) {
	// The live path must not pay for SHAP it does not use.
	var body predictRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&body)
		_ = json.NewEncoder(w).Encode(okResponse())
	}))
	defer srv.Close()

	c := New(DefaultConfig(srv.URL), testLogger())
	if _, err := c.Predict(context.Background(), VariantPitching, []PitchFeatures{samplePitch()}); err != nil {
		t.Fatal(err)
	}
	if body.Explain {
		t.Fatal("the prediction path requested an explanation")
	}
}

func contains(haystack, needle string) bool {
	return len(haystack) >= len(needle) &&
		(haystack == needle || len(needle) == 0 ||
			indexOf(haystack, needle) >= 0)
}

func indexOf(haystack, needle string) int {
	for i := 0; i+len(needle) <= len(haystack); i++ {
		if haystack[i:i+len(needle)] == needle {
			return i
		}
	}
	return -1
}
