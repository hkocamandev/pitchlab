// Command processor consumes pitch events and turns them into scored,
// persisted pitches.
//
// The second binary from the same codebase as the API. They share the domain
// model, the repository layer and the validation rules; splitting the code
// would mean keeping two copies of the same types in sync for nothing. They
// are split at runtime because their scaling profiles genuinely differ: the
// API is latency-bound and driven by unpredictable user traffic, while this
// process is throughput-bound and driven by Kafka lag. A burst replay that
// saturates this process must not slow the dashboard down with it.
//
// Two consumer groups run here, on the same topic, for different reasons:
//
//	pitch-processor    reads pitch.raw, scores and stores
//	anomaly-evaluator  reads pitch.analyzed, watches for baseline departures
//
// The second is a separate group rather than a step in the first because an
// anomaly is a session-level finding -- it has nothing to say until an outing
// is over -- and because evaluating it must not extend the transaction that
// stores a pitch.
package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/hkocamandev/pitchlab/internal/cache"
	"github.com/hkocamandev/pitchlab/internal/db"
	"github.com/hkocamandev/pitchlab/internal/events"
	pkafka "github.com/hkocamandev/pitchlab/internal/kafka"
	"github.com/hkocamandev/pitchlab/internal/mlclient"
	"github.com/hkocamandev/pitchlab/internal/observability"
	"github.com/hkocamandev/pitchlab/internal/processor"
)

func main() {
	if err := run(); err != nil {
		slog.Error("fatal", "error", err)
		os.Exit(1)
	}
}

func run() error {
	version := env("PITCHLAB_VERSION", "dev")

	log := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{
		Level: parseLevel(env("PITCHLAB_LOG_LEVEL", "info")),
		ReplaceAttr: func(_ []string, a slog.Attr) slog.Attr {
			if a.Key == slog.TimeKey {
				if t, ok := a.Value.Any().(time.Time); ok {
					return slog.String("ts", t.UTC().Format(time.RFC3339Nano))
				}
			}
			return a
		},
	})).With("service", "pitchlab-processor", "version", version)
	slog.SetDefault(log)

	brokers := strings.Split(env("KAFKA_BROKERS", "localhost:9094"), ",")
	dsn := env("DATABASE_URL",
		"postgres://pitchlab:pitchlab@localhost:5432/pitchlab?sslmode=disable")
	mlURL := env("PITCHLAB_ML_URL", "http://localhost:8000")
	concurrency := envInt("PITCHLAB_CONSUMER_CONCURRENCY", 3)

	log.Info("starting",
		"brokers", brokers, "ml_url", mlURL, "concurrency", concurrency)

	ctx, stop := signal.NotifyContext(context.Background(),
		syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	metrics := observability.New()

	// The scrape endpoint is the processor's only HTTP surface: it has no
	// product API, but it is the service whose health matters most, because
	// consumer lag is where "the system cannot keep up" first becomes visible.
	stopMetrics := startMetricsServer(
		env("PITCHLAB_PROCESSOR_METRICS_ADDR", ":9091"), metrics, log)
	defer stopMetrics()

	poolCfg := db.DefaultPoolConfig(dsn)
	poolCfg.Tracer = observability.NewQueryTracer(metrics)

	pool, err := db.NewPool(ctx, poolCfg)
	if err != nil {
		return err
	}
	defer pool.Close()
	store := db.NewStore(pool)

	producer := pkafka.NewProducer(pkafka.DefaultProducerConfig(brokers), log).
		WithMetrics(metrics.ProducerHooks())
	defer func() {
		if err := producer.Close(); err != nil {
			log.Warn("close producer", "error", err)
		}
	}()

	ml := mlclient.New(mlclient.DefaultConfig(mlURL), log)
	metrics.TrackBreaker(func() string { return ml.BreakerState().String() })

	// A warning, not a failure. The pipeline is designed to keep running with
	// inference down -- pitches are stored with their prediction marked failed
	// and can be backfilled -- so refusing to start would be worse than
	// starting degraded.
	readyCtx, cancelReady := context.WithTimeout(ctx, 5*time.Second)
	if err := ml.Ready(readyCtx); err != nil {
		log.Warn("inference service is not ready; pitches will be stored unscored",
			"error", err)
	}
	cancelReady()

	// Optional by design: with Redis down the processor stores everything it
	// stores now, and the dashboard simply recomputes its live numbers from
	// the rows.
	redis, err := cache.New(cache.DefaultConfig(
		env("REDIS_URL", "redis://localhost:6379/0")), log)
	if err != nil {
		log.Warn("cache disabled; continuing without it", "error", err)
		redis = nil
	}
	defer func() {
		if err := redis.Close(); err != nil {
			log.Warn("close cache", "error", err)
		}
	}()
	redis = redis.WithMetrics(cache.Metrics{OnOperation: metrics.CacheHook()})

	onDuplicate, onPersisted, onPrediction := metrics.PipelineHooks()
	pipeline := processor.NewPipeline(store, ml, producer, log, version).
		WithCache(redis).
		WithMetrics(processor.PipelineMetrics{
			OnDuplicate:        onDuplicate,
			OnPersisted:        onPersisted,
			OnPrediction:       onPrediction,
			OnPredictionStored: metrics.PredictionStoredHook(),
		})

	anomalyCfg := processor.DefaultAnomalyConfig()
	if v := os.Getenv("PITCHLAB_ANOMALY_IDLE_TIMEOUT"); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			anomalyCfg.IdleTimeout = d
		}
	}
	evaluator := processor.NewAnomalyEvaluator(store, producer, anomalyCfg, log, version).
		WithCache(redis).
		OnFinding(metrics.AnomalyHook())

	onProcessed, onDLQ := metrics.ConsumerHooks()
	consumerMetrics := pkafka.ConsumerMetrics{OnProcessed: onProcessed, OnDLQ: onDLQ}

	// Lag is sampled from the broker rather than observed while consuming,
	// because a reader in a consumer group cannot report its own lag.
	for topic, group := range map[string]string{
		events.TopicPitchRaw:      "pitch-processor",
		events.TopicPitchAnalyzed: "anomaly-evaluator",
	} {
		sampler := pkafka.NewLagSampler(brokers, topic, group, log, metrics.LagHook())
		go sampler.Run(ctx)
	}

	rawConsumer := pkafka.NewConsumer(withConcurrency(
		pkafka.DefaultConsumerConfig(brokers, events.TopicPitchRaw, "pitch-processor"),
		concurrency, events.TopicPitchRawDLQ, false), producer, log).
		WithMetrics(consumerMetrics)

	analyzedConsumer := pkafka.NewConsumer(withConcurrency(
		pkafka.DefaultConsumerConfig(brokers, events.TopicPitchAnalyzed, "anomaly-evaluator"),
		1, events.TopicPitchAnalyzedDLQ, false), producer, log).
		WithMetrics(consumerMetrics)

	var wg sync.WaitGroup
	errCh := make(chan error, 3)

	wg.Add(1)
	go func() {
		defer wg.Done()
		if err := rawConsumer.Run(ctx, pipeline.HandlePitchRaw); err != nil {
			errCh <- err
		}
	}()

	wg.Add(1)
	go func() {
		defer wg.Done()
		if err := analyzedConsumer.Run(ctx, evaluator.Handle); err != nil {
			errCh <- err
		}
	}()

	wg.Add(1)
	go func() {
		defer wg.Done()
		if err := evaluator.Run(ctx); err != nil {
			errCh <- err
		}
	}()

	go func() {
		wg.Wait()
		close(errCh)
	}()

	select {
	case err, ok := <-errCh:
		if ok && err != nil {
			stop()
			// Wait for the others to unwind before reporting, so the shutdown
			// is orderly rather than a race to exit.
			wg.Wait()
			return err
		}
	case <-ctx.Done():
		log.Info("shutdown signal received")
	}

	wg.Wait()
	log.Info("stopped")
	return nil
}

