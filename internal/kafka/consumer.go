package kafka

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math/rand"
	"sync"
	"time"

	"github.com/segmentio/kafka-go"

	"github.com/hkocamandev/pitchlab/internal/events"
)

// Handler processes one envelope.
//
// Returning nil means the work is durably done and the offset may advance.
// Returning ErrDuplicate means it was already done, which is equally fine.
// Any other error is classified and either retried or dead-lettered.
type Handler func(ctx context.Context, env *events.Envelope) error

// ConsumerConfig configures a consumer.
type ConsumerConfig struct {
	Brokers []string
	Topic   string
	GroupID string

	// DLQTopic receives messages that cannot be processed. Empty means the
	// consumer has nowhere to put them, which is correct for a broadcast
	// consumer whose durable record lives elsewhere.
	DLQTopic string

	// Concurrency is how many readers this process runs in the group. Each
	// reader handles its assigned partitions strictly sequentially, so
	// per-partition ordering holds; the parallelism comes from having several
	// readers, not from processing one partition out of order.
	Concurrency int

	// StartFromLatest skips history on first join. Correct for a live
	// broadcast consumer, wrong for one that must not miss anything.
	StartFromLatest bool

	MaxAttempts    int
	InitialBackoff time.Duration
	MaxBackoff     time.Duration

	MinBytes int
	MaxBytes int
	MaxWait  time.Duration
}

// DefaultConsumerConfig returns sensible defaults.
func DefaultConsumerConfig(brokers []string, topic, groupID string) ConsumerConfig {
	return ConsumerConfig{
		Brokers:     brokers,
		Topic:       topic,
		GroupID:     groupID,
		Concurrency: 3,
		// Five attempts over roughly three seconds. The ceiling matters:
		// retries block the partition, so the budget is a deliberate bound on
		// how long one bad message can hold up the ones behind it.
		MaxAttempts:    5,
		InitialBackoff: 100 * time.Millisecond,
		MaxBackoff:     2 * time.Second,
		MinBytes:       1,
		MaxBytes:       10 << 20,
		MaxWait:        500 * time.Millisecond,
	}
}

// Consumer reads a topic and hands each message to a Handler.
type Consumer struct {
	cfg      ConsumerConfig
	producer *Producer
	log      *slog.Logger
	metrics  ConsumerMetrics
}

// ConsumerMetrics receives per-message outcomes. Nil callbacks are ignored,
// so wiring metrics is optional and the consumer stays testable without them.
type ConsumerMetrics struct {
	// OnProcessed reports the outcome of one message using the vocabulary
	// below, not the internal error class.
	//
	// They are not the same thing and conflating them was a real defect: the
	// success path reported ClassTransient, because that is the zero value of
	// the enum, so every processed message was labelled "transient" and the
	// dashboard could not tell success from a retryable failure.
	//
	//	ok         processed, offset may advance
	//	duplicate  already done; the natural key absorbed a redelivery
	//	dlq        gave up and dead-lettered it
	OnProcessed func(topic string, result string, attempts int, d time.Duration)
	OnDLQ       func(topic, errorClass string)

	// OnFetchError reports a failed poll. Separate from OnProcessed because
	// nothing was processed: the consumer could not reach the broker at all.
	//
	// Consumer lag is deliberately not here. A reader inside a consumer group
	// cannot report its own lag, so it is sampled from the broker by
	// LagSampler instead.
	OnFetchError func(topic string)
}

// NewConsumer builds a consumer.
func NewConsumer(cfg ConsumerConfig, producer *Producer, log *slog.Logger) *Consumer {
	if cfg.Concurrency <= 0 {
		cfg.Concurrency = 1
	}
	if cfg.MaxAttempts <= 0 {
		cfg.MaxAttempts = 5
	}
	if cfg.InitialBackoff <= 0 {
		cfg.InitialBackoff = 100 * time.Millisecond
	}
	if cfg.MaxBackoff <= 0 {
		cfg.MaxBackoff = 2 * time.Second
	}
	return &Consumer{cfg: cfg, producer: producer, log: log}
}

// WithMetrics attaches metric callbacks.
func (c *Consumer) WithMetrics(m ConsumerMetrics) *Consumer {
	c.metrics = m
	return c
}

