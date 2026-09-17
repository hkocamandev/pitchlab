// Command poison publishes one deliberately malformed event into the raw
// pitch topic.
//
// It exists so the dead-letter path can be demonstrated on purpose. Every
// other way of producing a poison message -- corrupting a tape, stopping a
// dependency mid-flight, editing a row behind the pipeline's back -- also
// breaks something else, which makes it unclear afterwards what was being
// shown. This publishes exactly one message that the consumer cannot parse
// and changes nothing else.
//
// What it demonstrates: the consumer classifies the failure, writes a dead
// letter carrying the original bytes and the reason, commits the offset, and
// carries on. A single bad message must not stop a partition.
//
//	go run ./cmd/poison
//	make dlq          # the letter, with the class it was filed under
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/segmentio/kafka-go"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func run() error {
	var (
		brokerList = flag.String("brokers", env("KAFKA_BROKERS", "localhost:9094"),
			"comma-separated broker addresses")
		topic = flag.String("topic", "pitchlab.pitch.raw.v1",
			"topic to publish the malformed event to")
		key = flag.String("key", "demo:poison",
			"partition key; the letter lands on the partition this hashes to")
	)
	flag.Parse()

	// A syntactically valid JSON object that is not a valid envelope: the
	// event id is empty, so parsing fails on a field the consumer needs
	// before it can do anything else.
	//
	// Valid JSON on purpose. Raw bytes that are not JSON at all take a
	// different path -- the dead letter then cannot carry a readable copy --
	// and the path worth showing is the ordinary one, where a producer sent
	// something well-formed and wrong.
	payload := []byte(`{"event_id":"","event_type":"pitchlab.pitch.raw","data":{}}`)

	w := &kafka.Writer{
		Addr:         kafka.TCP(strings.Split(*brokerList, ",")...),
		Topic:        *topic,
		Balancer:     &kafka.Hash{},
		RequiredAcks: kafka.RequireAll,
	}
	defer func() { _ = w.Close() }()

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	if err := w.WriteMessages(ctx, kafka.Message{
		Key:   []byte(*key),
		Value: payload,
	}); err != nil {
		return fmt.Errorf("publish to %s: %w", *topic, err)
	}

	fmt.Printf("published one malformed event to %s\n", *topic)
	fmt.Println("it will be dead-lettered; see it with: make dlq")
	return nil
}

func env(key, fallback string) string {
	if v, ok := os.LookupEnv(key); ok && v != "" {
		return v
	}
	return fallback
}
