// Package observability holds the metric definitions and the plumbing that
// connects them to the rest of the system.
//
// Every metric in the project is declared here, in one file, on purpose. The
// rule that matters most for metrics is a cardinality rule -- no identifier
// ever becomes a label -- and a rule is only enforceable if there is one place
// to check. A metric declared next to the code that increments it is a metric
// nobody reviews.
package observability

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
)

// Forbidden label names.
//
// Session, athlete, pitch and correlation identifiers are unbounded. A label
// with one of these creates a new time series per pitch, and a Prometheus
// instance that ingests a replay at 800x would be storing millions of series
// within a minute -- the classic way a metrics stack is taken down by the
// system it is supposed to watch.
//
// These belong in logs, where they are searchable and cost nothing to add.
// A test enforces this list against the actual registry.
var ForbiddenLabels = []string{
	"session_id", "athlete_id", "pitch_id", "batter_id", "pitcher_id",
	"correlation_id", "request_id", "event_id", "idempotency_key",
	"external_pitch_uid", "external_session_uid", "user_id",
}

// Latency buckets.
//
// Chosen for the two shapes this system actually produces rather than taken
// from the library default: an HTTP read that should be single-digit
// milliseconds, and an inference call that should be tens.
var (
	httpBuckets = []float64{
		0.001, 0.0025, 0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5,
	}
	processingBuckets = []float64{
		0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10,
	}
)

// Metrics is every metric this project exports.
type Metrics struct {
	registry *prometheus.Registry

	// HTTP.
	HTTPDuration *prometheus.HistogramVec
	HTTPTotal    *prometheus.CounterVec
	HTTPInFlight prometheus.Gauge

	// WebSocket.
	WSMessagesSent  *prometheus.CounterVec
	WSMessagesDrop  *prometheus.CounterVec
	WSSlowConsumers prometheus.Gauge

	// Cache.
	CacheOperations *prometheus.CounterVec

	// Database.
	DBQueryDuration *prometheus.HistogramVec

	// Kafka.
	KafkaConsumed      *prometheus.CounterVec
	KafkaLag           *prometheus.GaugeVec
	KafkaProcessing    *prometheus.HistogramVec
	KafkaProduceErrors *prometheus.CounterVec
	KafkaFetchErrors   *prometheus.CounterVec
	KafkaDLQ           *prometheus.CounterVec

	// Pipeline.
	PitchPersistDuration prometheus.Histogram
	PitchDuplicates      prometheus.Counter
	PredictionsGenerated *prometheus.CounterVec
	AnomaliesDetected    *prometheus.CounterVec

	// Inference client.
	MLDuration *prometheus.HistogramVec
	MLErrors   *prometheus.CounterVec
}