func withConcurrency(
	cfg pkafka.ConsumerConfig, concurrency int, dlq string, fromLatest bool,
) pkafka.ConsumerConfig {
	cfg.Concurrency = concurrency
	cfg.DLQTopic = dlq
	cfg.StartFromLatest = fromLatest
	return cfg
}

// startMetricsServer exposes /metrics, /healthz and /readyz for the processor.
//
// A separate listener from anything product-facing, on its own port, because
// these endpoints are infrastructure: they should be reachable when the
// service is degraded, and they should not be exposed alongside an API.
//
// /healthz checks nothing here either. If it probed Kafka, a broker blip would
// mark every processor instance unhealthy and the orchestrator would restart
// all of them at once -- turning a recoverable pause into a cold-start storm.
func startMetricsServer(
	addr string, metrics *observability.Metrics, log *slog.Logger,
) func() {
	mux := http.NewServeMux()
	mux.Handle("GET /metrics", metrics.Handler())
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":"ok"}`))
	})

	srv := &http.Server{
		Addr:              addr,
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
	}

	go func() {
		log.Info("metrics listening", "addr", addr)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			// A failure here must not stop the processor. Losing metrics is
			// bad; losing the pipeline because metrics could not bind a port
			// would be worse.
			log.Error("metrics server stopped", "error", err)
		}
	}()

	return func() {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdownCtx)
	}
}

func env(key, fallback string) string {
	if v, ok := os.LookupEnv(key); ok && v != "" {
		return v
	}
	return fallback
}

func envInt(key string, fallback int) int {
	if v, ok := os.LookupEnv(key); ok && v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			return n
		}
	}
	return fallback
}

func parseLevel(s string) slog.Level {
	switch strings.ToLower(s) {
	case "debug":
		return slog.LevelDebug
	case "warn":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}
