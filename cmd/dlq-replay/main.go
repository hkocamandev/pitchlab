// Command dlq-replay republishes dead letters to their original topic.
//
// A dead-letter queue that cannot be drained is just a place where failures
// go to be forgotten. The operational loop it completes is:
//
//	message fails  ->  DLQ  ->  alert  ->  read the error class and the
//	correlation id  ->  fix the cause  ->  replay
//
// Replay is safe because the consumer is idempotent: a message that was
// partially processed before failing will be recognised by its natural key and
// skipped rather than duplicated. That property is what makes this tool a
// one-liner rather than a reconciliation exercise.
//
//	go run ./cmd/dlq-replay --dlq pitchlab.pitch.raw.v1.dlq --dry-run
//	go run ./cmd/dlq-replay --dlq pitchlab.pitch.raw.v1.dlq --filter RETRY_EXHAUSTED
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	kgo "github.com/segmentio/kafka-go"

	pkafka "github.com/hkocamandev/pitchlab/internal/kafka"
)

func main() {
	if err := run(); err != nil {
		slog.Error("fatal", "error", err)
		os.Exit(1)
	}
}

func run() error {
	var (
		brokerList = flag.String("brokers", env("KAFKA_BROKERS", "localhost:9094"),
			"comma-separated broker addresses")
		dlqTopic = flag.String("dlq", "", "dead-letter topic to drain (required)")
		target   = flag.String("to", "",
			"topic to republish to (default: the original topic recorded in each letter)")
		filter = flag.String("filter", "",
			"only replay letters with this error class")
		dryRun = flag.Bool("dry-run", false,
			"report what would be replayed without publishing")
		limit   = flag.Int("limit", 0, "stop after this many letters (0 = all)")
		timeout = flag.Duration("timeout", 30*time.Second,
			"give up when the DLQ produces nothing for this long")
	)
	flag.Parse()

	if *dlqTopic == "" {
		flag.Usage()
		return errors.New("--dlq is required")
	}

	log := slog.New(slog.NewJSONHandler(os.Stdout, nil)).With("tool", "dlq-replay")
	brokers := strings.Split(*brokerList, ",")

	ctx, stop := signal.NotifyContext(context.Background(),
		syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	// Read without a consumer group and without committing.
	//
	// Deliberate: draining a DLQ is a manual, reviewable operation, and a
	// committed offset would mean a second run silently skipped letters that
	// the first one failed to republish. Re-reading from the beginning every
	// time is the safer default, and the idempotent consumer makes the
	// duplicate reads harmless.
	reader := kgo.NewReader(kgo.ReaderConfig{
		Brokers:   brokers,
		Topic:     *dlqTopic,
		Partition: 0,
		MinBytes:  1,
		MaxBytes:  10 << 20,
	})
	defer reader.Close()

	if err := reader.SetOffset(kgo.FirstOffset); err != nil {
		return fmt.Errorf("seek to the start of %s: %w", *dlqTopic, err)
	}

	log.Info("draining", "dlq", *dlqTopic, "filter", orAny(*filter), "dry_run", *dryRun)

	var read, replayed, skipped, failed int
	byClass := map[string]int{}

	for {
		if *limit > 0 && replayed >= *limit {
			break
		}

		readCtx, cancel := context.WithTimeout(ctx, *timeout)
		msg, err := reader.ReadMessage(readCtx)
		cancel()
		if err != nil {
			if ctx.Err() != nil {
				log.Info("interrupted")
			}
			break
		}
		read++

		var letter pkafka.DeadLetter
		if err := json.Unmarshal(msg.Value, &letter); err != nil {
			log.Error("dead letter is not decodable; leaving it in place",
				"offset", msg.Offset, "error", err)
			failed++
			continue
		}

		byClass[letter.Failure.ErrorClass]++

		if *filter != "" && letter.Failure.ErrorClass != *filter {
			skipped++
			continue
		}

		topic := letter.OriginalTopic
		if *target != "" {
			topic = *target
		}
		if topic == "" {
			log.Error("no destination topic recorded and none given",
				"offset", msg.Offset)
			failed++
			continue
		}

		entry := log.With(
			"error_class", letter.Failure.ErrorClass,
			"correlation_id", letter.Failure.CorrelationID,
			"original_topic", letter.OriginalTopic,
			"original_offset", letter.OriginalOffset,
		)

		if *dryRun {
			entry.Info("would replay", "to", topic)
			replayed++
			continue
		}

		// The original bytes, not the decoded copy. The decoded copy is only
		// present when the message was valid JSON, and a malformed message is
		// exactly the kind that needs replaying after a producer fix.
		if err := publishRaw(ctx, brokers, topic, letter.OriginalKey, letter.OriginalPayload); err != nil {
			entry.Error("replay failed", "error", err)
			failed++
			continue
		}

		entry.Info("replayed", "to", topic)
		replayed++
	}

	log.Info("done",
		"read", read, "replayed", replayed, "skipped", skipped, "failed", failed)
	for class, n := range byClass {
		log.Info("error class", "class", class, "count", n)
	}

	if failed > 0 {
		return fmt.Errorf("%d letters could not be replayed", failed)
	}
	return nil
}

// publishRaw writes the preserved bytes back to a work topic unchanged.
//
// The envelope is not rebuilt: its event id, correlation id and idempotency
// key must survive, or the replayed message would look like a new event and
// the consumer's duplicate detection would not fire.
func publishRaw(ctx context.Context, brokers []string, topic, key string, payload []byte) error {
	w := &kgo.Writer{
		Addr:         kgo.TCP(brokers...),
		Topic:        topic,
		Balancer:     &kgo.Hash{},
		RequiredAcks: kgo.RequireAll,
		Transport:    &kgo.Transport{MetadataTTL: time.Second},
	}
	defer w.Close()

	return w.WriteMessages(ctx, kgo.Message{
		Key:   []byte(key),
		Value: payload,
		Headers: []kgo.Header{
			{Key: "replayed_from_dlq", Value: []byte("true")},
		},
	})
}

func env(key, fallback string) string {
	if v, ok := os.LookupEnv(key); ok && v != "" {
		return v
	}
	return fallback
}

func orAny(s string) string {
	if s == "" {
		return "(any)"
	}
	return s
}