// New builds the metric set and registers it.
//
// A private registry rather than the default one. The default registry is
// global state that any dependency can write to, and a test that asserts what
// this service exports should not be at the mercy of a library that registered
// something during init.
func New() *Metrics {
	reg := prometheus.NewRegistry()

	m := &Metrics{
		registry: reg,

		HTTPDuration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "http_request_duration_seconds",
			Help:    "HTTP request latency.",
			Buckets: httpBuckets,
			// route is the registered pattern, never the requested path. The
			// path contains a UUID; the pattern does not, and there are a
			// fixed number of patterns.
		}, []string{"method", "route", "status"}),

		HTTPTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "http_requests_total",
			Help: "HTTP requests by outcome.",
		}, []string{"method", "route", "status"}),

		HTTPInFlight: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "http_requests_in_flight",
			Help: "HTTP requests currently being served.",
		}),

		WSMessagesSent: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "websocket_messages_sent_total",
			Help: "Messages handed to WebSocket connections.",
		}, []string{"message_type"}),

		WSMessagesDrop: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "websocket_messages_dropped_total",
			Help: "Messages dropped because a client could not keep up.",
		}, []string{"reason"}),

		WSSlowConsumers: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "websocket_slow_consumers",
			Help: "Connections whose send queue is near capacity.",
		}),

		CacheOperations: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "cache_operations_total",
			Help: "Cache operations by outcome.",
		}, []string{"operation", "result"}),

		DBQueryDuration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "db_query_duration_seconds",
			Help:    "Database query latency by query name.",
			Buckets: httpBuckets,
			// query_name comes from the generated query's own name, which is
			// bounded by the number of queries in the codebase. The SQL text
			// itself is never a label.
		}, []string{"query_name"}),

		KafkaConsumed: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "kafka_messages_consumed_total",
			Help: "Messages consumed by outcome.",
		}, []string{"topic", "result"}),

		KafkaLag: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "kafka_consumer_lag",
			Help: "Messages behind the head of the partition, sampled from the broker.",
			// Computed from committed offsets against log ends rather than
			// read from the consumer: the client library reports -1 for a
			// reader in a group, and -1 is worse than nothing for the metric
			// the whole pipeline is judged by.
			//
			// Partition counts are fixed at topic creation, so the label is
			// bounded by design.
		}, []string{"topic", "partition"}),

		KafkaProcessing: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "kafka_message_processing_duration_seconds",
			Help:    "Time to process one message, including retries.",
			Buckets: processingBuckets,
		}, []string{"topic"}),

		KafkaProduceErrors: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "kafka_produce_errors_total",
			Help: "Failed publishes.",
		}, []string{"topic"}),

		KafkaFetchErrors: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "kafka_fetch_errors_total",
			Help: "Failed polls. The consumer retries rather than exiting.",
		}, []string{"topic"}),

		KafkaDLQ: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "kafka_dlq_messages_total",
			Help: "Messages written to a dead-letter queue. Should stay at zero.",
		}, []string{"topic", "error_class"}),

		PitchPersistDuration: prometheus.NewHistogram(prometheus.HistogramOpts{
			Name:    "pitch_persist_duration_seconds",
			Help:    "Time to write one pitch and its measurement.",
			Buckets: httpBuckets,
		}),

		PitchDuplicates: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "pitch_duplicates_skipped_total",
			Help: "Redeliveries recognised and skipped. Evidence idempotency works.",
		}),

		PredictionsGenerated: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "predictions_generated_total",
			Help: "Predictions written.",
		}, []string{"model_variant", "model_version"}),

		AnomaliesDetected: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "anomalies_detected_total",
			Help: "Anomalies raised.",
		}, []string{"metric", "severity"}),

		MLDuration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "ml_inference_duration_seconds",
			Help:    "Inference call latency as seen by the caller.",
			Buckets: processingBuckets,
		}, []string{"model_variant"}),

		MLErrors: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "ml_inference_errors_total",
			Help: "Failed inference calls.",
		}, []string{"model_variant", "error_class"}),
	}

	reg.MustRegister(
		m.HTTPDuration, m.HTTPTotal, m.HTTPInFlight,
		m.WSMessagesSent, m.WSMessagesDrop, m.WSSlowConsumers,
		m.CacheOperations, m.DBQueryDuration,
		m.KafkaConsumed, m.KafkaLag, m.KafkaProcessing,
		m.KafkaProduceErrors, m.KafkaFetchErrors, m.KafkaDLQ,
		m.PitchPersistDuration, m.PitchDuplicates,
		m.PredictionsGenerated, m.AnomaliesDetected,
		m.MLDuration, m.MLErrors,
	)

	// Process and runtime metrics. Memory, goroutine count and file
	// descriptors answer "is this process healthy" without any application
	// code, and the goroutine count is what would show a WebSocket leak.
	reg.MustRegister(
		collectors.NewGoCollector(),
		collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}),
	)

	return m
}

// Registry exposes the registry for the scrape handler and for tests.
func (m *Metrics) Registry() *prometheus.Registry { return m.registry }

// TrackWebSocketConnections publishes the live connection count.
//
// Read from the hub at scrape time rather than maintained by incrementing on
// connect and decrementing on disconnect. A counted gauge drifts the first
// time a disconnect path is missed, and it drifts silently: the graph stays
// plausible while it slowly stops matching reality. Reading the number from
// the thing that owns it cannot drift.
func (m *Metrics) TrackWebSocketConnections(count func() int64) {
	m.registry.MustRegister(prometheus.NewGaugeFunc(prometheus.GaugeOpts{
		Name: "websocket_connections_active",
		Help: "Open WebSocket connections.",
	}, func() float64 { return float64(count()) }))
}

// TrackBreaker publishes the inference circuit breaker's position.
//
// Also read at scrape time. A breaker changes state on failure paths, which
// are exactly the paths least likely to have their instrumentation exercised.
func (m *Metrics) TrackBreaker(state func() string) {
	m.registry.MustRegister(prometheus.NewGaugeFunc(prometheus.GaugeOpts{
		Name: "ml_circuit_breaker_state",
		Help: "0 closed, 1 half-open, 2 open.",
	}, func() float64 {
		switch state() {
		case "half_open":
			return 1
		case "open":
			return 2
		default:
			return 0
		}
	}))
}
