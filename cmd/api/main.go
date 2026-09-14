// Command api serves the REST surface under /api/v1.
//
// It is one of two binaries built from the same codebase. The API and the
// stream processor share the domain model, the repository layer and the
// validation rules -- splitting the code would mean keeping two copies of the
// same types in sync for nothing. They are split at runtime because their
// scaling profiles genuinely differ: this process is latency-bound and driven
// by unpredictable user traffic, while the processor is throughput-bound and
// driven by Kafka lag. A burst replay that saturates the processor must not
// slow the dashboard down with it.
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

	"github.com/google/uuid"

	"github.com/hkocamandev/pitchlab/internal/api"
	"github.com/hkocamandev/pitchlab/internal/cache"
	"github.com/hkocamandev/pitchlab/internal/config"
	"github.com/hkocamandev/pitchlab/internal/db"
	"github.com/hkocamandev/pitchlab/internal/events"
	pkafka "github.com/hkocamandev/pitchlab/internal/kafka"
	"github.com/hkocamandev/pitchlab/internal/mlclient"
	"github.com/hkocamandev/pitchlab/internal/observability"
	"github.com/hkocamandev/pitchlab/internal/ws"
)

func main() {
	if err := run(); err != nil {
		slog.Error("fatal", "error", err)
		os.Exit(1)
	}
}

func run() error {
	cfg, err := config.LoadAPI()
	if err != nil {
		// Configuration problems are fatal at startup by design. A process
		// that boots with half a configuration fails later, somewhere less
		// obvious, and usually under load.
		return err
	}

	log := newLogger(cfg)
	slog.SetDefault(log)

	log.Info("starting",
		"service", "pitchlab-api",
		"version", cfg.Version,
		"addr", cfg.Addr,
		"allowed_origins", cfg.AllowedOrigins,
	)

	// Signal handling is installed before anything is opened, so a Ctrl-C
	// during startup still unwinds cleanly.
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	metrics := observability.New()

	poolCfg := db.DefaultPoolConfig(cfg.DatabaseURL)
	poolCfg.Tracer = observability.NewQueryTracer(metrics)

	pool, err := db.NewPool(ctx, poolCfg)
	if err != nil {
		return err
	}
	defer pool.Close()
	store := db.NewStore(pool)

	// Redis is optional. A failure to build the client is logged and the
	// process continues without a cache -- making the API refuse to start over
	// a cache would make the cache critical, which is the one property this
	// layer must not have.
	redis, err := cache.New(cache.DefaultConfig(cfg.RedisURL), log)
	if err != nil {
		log.Warn("cache disabled; continuing without it", "error", err)
		redis = nil
	}
	redis = redis.WithMetrics(cache.Metrics{OnOperation: metrics.CacheHook()})
	defer func() {
		if err := redis.Close(); err != nil {
			log.Warn("close cache", "error", err)
		}
	}()

	onSent, onDropped, onSlow := metrics.WebSocketHooks()
	hub := ws.NewHub(log).WithMetrics(ws.HubMetrics{
		OnSent: onSent, OnDropped: onDropped, OnSlowConsumer: onSlow,
	})
	// Read from the hub at scrape time rather than counted on connect and
	// disconnect, so the number cannot drift from the truth.
	metrics.TrackWebSocketConnections(hub.Clients)
	var hubDone sync.WaitGroup
	hubDone.Add(1)
	go func() {
		defer hubDone.Done()
		hub.Run(ctx)
	}()

	fanoutDone := startFanout(ctx, hub, metrics, log)

	// Used only for on-demand explanations. Scores are written by the
	// processor and read from the database; the API never scores a pitch on
	// the request path.
	//
	// The deadline is longer than the processor's two seconds, and that is not
	// an oversight. Attribution costs a model evaluation per feature, and the
	// first request for a variant also builds the explainer -- measured at
	// over five seconds. The processor's short timeout is right for scoring,
	// where a hung service would hold a Kafka partition hostage; here the only
	// thing waiting is one person who asked about one pitch.
	mlCfg := mlclient.DefaultConfig(env("PITCHLAB_ML_URL", "http://localhost:8000"))
	mlCfg.Timeout = duration("PITCHLAB_EXPLAIN_TIMEOUT", 20*time.Second)
	ml := mlclient.New(mlCfg, log)
	metrics.TrackBreaker(func() string { return ml.BreakerState().String() })

	srv := api.New(cfg, store, log).
		WithMetrics(metrics).
		WithCache(redis).
		WithInference(ml).
		WithWebSocket(hub).
		HTTPServer()

	serverErr := make(chan error, 1)
	go func() {
		log.Info("listening", "addr", cfg.Addr)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			serverErr <- err
		}
	}()

	select {
	case err := <-serverErr:
		return err
	case <-ctx.Done():
		log.Info("shutdown signal received")
	}

	// Graceful shutdown: stop accepting, let in-flight requests finish, then
	// close. Killing the listener outright would fail requests that were
	// moments from succeeding.
	shutdownCtx, cancel := context.WithTimeout(context.Background(), cfg.ShutdownTimeout)
	defer cancel()

	if err := srv.Shutdown(shutdownCtx); err != nil {
		log.Warn("graceful shutdown timed out; forcing close", "error", err)
		_ = srv.Close()
	}

	// The listener is closed, so no new connection can arrive; the hub and the
	// fan-out are then stopped in that order, which is what lets every live
	// socket receive a 1001 Going Away instead of a reset.
	hubDone.Wait()
	fanoutDone.Wait()

	log.Info("stopped")
	return nil
}