// Run consumes until the context is cancelled.
func (c *Consumer) Run(ctx context.Context, handle Handler) error {
	var wg sync.WaitGroup
	errCh := make(chan error, c.cfg.Concurrency)

	for i := 0; i < c.cfg.Concurrency; i++ {
		wg.Add(1)
		go func(worker int) {
			defer wg.Done()
			if err := c.runReader(ctx, worker, handle); err != nil {
				errCh <- err
			}
		}(i)
	}

	wg.Wait()
	close(errCh)

	for err := range errCh {
		if err != nil && !errors.Is(err, context.Canceled) {
			return err
		}
	}
	return nil
}

func (c *Consumer) runReader(ctx context.Context, worker int, handle Handler) error {
	startOffset := kafka.FirstOffset
	if c.cfg.StartFromLatest {
		startOffset = kafka.LastOffset
	}

	reader := kafka.NewReader(kafka.ReaderConfig{
		Brokers:     c.cfg.Brokers,
		Topic:       c.cfg.Topic,
		GroupID:     c.cfg.GroupID,
		MinBytes:    c.cfg.MinBytes,
		MaxBytes:    c.cfg.MaxBytes,
		MaxWait:     c.cfg.MaxWait,
		StartOffset: startOffset,
		// Auto-commit is off. It would advance the offset on a timer,
		// independent of whether the work succeeded, and a crash in that
		// window loses the message silently.
		CommitInterval: 0,
		ErrorLogger: kafka.LoggerFunc(func(msg string, args ...any) {
			c.log.Error("kafka reader", "topic", c.cfg.Topic,
				"detail", fmt.Sprintf(msg, args...))
		}),
	})
	defer func() {
		if err := reader.Close(); err != nil {
			c.log.Warn("close reader", "topic", c.cfg.Topic, "error", err)
		}
	}()

	log := c.log.With("topic", c.cfg.Topic, "group", c.cfg.GroupID, "worker", worker)
	log.Info("consumer started")

	// Consecutive fetch failures, used only to grow the backoff. Reset on the
	// first success, so an hour of healthy consumption does not make the next
	// blip wait the maximum.
	fetchFailures := 0

	for {
		// FetchMessage, not ReadMessage: ReadMessage commits for you, which
		// is the same mistake as auto-commit wearing a different hat.
		msg, err := reader.FetchMessage(ctx)
		if err != nil {
			if errors.Is(err, context.Canceled) || errors.Is(err, ctx.Err()) {
				log.Info("consumer stopping")
				return nil
			}

			// A failed fetch is not a reason to end the process.
			//
			// This used to return the error, which propagated to main and
			// exited. A single transient dial failure -- a broker restarting,
			// a rebalance, or a machine briefly out of sockets -- killed the
			// stream processor and left recovery entirely to whatever was
			// supposed to restart it. It was found by a load test exhausting
			// local file descriptors, which is the mildest possible version of
			// a problem that in production is a rolling broker upgrade.
			//
			// The reader reconnects on its own; what it needs is for the loop
			// to keep asking. Backing off first so an unreachable broker is not
			// hammered.
			fetchFailures++
			delay := c.backoff(min(fetchFailures, c.cfg.MaxAttempts))
			log.Warn("fetch failed; retrying",
				"consecutive_failures", fetchFailures, "retry_in", delay, "error", err)
			c.recordFetchError(c.cfg.Topic)

			select {
			case <-time.After(delay):
			case <-ctx.Done():
				return nil
			}
			continue
		}
		fetchFailures = 0

		c.processMessage(ctx, log, msg, handle)

		// The offset advances only now, after the work is durably done.
		// At-least-once: a crash before this point replays the message, which
		// the handler is required to absorb. The alternative, committing
		// first, is at-most-once and loses pitches.
		if err := reader.CommitMessages(ctx, msg); err != nil {
			if errors.Is(err, context.Canceled) {
				return nil
			}
			// The work is done but the offset did not move, so the message
			// will be redelivered. That is survivable precisely because the
			// handler is idempotent.
			log.Error("commit offset", "partition", msg.Partition,
				"offset", msg.Offset, "error", err)
		}
	}
}

