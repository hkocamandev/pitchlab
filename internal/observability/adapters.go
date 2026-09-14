package observability

import (
	"strconv"
	"time"
)

// The adapters below turn this package's metrics into the callback structs the
// rest of the codebase already declares.
//
// Those callbacks were added in earlier phases specifically so that no package
// would have to import a metrics library to be observable. Keeping the wiring
// here preserves that: internal/kafka, internal/processor, internal/ws and
// internal/cache still know nothing about Prometheus, and every one of them is
// testable without it.

// ConsumerHooks reports Kafka consumption.
func (m *Metrics) ConsumerHooks() (
	onProcessed func(topic string, class string, attempts int, d time.Duration),
	onDLQ func(topic, errorClass string),
) {
	onProcessed = func(topic string, class string, _ int, d time.Duration) {
		m.KafkaConsumed.WithLabelValues(topic, class).Inc()
		m.KafkaProcessing.WithLabelValues(topic).Observe(d.Seconds())
	}
	onDLQ = func(topic, errorClass string) {
		m.KafkaDLQ.WithLabelValues(topic, errorClass).Inc()
	}
	return onProcessed, onDLQ
}

// LagHook reports a sampled consumer lag.
//
// Separate from the consumer hooks because lag is not observed while
// consuming: it is computed from the group's committed offsets and the
// partitions' log ends, on a ticker, by a sampler that talks to the broker
// directly.
func (m *Metrics) LagHook() func(topic string, partition int, lag int64) {
	return func(topic string, partition int, lag int64) {
		// Partition counts are fixed at topic creation -- which is the same
		// reason auto-creation is disabled on the broker -- so this label is
		// bounded by design.
		m.KafkaLag.WithLabelValues(topic, strconv.Itoa(partition)).Set(float64(lag))
	}
}

// ProducerHooks reports publish failures.
func (m *Metrics) ProducerHooks() func(topic string) {
	return func(topic string) {
		m.KafkaProduceErrors.WithLabelValues(topic).Inc()
	}
}

// PipelineHooks reports pitch processing.
func (m *Metrics) PipelineHooks() (
	onDuplicate func(),
	onPersisted func(time.Duration),
	onPrediction func(variant string, d time.Duration, err error),
) {
	onDuplicate = func() { m.PitchDuplicates.Inc() }
	onPersisted = func(d time.Duration) {
		m.PitchPersistDuration.Observe(d.Seconds())
	}
	onPrediction = func(variant string, d time.Duration, err error) {
		m.MLDuration.WithLabelValues(variant).Observe(d.Seconds())
		if err != nil {
			// The class comes from a fixed vocabulary, never from the error
			// text -- an error message can contain an address, an id, or
			// anything else unbounded.
			m.MLErrors.WithLabelValues(variant, ClassifyMLError(err)).Inc()
		}
	}
	return onDuplicate, onPersisted, onPrediction
}

// PredictionStoredHook counts a written prediction.
func (m *Metrics) PredictionStoredHook() func(variant, modelVersion string) {
	return func(variant, modelVersion string) {
		m.PredictionsGenerated.WithLabelValues(variant, modelVersion).Inc()
	}
}

// AnomalyHook reports detections.
func (m *Metrics) AnomalyHook() func(metric, severity string) {
	return func(metric, severity string) {
		m.AnomaliesDetected.WithLabelValues(metric, severity).Inc()
	}
}

// CacheHook reports cache operations.
func (m *Metrics) CacheHook() func(operation, result string) {
	return func(operation, result string) {
		m.CacheOperations.WithLabelValues(operation, result).Inc()
	}
}

// WebSocketHooks report delivery activity.
//
// Connection count is not here: it is read from the hub at scrape time, so it
// cannot drift away from the truth the way a counted gauge does.
func (m *Metrics) WebSocketHooks() (
	onSent func(messageType string),
	onDropped func(messageType string),
	onSlowConsumer func(delta int),
) {
	onSent = func(messageType string) {
		m.WSMessagesSent.WithLabelValues(messageType).Inc()
	}
	onDropped = func(messageType string) {
		m.WSMessagesDrop.WithLabelValues(messageType).Inc()
	}
	onSlowConsumer = func(delta int) { m.WSSlowConsumers.Add(float64(delta)) }
	return onSent, onDropped, onSlowConsumer
}
