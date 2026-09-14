package cache

import (
	"context"
	"encoding/json"

	"github.com/google/uuid"
)

// Generation returns the invalidation counter for an athlete's rollups.
//
// Zero means "no generation recorded", which is a perfectly good generation to
// cache under: the counter only ever has to distinguish before from after, not
// count anything.
func (c *Cache) Generation(ctx context.Context, athleteID uuid.UUID) int64 {
	if !c.enabled() {
		return 0
	}

	var gen int64
	c.call(ctx, "generation_read", func(ctx context.Context) error {
		var err error
		gen, err = c.rdb.Get(ctx, generationKey(athleteID)).Int64()
		return err
	})
	return gen
}

// InvalidateAthlete marks every cached rollup for an athlete as out of date.
//
// One INCR replaces every entry for that athlete regardless of how many query
// variants exist, without scanning the keyspace for keys to delete. The old
// entries are not removed; they become unaddressable and expire on their own.
//
// Invalidating rather than updating is deliberate. Updating the cache in place
// would require the writer -- the stream processor -- to know how the API
// computes its rollups, which puts the same logic in two services and makes
// the cache wrong the first time one of them changes. Deleting is simple, is
// always correct, and the next reader recomputes.
func (c *Cache) InvalidateAthlete(ctx context.Context, athleteID uuid.UUID) {
	if !c.enabled() {
		return
	}
	key := generationKey(athleteID)
	c.call(ctx, "generation_bump", func(ctx context.Context) error {
		pipe := c.rdb.Pipeline()
		pipe.Incr(ctx, key)
		pipe.Expire(ctx, key, GenerationTTL)
		_, err := pipe.Exec(ctx)
		return err
	})
}

// GetAnalytics reads a cached rollup into target.
//
// The generation is taken by the caller rather than read here, so that the
// same value can be used for the subsequent write -- otherwise a bump landing
// between the read and the write would store the fresh payload under the stale
// generation, where nothing would ever find it.
func (c *Cache) GetAnalytics(
	ctx context.Context, athleteID uuid.UUID, generation int64, variant string,
	target any,
) bool {
	if !c.enabled() {
		return false
	}

	var raw []byte
	ok := c.call(ctx, "analytics_read", func(ctx context.Context) error {
		var err error
		raw, err = c.rdb.Get(ctx,
			analyticsKey(athleteID, generation, variant)).Bytes()
		return err
	})
	if !ok || len(raw) == 0 {
		return false
	}

	if err := json.Unmarshal(raw, target); err != nil {
		// A cached entry that cannot be decoded is treated as absent rather
		// than as a failure. The schema version in the key is supposed to make
		// this impossible; if it happens anyway, recomputing is right and
		// serving a half-decoded payload is not.
		c.log.Warn("cached analytics could not be decoded; recomputing",
			"athlete_id", athleteID, "error", err)
		c.metrics.record("analytics_read", ResultError)
		return false
	}
	return true
}

// SetAnalytics stores a rollup.
func (c *Cache) SetAnalytics(
	ctx context.Context, athleteID uuid.UUID, generation int64, variant string,
	value any,
) {
	if !c.enabled() {
		return
	}

	raw, err := json.Marshal(value)
	if err != nil {
		c.log.Warn("analytics payload could not be encoded for caching",
			"athlete_id", athleteID, "error", err)
		return
	}

	c.call(ctx, "analytics_write", func(ctx context.Context) error {
		return c.rdb.Set(ctx,
			analyticsKey(athleteID, generation, variant), raw, SummaryTTL).Err()
	})
}