func (c *Consumer) processMessage(
	ctx context.Context, log *slog.Logger, msg kafka.Message, handle Handler,
) {
	started := time.Now()

	env, err := events.Parse(msg.Value)
	if err != nil {
		// A message that cannot be parsed will never parse. Retrying is pure
		// delay.
		//
		// The class distinguishes malformed bytes from a message this build
		// is too old to read: the first means something published garbage,
		// the second means a newer producer is already in the topic and this
		// consumer needs upgrading. Collapsing them into one class hides a
		// rollout problem behind what looks like corruption.
		class := parseErrorClass(err)
		log.Error("unparseable message", "error_class", class,
			"partition", msg.Partition, "offset", msg.Offset, "error", err)
		c.deadLetter(ctx, log, msg, class, err, 1)
		c.recordProcessed(ResultDLQ, 1, started)
		return
	}

	log = log.With("correlation_id", env.CorrelationID.String(),
		"event_type", env.EventType)

	var lastErr error
	for attempt := 1; attempt <= c.cfg.MaxAttempts; attempt++ {
		lastErr = handle(ctx, env)
		if lastErr == nil {
			c.recordProcessed(ResultOK, attempt, started)
			return
		}

		switch Classify(lastErr) {
		case ClassDuplicate:
			// Expected under at-least-once delivery, not a problem. Logged at
			// info so the idempotency machinery is visibly working.
			log.Info("duplicate message ignored",
				"idempotency_key", env.IdempotencyKey)
			c.recordProcessed(ResultDuplicate, attempt, started)
			return

		case ClassPermanent:
			log.Error("permanent failure; dead-lettering",
				"attempt", attempt, "error", lastErr)
			c.deadLetter(ctx, log, msg, errorClass(lastErr), lastErr, attempt)
			c.recordProcessed(ResultDLQ, attempt, started)
			return

		case ClassTransient:
			if attempt == c.cfg.MaxAttempts {
				break
			}
			if ctx.Err() != nil {
				return
			}
			delay := c.backoff(attempt)
			log.Warn("transient failure; retrying",
				"attempt", attempt, "retry_in", delay, "error", lastErr)
			select {
			case <-time.After(delay):
			case <-ctx.Done():
				return
			}
		}
	}

	log.Error("retries exhausted; dead-lettering",
		"attempts", c.cfg.MaxAttempts, "error", lastErr)
	c.deadLetter(ctx, log, msg, "RETRY_EXHAUSTED", lastErr, c.cfg.MaxAttempts)
	c.recordProcessed(ResultDLQ, c.cfg.MaxAttempts, started)
}

// backoff returns an exponentially growing delay with jitter.
//
// The jitter is not decoration. When a shared dependency fails, every
// partition's message fails at once; without jitter they would all retry at
// exactly the same instants and hit the recovering service as a wave.
func (c *Consumer) backoff(attempt int) time.Duration {
	delay := c.cfg.InitialBackoff << (attempt - 1)
	if delay > c.cfg.MaxBackoff {
		delay = c.cfg.MaxBackoff
	}
	jitter := time.Duration(rand.Int63n(int64(delay/2) + 1))
	return delay/2 + jitter
}

// DeadLetter is the envelope written to a DLQ topic.
//
// The original message is preserved byte for byte so that, once the cause is
// fixed, it can be republished to the work topic unchanged.
type DeadLetter struct {
	// OriginalPayload holds the original bytes, base64-encoded. This is the
	// authoritative copy and the one the replay tool uses.
	//
	// It is not a json.RawMessage, and that is the whole point. The first
	// version of this struct embedded the bytes as raw JSON, which meant
	// encoding the dead letter failed whenever the original was malformed --
	// so the dead-letter queue silently dropped exactly the messages it
	// exists to capture. An integration test publishing `{"this is not: `
	// found it.
	OriginalPayload []byte `json:"original_payload"`

	// OriginalEvent is a decoded copy, present only when the original was
	// valid JSON. It exists so a human reading the DLQ in a console or in
	// Kafka UI can see the message without decoding anything.
	OriginalEvent json.RawMessage `json:"original_event,omitempty"`

	OriginalTopic     string `json:"original_topic"`
	OriginalPartition int    `json:"original_partition"`
	OriginalOffset    int64  `json:"original_offset"`
	OriginalKey       string `json:"original_key"`

	Failure DeadLetterFailure `json:"failure"`
}

// DeadLetterFailure describes why the message could not be processed.
type DeadLetterFailure struct {
	ErrorClass    string    `json:"error_class"`
	ErrorMessage  string    `json:"error_message"`
	Attempts      int       `json:"attempts"`
	FailedAt      time.Time `json:"failed_at"`
	Consumer      string    `json:"consumer"`
	ConsumerGroup string    `json:"consumer_group"`
	CorrelationID string    `json:"correlation_id,omitempty"`
}