// startFanout consumes the analyzed-pitch and anomaly topics and broadcasts
// them to connected dashboards.
//
// It is deliberately lenient about failure. This consumer owns no durable
// state: everything it broadcasts is already committed to PostgreSQL, so a
// broker that is unreachable costs live updates and nothing else. Refusing to
// start the API over it would take the REST surface down for a cache-shaped
// problem.
func startFanout(
	ctx context.Context, hub *ws.Hub, metrics *observability.Metrics,
	log *slog.Logger,
) *sync.WaitGroup {
	var wg sync.WaitGroup

	brokers := strings.Split(env("KAFKA_BROKERS", "localhost:9094"), ",")
	fanout := api.NewFanout(hub, log)
	onProcessed, onDLQ := metrics.ConsumerHooks()

	// A group per instance, not one group shared by every replica.
	//
	// A shared group splits the partitions between replicas, so a browser
	// connected to the replica that was not assigned its session's partition
	// would sit there receiving nothing -- with no error anywhere, because
	// from Kafka's point of view the messages were delivered. Each replica
	// reading the whole topic costs bandwidth and gets the semantics right.
	// Phase 8 replaces this with a single consumer fanning out over Redis.
	group := env("PITCHLAB_WS_GROUP_PREFIX", "ws-fanout") + "-" + instanceID()

	for _, topic := range []string{events.TopicPitchAnalyzed, events.TopicAnomaly} {
		cfg := pkafka.DefaultConsumerConfig(brokers, topic, group)
		// One reader: broadcast order within a session should match the order
		// the pitches were processed in.
		cfg.Concurrency = 1
		// Start at the latest offset. A dashboard opened now wants what is
		// happening now, not a replay of every pitch since the topic was
		// created.
		cfg.StartFromLatest = true
		// No dead-letter queue. There is nothing to recover: the durable copy
		// is in PostgreSQL and a live notification that missed its moment has
		// no value later.
		cfg.DLQTopic = ""

		consumer := pkafka.NewConsumer(cfg, nil, log).WithMetrics(pkafka.ConsumerMetrics{
			OnProcessed: onProcessed, OnDLQ: onDLQ,
		})

		sampler := pkafka.NewLagSampler(brokers, topic, group, log, metrics.LagHook())
		go sampler.Run(ctx)

		wg.Add(1)
		go func(topic string) {
			defer wg.Done()
			if err := consumer.Run(ctx, fanout.Handle); err != nil {
				log.Error("websocket fan-out stopped", "topic", topic, "error", err)
			}
		}(topic)
	}

	return &wg
}

// instanceID identifies this process for consumer-group naming.
func instanceID() string {
	if v := os.Getenv("PITCHLAB_INSTANCE_ID"); v != "" {
		return v
	}
	if host, err := os.Hostname(); err == nil && host != "" {
		return host + "-" + strconv.Itoa(os.Getpid())
	}
	return uuid.NewString()
}

func duration(key string, fallback time.Duration) time.Duration {
	if v, ok := os.LookupEnv(key); ok && v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			return d
		}
	}
	return fallback
}

func env(key, fallback string) string {
	if v, ok := os.LookupEnv(key); ok && v != "" {
		return v
	}
	return fallback
}

func newLogger(cfg config.API) *slog.Logger {
	level := slog.LevelInfo
	switch cfg.LogLevel {
	case "debug":
		level = slog.LevelDebug
	case "warn":
		level = slog.LevelWarn
	case "error":
		level = slog.LevelError
	}

	opts := &slog.HandlerOptions{
		Level: level,
		ReplaceAttr: func(_ []string, a slog.Attr) slog.Attr {
			// RFC 3339 with nanoseconds, so log lines from different services
			// interleave correctly when traced by correlation id.
			if a.Key == slog.TimeKey {
				if t, ok := a.Value.Any().(time.Time); ok {
					return slog.String("ts", t.UTC().Format(time.RFC3339Nano))
				}
			}
			return a
		},
	}

	var handler slog.Handler
	if cfg.LogFormat == "text" {
		handler = slog.NewTextHandler(os.Stdout, opts)
	} else {
		handler = slog.NewJSONHandler(os.Stdout, opts)
	}

	// Every line carries the service and version, so a multi-service log
	// stream stays attributable.
	return slog.New(handler).With(
		"service", "pitchlab-api",
		"version", cfg.Version,
	)
}
