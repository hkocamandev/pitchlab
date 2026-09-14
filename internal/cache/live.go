package cache

import (
	"context"
	"strconv"
	"time"

	"github.com/google/uuid"
)

// Hash field names for the live-session key.
const (
	fPitchCount  = "pitch_count"
	fSumSpeed    = "sum_speed"
	fCountSpeed  = "count_speed"
	fSumSpin     = "sum_spin"
	fCountSpin   = "count_spin"
	fSumScore    = "sum_score"
	fCountScore  = "count_score"
	fWhiffCount  = "whiff_count"
	fLastPitchAt = "last_pitch_at"
	fBalls       = "current_balls"
	fStrikes     = "current_strikes"
	fOuts        = "current_outs"
	fBatterID    = "current_batter_id"
)

// PitchObservation is one pitch's contribution to an outing's running totals.
//
// The pointers matter: a pitch with no spin reading must not be counted in the
// spin average. Summing zero would silently drag the average down, which is
// the kind of wrong number that survives review because it looks plausible.
type PitchObservation struct {
	ReleaseSpeed *float64
	ReleaseSpin  *float64
	Score        *float64
	IsWhiff      bool

	ThrownAt time.Time
	Balls    int
	Strikes  int
	Outs     int
	BatterID *uuid.UUID
}

// LiveMetrics is an outing's state as the cache knows it.
type LiveMetrics struct {
	PitchCount int64
	WhiffCount int64

	AvgReleaseSpeed *float64
	AvgReleaseSpin  *float64
	AvgPitchScore   *float64

	LastPitchAt *time.Time
	Balls       int
	Strikes     int
	Outs        int
	BatterID    *uuid.UUID
}

// RecordPitch folds one pitch into an outing's counters.
//
// This is the case Redis genuinely earns its place in. These counters change
// once per pitch, and in burst replay that is hundreds of writes a second to
// the same logical row. In PostgreSQL that means hundreds of UPDATEs against
// one row: lock contention, a new row version each time, and a table that
// needs vacuuming to stay usable. HINCRBY is atomic, lock-free and O(1).
//
// Losing them costs nothing, which is the other half of the argument. Every
// value here is recomputable from the pitches table, so the cache is an
// accelerator and never a record.
func (c *Cache) RecordPitch(
	ctx context.Context, sessionID uuid.UUID, obs PitchObservation,
) {
	if !c.enabled() {
		return
	}

	key := liveKey(sessionID)

	c.call(ctx, "live_record", func(ctx context.Context) error {
		// One round trip. Issuing eight commands separately would put eight
		// network latencies on the per-pitch path.
		pipe := c.rdb.Pipeline()

		pipe.HIncrBy(ctx, key, fPitchCount, 1)
		if obs.ReleaseSpeed != nil {
			pipe.HIncrByFloat(ctx, key, fSumSpeed, *obs.ReleaseSpeed)
			pipe.HIncrBy(ctx, key, fCountSpeed, 1)
		}
		if obs.ReleaseSpin != nil {
			pipe.HIncrByFloat(ctx, key, fSumSpin, *obs.ReleaseSpin)
			pipe.HIncrBy(ctx, key, fCountSpin, 1)
		}
		if obs.Score != nil {
			pipe.HIncrByFloat(ctx, key, fSumScore, *obs.Score)
			pipe.HIncrBy(ctx, key, fCountScore, 1)
		}
		if obs.IsWhiff {
			pipe.HIncrBy(ctx, key, fWhiffCount, 1)
		}

		fields := map[string]any{
			fBalls:   obs.Balls,
			fStrikes: obs.Strikes,
			fOuts:    obs.Outs,
		}
		if !obs.ThrownAt.IsZero() {
			fields[fLastPitchAt] = obs.ThrownAt.UTC().Format(time.RFC3339Nano)
		}
		if obs.BatterID != nil {
			fields[fBatterID] = obs.BatterID.String()
		}
		pipe.HSet(ctx, key, fields)

		// Renewed on every write, so the key lives as long as the outing does
		// and then goes away by itself.
		pipe.Expire(ctx, key, LiveTTL)

		_, err := pipe.Exec(ctx)
		return err
	})
}

// LiveSession reads an outing's running state.
//
// Reports false when there is nothing cached, which the caller answers by
// recomputing from PostgreSQL. A false here is never an error condition; it is
// the normal path with Redis absent.
func (c *Cache) LiveSession(
	ctx context.Context, sessionID uuid.UUID,
) (LiveMetrics, bool) {
	if !c.enabled() {
		return LiveMetrics{}, false
	}

	var raw map[string]string
	ok := c.call(ctx, "live_read", func(ctx context.Context) error {
		var err error
		raw, err = c.rdb.HGetAll(ctx, liveKey(sessionID)).Result()
		return err
	})
	// HGETALL answers an absent key with an empty map rather than redis.Nil,
	// so the emptiness has to be checked separately from the error.
	if !ok || len(raw) == 0 {
		return LiveMetrics{}, false
	}

	m := LiveMetrics{
		PitchCount: parseInt(raw[fPitchCount]),
		WhiffCount: parseInt(raw[fWhiffCount]),
		Balls:      int(parseInt(raw[fBalls])),
		Strikes:    int(parseInt(raw[fStrikes])),
		Outs:       int(parseInt(raw[fOuts])),
	}

	m.AvgReleaseSpeed = average(raw[fSumSpeed], raw[fCountSpeed])
	m.AvgReleaseSpin = average(raw[fSumSpin], raw[fCountSpin])
	m.AvgPitchScore = average(raw[fSumScore], raw[fCountScore])

	if ts, err := time.Parse(time.RFC3339Nano, raw[fLastPitchAt]); err == nil {
		m.LastPitchAt = &ts
	}
	if id, err := uuid.Parse(raw[fBatterID]); err == nil {
		m.BatterID = &id
	}

	return m, true
}

// DropLiveSession removes an outing's counters. Used when an outing ends and
// the authoritative aggregates have been written back to PostgreSQL.
func (c *Cache) DropLiveSession(ctx context.Context, sessionID uuid.UUID) {
	if !c.enabled() {
		return
	}
	c.call(ctx, "live_drop", func(ctx context.Context) error {
		return c.rdb.Del(ctx, liveKey(sessionID)).Err()
	})
}

// average divides a sum by its own count, so a field that was missing on some
// pitches is averaged over the pitches that actually had it.
func average(sum, count string) *float64 {
	n := parseInt(count)
	if n <= 0 {
		return nil
	}
	total, err := strconv.ParseFloat(sum, 64)
	if err != nil {
		return nil
	}
	avg := total / float64(n)
	return &avg
}

func parseInt(s string) int64 {
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		return 0
	}
	return n
}
