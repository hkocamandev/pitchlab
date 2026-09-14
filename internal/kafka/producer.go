package kafka

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"time"

	"github.com/segmentio/kafka-go"

	"github.com/hkocamandev/pitchlab/internal/events"
)

// ProducerConfig configures a writer.
type ProducerConfig struct {
	Brokers []string

	// BatchTimeout trades a little latency for fewer round trips. Ten
	// milliseconds is invisible in a pipeline whose events describe pitches
	// twenty seconds apart, and it matters a great deal under burst replay.
	BatchTimeout time.Duration
	BatchSize    int

	WriteTimeout time.Duration
	MaxAttempts  int

	// MetadataTTL bounds how stale the cached view of topics and partitions
	// may be. Short, because a topic created moments ago has to be writable.
	MetadataTTL time.Duration
}

// DefaultProducerConfig returns sensible defaults.
func DefaultProducerConfig(brokers []string) ProducerConfig {
	return ProducerConfig{
		Brokers:      brokers,
		BatchTimeout: 10 * time.Millisecond,
		BatchSize:    100,
		WriteTimeout: 10 * time.Second,
		MaxAttempts:  5,
		MetadataTTL:  1 * time.Second,
	}
}

// Producer writes envelopes to Kafka.
type Producer struct {
	writer    *kafka.Writer
	transport *kafka.Transport
	log       *slog.Logger

	// onProduceError is optional and carries only the topic. Nothing
	// identifying the message reaches it, because the only caller is a metric.
	onProduceError func(topic string)
}

// WithMetrics attaches a publish-failure hook.
func (p *Producer) WithMetrics(onProduceError func(topic string)) *Producer {
	p.onProduceError = onProduceError
	return p
}

func (p *Producer) recordProduceError(topic string) {
	if p.onProduceError != nil {
		p.onProduceError(topic)
	}
}

// NewProducer builds a producer.
//
// RequireAll, not RequireOne: acknowledging before every in-sync replica has
// the message means a broker failure can lose it. On a single-broker
// development cluster the two are identical, but the setting is a statement
// of intent that survives into a real deployment.
func NewProducer(cfg ProducerConfig, log *slog.Logger) *Producer {
	// An explicit transport rather than the library's package-level default.
	//
	// The default is shared by every writer in the process and caches topic
	// metadata globally, so a writer can be told a topic does not exist
	// because a *different* writer looked before it was created. That showed
	// up as integration tests passing alone and failing as a suite, and the
	// same staleness would delay writes to a newly created topic in
	// production. Owning the transport also means Close actually releases
	// this producer's connections.
	transport := &kafka.Transport{
		MetadataTTL: cfg.MetadataTTL,
		DialTimeout: cfg.WriteTimeout,
	}

	return &Producer{
		transport: transport,
		writer: &kafka.Writer{
			Addr:      kafka.TCP(cfg.Brokers...),
			Transport: transport,
			Balancer:  &kafka.Hash{},
			// Hash balancer, not round-robin: the partition must be a
			// function of the key, because that is the entire mechanism
			// guaranteeing a session's pitches stay in order.
			RequiredAcks: kafka.RequireAll,
			Async:        false,
			// Synchronous writes. Async would let the caller commit a Kafka
			// offset for work whose downstream event had not actually been
			// written yet.
			BatchTimeout: cfg.BatchTimeout,
			BatchSize:    cfg.BatchSize,
			WriteTimeout: cfg.WriteTimeout,
			MaxAttempts:  cfg.MaxAttempts,
			Compression:  kafka.Snappy,
			ErrorLogger: kafka.LoggerFunc(func(msg string, args ...any) {
				log.Error("kafka writer", "detail", fmt.Sprintf(msg, args...))
			}),
		},
		log: log,
	}
}

// Publish writes one envelope to a topic.
func (p *Producer) Publish(ctx context.Context, topic string, env *events.Envelope) error {
	if err := env.Validate(); err != nil {
		// Refusing to publish an invalid envelope keeps the problem on the
		// producing side, where there is a stack trace, instead of turning it
		// into a dead letter somebody has to reverse-engineer later.
		return Permanent("refusing to publish an invalid envelope", err)
	}

	value, err := json.Marshal(env)
	if err != nil {
		return Permanent("encode envelope", err)
	}

	msg := kafka.Message{
		Topic: topic,
		Key:   []byte(env.PartitionKey),
		Value: value,
		Time:  env.ProducedAt,
		// Headers duplicate three envelope fields so that tooling -- a
		// console consumer, Kafka UI -- can filter and trace without parsing
		// the body.
		Headers: []kafka.Header{
			{Key: "event_type", Value: []byte(env.EventType)},
			{Key: "correlation_id", Value: []byte(env.CorrelationID.String())},
			{Key: "idempotency_key", Value: []byte(env.IdempotencyKey)},
		},
	}

	if err := p.writer.WriteMessages(ctx, msg); err != nil {
		p.recordProduceError(topic)
		return fmt.Errorf("publish to %s: %w", topic, err)
	}
	return nil
}

// PublishBatch writes several envelopes at once.
func (p *Producer) PublishBatch(ctx context.Context, topic string, envs []*events.Envelope) error {
	if len(envs) == 0 {
		return nil
	}

	msgs := make([]kafka.Message, 0, len(envs))
	for _, env := range envs {
		if err := env.Validate(); err != nil {
			return Permanent("refusing to publish an invalid envelope", err)
		}
		value, err := json.Marshal(env)
		if err != nil {
			return Permanent("encode envelope", err)
		}
		msgs = append(msgs, kafka.Message{
			Topic: topic,
			Key:   []byte(env.PartitionKey),
			Value: value,
			Time:  env.ProducedAt,
			Headers: []kafka.Header{
				{Key: "event_type", Value: []byte(env.EventType)},
				{Key: "correlation_id", Value: []byte(env.CorrelationID.String())},
				{Key: "idempotency_key", Value: []byte(env.IdempotencyKey)},
			},
		})
	}

	if err := p.writer.WriteMessages(ctx, msgs...); err != nil {
		p.recordProduceError(topic)
		return fmt.Errorf("publish %d messages to %s: %w", len(msgs), topic, err)
	}
	return nil
}

// Close flushes and shuts down.
func (p *Producer) Close() error {
	err := p.writer.Close()
	if p.transport != nil {
		p.transport.CloseIdleConnections()
	}
	return err
}

// Stats exposes writer counters for metrics.
func (p *Producer) Stats() kafka.WriterStats { return p.writer.Stats() }
