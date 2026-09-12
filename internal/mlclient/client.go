// Package mlclient talks to the Python inference service.
//
// The call is synchronous, and that is deliberate: the processor cannot
// persist a pitch without its prediction, because the prediction is part of
// what goes to the dashboard. Putting a topic between them would be
// asynchrony for its own sake -- one side would still be waiting, just with
// more moving parts.
//
// What is not deliberate is the pipeline stopping when inference is down. A
// pitch is persisted either way, marked as having no prediction, and can be
// backfilled later from data that is already in PostgreSQL.
package mlclient

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"time"
)

// Variant names the model to score with.
type Variant string

const (
	VariantPitching Variant = "pitching"
	VariantStuff    Variant = "stuff"
)

// PitchFeatures is one pitch's feature vector.
type PitchFeatures struct {
	PitchID    string         `json:"pitch_id,omitempty"`
	CountState string         `json:"count_state,omitempty"`
	Features   map[string]any `json:"features"`
}

type predictRequest struct {
	Variant Variant         `json:"variant"`
	Pitches []PitchFeatures `json:"pitches"`
	Explain bool            `json:"explain,omitempty"`
	TopN    int             `json:"top_n,omitempty"`
}

// Prediction is one scored pitch.
type Prediction struct {
	PitchID string `json:"pitch_id"`

	// The outcome model fills Probabilities and PredictedClass; the stuff
	// model fills WhiffProbability. Never both.
	Probabilities    map[string]float64 `json:"probabilities"`
	PredictedClass   *string            `json:"predicted_class"`
	WhiffProbability *float64           `json:"whiff_probability"`

	ScoredQuantity float64      `json:"scored_quantity"`
	Score          float64      `json:"score"`
	Explanation    *Explanation `json:"explanation"`
}

// Explanation is a SHAP attribution, in the model's margin space.
type Explanation struct {
	TargetClass          string         `json:"target_class"`
	Space                string         `json:"space"`
	BaseValue            float64        `json:"base_value"`
	PredictedValue       float64        `json:"predicted_value"`
	PredictedProbability float64        `json:"predicted_probability"`
	Contributions        []Contribution `json:"contributions"`
	OtherContribution    float64        `json:"other_contribution"`
}

// Contribution is one feature's share of a prediction.
type Contribution struct {
	Feature string   `json:"feature"`
	Value   *float64 `json:"value"`
	SHAP    float64  `json:"shap"`
}

// PredictResponse is a scored batch.
type PredictResponse struct {
	ModelVersion string       `json:"model_version"`
	Variant      string       `json:"variant"`
	Target       string       `json:"target"`
	Predictions  []Prediction `json:"predictions"`
	InferenceMs  float64      `json:"inference_ms"`
}

// ModelInfo is the published model card.
type ModelInfo struct {
	Version        string         `json:"version"`
	Variant        string         `json:"variant"`
	Target         string         `json:"target"`
	Algorithm      string         `json:"algorithm"`
	FeatureColumns []string       `json:"feature_columns"`
	FeatureCount   int            `json:"feature_count"`
	ClassLabels    []string       `json:"class_labels"`
	ScoreMu        float64        `json:"score_mu"`
	ScoreSigma     float64        `json:"score_sigma"`
	Metrics        map[string]any `json:"metrics"`
	GitSHA         *string        `json:"git_sha"`
}

// Config configures the client.
type Config struct {
	BaseURL string
	Timeout time.Duration
	// MaxBatch bounds a single request. The service caps it too; this avoids
	// a round trip to be told so.
	MaxBatch int
	Breaker  BreakerConfig
}

// DefaultConfig returns sensible defaults.
func DefaultConfig(baseURL string) Config {
	return Config{
		BaseURL: baseURL,
		// Short by intent: inference is a few milliseconds. A long timeout
		// here would hold a Kafka partition hostage to a hung service.
		Timeout:  2 * time.Second,
		MaxBatch: 500,
		Breaker:  DefaultBreakerConfig(),
	}
}

// Client is an HTTP client for the inference service.
type Client struct {
	cfg     Config
	http    *http.Client
	breaker *Breaker
	log     *slog.Logger
}

// ErrFeatureContract reports that the service rejected the payload as not
// matching the model's feature contract.
//
// This is never retried. It means training and serving disagree about what a
// feature vector looks like, which no amount of waiting fixes.
var ErrFeatureContract = errors.New("feature contract violation")

// New builds a client.
func New(cfg Config, log *slog.Logger) *Client {
	if cfg.Timeout <= 0 {
		cfg.Timeout = 2 * time.Second
	}
	if cfg.MaxBatch <= 0 {
		cfg.MaxBatch = 500
	}

	breaker := NewBreaker(cfg.Breaker)
	breaker.OnStateChange(func(from, to State) {
		log.Warn("ml circuit breaker changed state",
			"from", from.String(), "to", to.String())
	})

	return &Client{
		cfg:     cfg,
		http:    &http.Client{Timeout: cfg.Timeout},
		breaker: breaker,
		log:     log,
	}
}

