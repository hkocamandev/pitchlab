package kafka

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/segmentio/kafka-go"
)

// Consumer lag is the pipeline's most important number: if it climbs, the
// system is not keeping up with what is being produced, and every other
// symptom follows from that.
//
// It is also the number the client library cannot give us. Reader.Lag() is
// documented as meaningful only outside a consumer group, and returns -1
// inside one; ReadLag refuses outright for a group reader. Both were tried,
// and reporting -1 as the headline metric is worse than reporting nothing.
//
// So it is computed the way a lag exporter computes it: ask the broker for the
// group's committed offset on each partition, ask for the log end offset of
// each partition, and subtract.

// LagSampleInterval is how often the brokers are asked.
//
// Ten seconds against a ten-second scrape: often enough that a climbing lag
// shows up within one graph refresh, rare enough that three small admin
// requests are invisible next to the message traffic.
const LagSampleInterval = 10 * time.Second

// LagSampler reports how far behind a consumer group is.
type LagSampler struct {
	client  *kafka.Client
	topic   string
	groupID string
	log     *slog.Logger
	report  func(topic string, partition int, lag int64)
}

// NewLagSampler builds a sampler.
func NewLagSampler(
	brokers []string, topic, groupID string, log *slog.Logger,
	report func(topic string, partition int, lag int64),
) *LagSampler {
	return &LagSampler{
		client: &kafka.Client{
			Addr:    kafka.TCP(brokers...),
			Timeout: 5 * time.Second,
		},
		topic:   topic,
		groupID: groupID,
		log:     log.With("component", "lag-sampler", "topic", topic, "group", groupID),
		report:  report,
	}
}

// Run samples until the context is cancelled.
func (s *LagSampler) Run(ctx context.Context) {
	ticker := time.NewTicker(LagSampleInterval)
	defer ticker.Stop()

	// One sample immediately, so a dashboard opened right after start shows a
	// number rather than a gap.
	s.sampleOnce(ctx)

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.sampleOnce(ctx)
		}
	}
}

func (s *LagSampler) sampleOnce(ctx context.Context) {
	lags, err := s.Sample(ctx)
	if err != nil {
		if ctx.Err() != nil {
			return
		}
		// A failed sample is a missing data point, never a reason to disturb
		// consumption. Debug rather than warn: a rebalance produces a few of
		// these and they are not worth waking anyone for.
		s.log.Debug("could not sample consumer lag", "error", err)
		return
	}
	for partition, lag := range lags {
		s.report(s.topic, partition, lag)
	}
}

// Sample returns the lag per partition.
func (s *LagSampler) Sample(ctx context.Context) (map[int]int64, error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	meta, err := s.client.Metadata(ctx, &kafka.MetadataRequest{
		Topics: []string{s.topic},
	})
	if err != nil {
		return nil, fmt.Errorf("metadata: %w", err)
	}

	var partitions []int
	for _, topic := range meta.Topics {
		if topic.Name != s.topic {
			continue
		}
		if topic.Error != nil {
			return nil, fmt.Errorf("topic %s: %w", s.topic, topic.Error)
		}
		for _, p := range topic.Partitions {
			partitions = append(partitions, p.ID)
		}
	}
	if len(partitions) == 0 {
		return nil, fmt.Errorf("topic %s has no partitions", s.topic)
	}

	committed, err := s.client.OffsetFetch(ctx, &kafka.OffsetFetchRequest{
		GroupID: s.groupID,
		Topics:  map[string][]int{s.topic: partitions},
	})
	if err != nil {
		return nil, fmt.Errorf("offset fetch: %w", err)
	}
	if committed.Error != nil {
		return nil, fmt.Errorf("offset fetch: %w", committed.Error)
	}

	offsetRequests := make(map[string][]kafka.OffsetRequest, 1)
	for _, p := range partitions {
		offsetRequests[s.topic] = append(offsetRequests[s.topic],
			kafka.LastOffsetOf(p))
	}

	ends, err := s.client.ListOffsets(ctx, &kafka.ListOffsetsRequest{
		Topics: offsetRequests,
	})
	if err != nil {
		return nil, fmt.Errorf("list offsets: %w", err)
	}

	end := make(map[int]int64, len(partitions))
	for _, ps := range ends.Topics {
		for _, p := range ps {
			end[p.Partition] = p.LastOffset
		}
	}

	out := make(map[int]int64, len(partitions))
	for _, fetched := range committed.Topics[s.topic] {
		logEnd, ok := end[fetched.Partition]
		if !ok {
			continue
		}

		// A group that has never committed on a partition reports -1. Its lag
		// is the whole partition, not a negative number: reporting the raw
		// value would draw a line below zero on the graph that means "we have
		// not started" rather than "we are ahead".
		offset := fetched.CommittedOffset
		if offset < 0 {
			offset = 0
		}

		lag := logEnd - offset
		if lag < 0 {
			lag = 0
		}
		out[fetched.Partition] = lag
	}

	return out, nil
}
