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
	"sort"
	"strconv"
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
		limit      = flag.Int("limit", 0, "stop after this many letters (0 = all)")
		maxReplays = flag.Int("max-replays", 3,
			"refuse a letter that has already been replayed this many times (0 = no ceiling)")
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
	// Every partition, not partition zero.
	//
	// Dead letters are keyed by the original message key, so they are spread
	// across the topic exactly as the work was. A tool that reads one
	// partition reports an empty queue while letters sit in the other two --
	// which is worse than failing, because the operator believes the answer.
	partitions, err := partitionsOf(ctx, brokers, *dlqTopic)
	if err != nil {
		return err
	}

	log.Info("draining", "dlq", *dlqTopic, "partitions", len(partitions),
		"filter", orAny(*filter), "dry_run", *dryRun)

	var read, replayed, skipped, exhausted, failed int
	byClass := map[string]int{}

	drainPartition := func(
		ctx context.Context, log *slog.Logger, reader *kgo.Reader, partition int,
	) {
		for {
			if *limit > 0 && replayed >= *limit {
				return
			}

			readCtx, cancel := context.WithTimeout(ctx, *timeout)
			msg, err := reader.ReadMessage(readCtx)
			cancel()
			if err != nil {
				if ctx.Err() != nil {
					log.Info("interrupted")
				}
				// An idle partition is a finished partition: the letters are all
				// read, or there were none.
				return
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

			// The cycle guard, and it is not theoretical. Draining a dead-letter
			// queue back into the topic it came from means a permanently-broken
			// message fails again and is filed again; with nothing counting, the
			// next drain treats it as new. Measured once at twenty-nine letters
			// becoming twelve hundred in ten minutes, which also buries every
			// other letter in the queue.
			//
			// A letter that has exhausted its replays is left in place rather
			// than dropped: it is still the record of a failure, and the reason
			// it stopped being retried is now written down.
			if *maxReplays > 0 && letter.ReplayCount >= *maxReplays {
				log.Warn("letter has been replayed too often; leaving it in place",
					"offset", msg.Offset,
					"replay_count", letter.ReplayCount,
					"max_replays", *maxReplays,
					"error_class", letter.Failure.ErrorClass,
					"correlation_id", letter.Failure.CorrelationID)
				exhausted++
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
			if err := publishRaw(ctx, brokers, topic,
				letter.OriginalKey, letter.OriginalPayload, letter.ReplayCount+1); err != nil {
				entry.Error("replay failed", "error", err)
				failed++
				continue
			}

			entry.Info("replayed", "to", topic, "replay_count", letter.ReplayCount+1)
			replayed++
		}
	}

	// Partitions are drained one after another rather than concurrently. A
	// drain is an operator action taken while something is wrong; a readable,
	// ordered log of what happened is worth more than finishing sooner.
	for _, partition := range partitions {
		if *limit > 0 && replayed >= *limit {
			break
		}

		reader := kgo.NewReader(kgo.ReaderConfig{
			Brokers:   brokers,
			Topic:     *dlqTopic,
			Partition: partition,
			MinBytes:  1,
			MaxBytes:  10 << 20,
		})
		if err := reader.SetOffset(kgo.FirstOffset); err != nil {
			_ = reader.Close()
			return fmt.Errorf("seek to the start of %s partition %d: %w",
				*dlqTopic, partition, err)
		}

		drainPartition(ctx, log, reader, partition)
		_ = reader.Close()
	}

	log.Info("done",
		"read", read, "replayed", replayed, "skipped", skipped,
		"exhausted", exhausted, "failed", failed)
	for class, n := range byClass {
		log.Info("error class", "class", class, "count", n)
	}

	if failed > 0 {
		return fmt.Errorf("%d letters could not be replayed", failed)
	}
	return nil
}

// partitionsOf lists a topic's partitions.
//
// Asked of the cluster rather than assumed from configuration: a topic's
// partition count is a property of the cluster, and guessing it is how a
// drain quietly misses letters.
func partitionsOf(ctx context.Context, brokers []string, topic string) ([]int, error) {
	dialer := &kgo.Dialer{Timeout: 10 * time.Second}

	conn, err := dialer.DialContext(ctx, "tcp", brokers[0])
	if err != nil {
		return nil, fmt.Errorf("dial %s: %w", brokers[0], err)
	}
	defer func() { _ = conn.Close() }()

	parts, err := conn.ReadPartitions(topic)
	if err != nil {
		return nil, fmt.Errorf("read partitions of %s: %w", topic, err)
	}
	if len(parts) == 0 {
		return nil, fmt.Errorf("topic %s has no partitions", topic)
	}

	ids := make([]int, 0, len(parts))
	for _, p := range parts {
		ids = append(ids, p.ID)
	}
	sort.Ints(ids)
	return ids, nil
}

// publishRaw writes the preserved bytes back to a work topic unchanged.
//
// The envelope is not rebuilt: its event id, correlation id and idempotency
// key must survive, or the replayed message would look like a new event and
// the consumer's duplicate detection would not fire.
func publishRaw(
	ctx context.Context, brokers []string, topic, key string,
	payload []byte, replayCount int,
) error {
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
			// Read back by the consumer when it files the next dead letter,
			// which is what makes the ceiling above enforceable across round
			// trips. The old header said only that a replay had happened and
			// nothing ever read it -- true and useless.
			{Key: pkafka.ReplayCountHeader, Value: []byte(strconv.Itoa(replayCount))},
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