// BreakerState exposes the breaker for metrics.
func (c *Client) BreakerState() State { return c.breaker.State() }

// Predict scores a batch of pitches.
func (c *Client) Predict(
	ctx context.Context, variant Variant, pitches []PitchFeatures,
) (*PredictResponse, error) {
	if len(pitches) == 0 {
		return &PredictResponse{}, nil
	}
	if len(pitches) > c.cfg.MaxBatch {
		return nil, fmt.Errorf("batch of %d exceeds the limit of %d",
			len(pitches), c.cfg.MaxBatch)
	}

	var out *PredictResponse
	err := c.breaker.Do(func() error {
		resp, err := c.post(ctx, "/v1/predict", predictRequest{
			Variant: variant, Pitches: pitches,
		})
		if err != nil {
			return err
		}
		out = resp
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// Explain scores one pitch and asks for its attribution.
//
// Separate from Predict because SHAP costs materially more, and the live path
// needs only the number. This runs when a user opens a pitch.
func (c *Client) Explain(
	ctx context.Context, variant Variant, pitch PitchFeatures, topN int,
) (*PredictResponse, error) {
	if topN <= 0 {
		topN = 5
	}
	var out *PredictResponse
	err := c.breaker.Do(func() error {
		resp, err := c.post(ctx, "/v1/predict", predictRequest{
			Variant: variant, Pitches: []PitchFeatures{pitch},
			Explain: true, TopN: topN,
		})
		if err != nil {
			return err
		}
		out = resp
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// Models fetches the published model cards.
func (c *Client) Models(ctx context.Context) ([]ModelInfo, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.cfg.BaseURL+"/v1/models", nil)
	if err != nil {
		return nil, err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("fetch models: %w", err)
	}
	defer func() { _, _ = io.Copy(io.Discard, resp.Body); resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("fetch models: unexpected status %d", resp.StatusCode)
	}

	var body struct {
		Models []ModelInfo `json:"models"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return nil, fmt.Errorf("decode models: %w", err)
	}
	return body.Models, nil
}

// Ready reports whether the service has models loaded.
func (c *Client) Ready(ctx context.Context) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.cfg.BaseURL+"/readyz", nil)
	if err != nil {
		return err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer func() { _, _ = io.Copy(io.Discard, resp.Body); resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("inference service not ready: status %d", resp.StatusCode)
	}
	return nil
}

func (c *Client) post(ctx context.Context, path string, payload any) (*PredictResponse, error) {
	body, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("encode request: %w", err)
	}

	req, err := http.NewRequestWithContext(
		ctx, http.MethodPost, c.cfg.BaseURL+path, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")

	// Carry the trace forward, so one pitch stays greppable across both
	// services' logs.
	if corr := correlationIDFrom(ctx); corr != "" {
		req.Header.Set("X-Correlation-ID", corr)
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("call inference service: %w", err)
	}
	defer func() { _, _ = io.Copy(io.Discard, resp.Body); resp.Body.Close() }()

	switch {
	case resp.StatusCode == http.StatusOK:
	case resp.StatusCode == http.StatusBadRequest:
		// A contract violation is a deployment problem, not a transient one.
		// It is surfaced as its own error so the caller does not retry it.
		return nil, fmt.Errorf("%w: %s", ErrFeatureContract, readProblem(resp.Body))
	default:
		return nil, fmt.Errorf("inference service returned %d: %s",
			resp.StatusCode, readProblem(resp.Body))
	}

	var out PredictResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, fmt.Errorf("decode prediction: %w", err)
	}
	return &out, nil
}

func readProblem(r io.Reader) string {
	var p struct {
		Detail string `json:"detail"`
		Errors []struct {
			Field   string `json:"field"`
			Message string `json:"message"`
		} `json:"errors"`
	}
	if err := json.NewDecoder(r).Decode(&p); err != nil {
		return "unreadable error body"
	}
	if len(p.Errors) > 0 {
		return fmt.Sprintf("%s (%s: %s)", p.Detail, p.Errors[0].Field, p.Errors[0].Message)
	}
	return p.Detail
}

type ctxKey int

const ctxCorrelationID ctxKey = iota

// WithCorrelationID attaches a correlation id for outbound calls.
func WithCorrelationID(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, ctxCorrelationID, id)
}

func correlationIDFrom(ctx context.Context) string {
	id, _ := ctx.Value(ctxCorrelationID).(string)
	return id
}