func (c *Consumer) deadLetter(
	ctx context.Context, log *slog.Logger, msg kafka.Message,
	class string, cause error, attempts int,
) {
	if c.cfg.DLQTopic == "" || c.producer == nil {
		// No DLQ configured. Correct for a broadcast consumer: the durable
		// record is in PostgreSQL and a missed live notification is not worth
		// a second topic to manage.
		log.Warn("no DLQ configured; dropping message",
			"partition", msg.Partition, "offset", msg.Offset, "error_class", class)
		return
	}

	var correlationID string
	if env, err := events.Parse(msg.Value); err == nil {
		correlationID = env.CorrelationID.String()
	}

	letter := DeadLetter{
		OriginalPayload:   msg.Value,
		OriginalTopic:     msg.Topic,
		OriginalPartition: msg.Partition,
		OriginalOffset:    msg.Offset,
		OriginalKey:       string(msg.Key),
		Failure: DeadLetterFailure{
			ErrorClass:    class,
			ErrorMessage:  cause.Error(),
			Attempts:      attempts,
			FailedAt:      time.Now().UTC(),
			Consumer:      "pitchlab-processor",
			ConsumerGroup: c.cfg.GroupID,
			CorrelationID: correlationID,
		},
	}

	// Only attach the readable copy when it would not break the encoding.
	if json.Valid(msg.Value) {
		letter.OriginalEvent = json.RawMessage(msg.Value)
	}

	body, err := json.Marshal(letter)
	if err != nil {
		// Should now be unreachable: every field is encodable regardless of
		// what the original message contained. Losing the record of a failure
		// is worse than the failure, so it is still reported.
		log.Error("encode dead letter", "error", err,
			"partition", msg.Partition, "offset", msg.Offset)
		return
	}

	// A cancelled context must not stop a dead letter from being written;
	// losing it would lose the only record of the failure.
	writeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer cancel()

	if err := c.producer.writer.WriteMessages(writeCtx, kafka.Message{
		Topic: c.cfg.DLQTopic,
		Key:   msg.Key,
		Value: body,
		Headers: []kafka.Header{
			{Key: "error_class", Value: []byte(class)},
			{Key: "original_topic", Value: []byte(msg.Topic)},
			{Key: "correlation_id", Value: []byte(correlationID)},
		},
	}); err != nil {
		log.Error("write dead letter", "topic", c.cfg.DLQTopic, "error", err)
		return
	}

	if c.metrics.OnDLQ != nil {
		c.metrics.OnDLQ(msg.Topic, class)
	}
}

// Message outcomes, as reported to metrics.
const (
	ResultOK        = "ok"
	ResultDuplicate = "duplicate"
	ResultDLQ       = "dlq"
)

func (c *Consumer) recordFetchError(topic string) {
	if c.metrics.OnFetchError != nil {
		c.metrics.OnFetchError(topic)
	}
}

func (c *Consumer) recordProcessed(result string, attempts int, started time.Time) {
	if c.metrics.OnProcessed != nil {
		c.metrics.OnProcessed(c.cfg.Topic, result, attempts, time.Since(started))
	}
}

// parseErrorClass labels a failure that happened before the payload was
// readable at all.
func parseErrorClass(err error) string {
	var schemaErr *events.ErrUnsupportedSchema
	if errors.As(err, &schemaErr) {
		return "UNKNOWN_SCHEMA_VERSION"
	}
	switch {
	case errors.Is(err, events.ErrMissingEventID),
		errors.Is(err, events.ErrMissingEventType),
		errors.Is(err, events.ErrMissingCorrelationID),
		errors.Is(err, events.ErrMissingIdempotency),
		errors.Is(err, events.ErrMissingPartitionKey),
		errors.Is(err, events.ErrMissingOccurredAt),
		errors.Is(err, events.ErrEmptyData):
		return "SCHEMA_VALIDATION"
	}
	return "DESERIALIZATION"
}

func errorClass(err error) string {
	var perm *PermanentError
	if errors.As(err, &perm) {
		switch {
		case errors.Is(err, events.ErrMissingEventID),
			errors.Is(err, events.ErrMissingEventType),
			errors.Is(err, events.ErrMissingCorrelationID),
			errors.Is(err, events.ErrMissingIdempotency),
			errors.Is(err, events.ErrMissingPartitionKey),
			errors.Is(err, events.ErrEmptyData):
			return "SCHEMA_VALIDATION"
		}
		var schemaErr *events.ErrUnsupportedSchema
		if errors.As(err, &schemaErr) {
			return "UNKNOWN_SCHEMA_VERSION"
		}
		if isConstraintViolation(err) {
			return "PERSISTENCE_PERMANENT"
		}
		return "BUSINESS_RULE_VIOLATION"
	}
	return "UNKNOWN"
}
