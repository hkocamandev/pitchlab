// Package cache is the Redis layer.
//
// Everything here is optional by construction. A nil *Cache is a valid cache
// that always misses, and every method tolerates being called on one, so no
// caller needs an "if the cache is configured" branch. That is not a
// convenience: it is how the promise that Redis is not critical is kept in the
// type system rather than in reviewers' memory.
//
// The same idea runs through the error handling. No method returns an error
// for a Redis failure. A read that fails is a miss, a write that fails is
// logged and forgotten, and the caller carries on against PostgreSQL. There is
// no code path in which a Redis problem can become a failed request.
package cache

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/redis/go-redis/v9"
)

// Timeout bounds every Redis call.
//
// Short on purpose. A cache exists to make things faster; without a tight
// deadline a degraded Redis turns it into a tax paid on every request, which
// is worse than not having one. Fifty milliseconds is far beyond a healthy
// round trip on a local network and far below anything a user would notice.
const Timeout = 50 * time.Millisecond

// Key lifetimes.
const (
	// LiveTTL keeps an outing's counters around well past the end of a game,
	// so a dashboard opened late still finds them, and lets them expire on
	// their own afterwards. Nothing has to clean them up.
	LiveTTL = 6 * time.Hour

	// SummaryTTL is how long an analytics rollup may be stale if nothing
	// invalidates it first. Event-based invalidation is the primary
	// mechanism; this is the safety net for the case where it is missed.
	SummaryTTL = 300 * time.Second

	// GenerationTTL must comfortably exceed SummaryTTL.
	//
	// The generation counter is what makes a cached entry addressable. If it
	// expired while entries from an older generation were still alive, the
	// counter would restart at 1 and could name an entry that is no longer
	// current. Twenty-four hours against five minutes leaves no overlap.
	GenerationTTL = 24 * time.Hour
)

// Result labels for the metrics callback.
//
// A write has no "hit": it either happened or it did not. Reporting a
// successful write as a hit inflated the hit ratio with operations that were
// never lookups, which is the kind of metric that looks healthy precisely
// because it is measuring the wrong thing.
const (
	ResultHit   = "hit"
	ResultMiss  = "miss"
	ResultOK    = "ok"
	ResultError = "error"
)

// Metrics receives the outcome of every cache operation.
//
// Nil is fine. Instrumentation is wired in the observability phase; the cache
// works, and is tested, without it.
type Metrics struct {
	OnOperation func(name, result string)
}

func (m Metrics) record(name, result string) {
	if m.OnOperation != nil {
		m.OnOperation(name, result)
	}
}

// Cache is the Redis client.
type Cache struct {
	rdb     *redis.Client
	log     *slog.Logger
	metrics Metrics
}

// Config configures the connection.
type Config struct {
	URL string
	// PoolSize has to keep up with the number of requests that can be in
	// flight at once. Measured: with ten connections and fifty concurrent HTTP
	// workers, a fraction of calls queued past the 50ms deadline and fell
	// through to PostgreSQL -- correct behaviour, but the cache was silently
	// doing less work than it appeared to.
	PoolSize int
	// MinIdleConns pre-opens connections so the first requests after start do
	// not each pay a dial inside the 50ms budget. Without it the errors are
	// harmless -- the call falls through to PostgreSQL -- but they are also
	// misleading: an error counter that always shows a burst at startup is one
	// people learn to ignore.
	MinIdleConns int
	DialTimeout  time.Duration
	ReadTimeout  time.Duration
	WriteTimeout time.Duration
}

// DefaultConfig returns sensible defaults.
func DefaultConfig(url string) Config {
	return Config{
		URL:          url,
		PoolSize:     64,
		MinIdleConns: 8,
		DialTimeout:  Timeout,
		ReadTimeout:  Timeout,
		WriteTimeout: Timeout,
	}
}

// New builds a cache.
//
// It does not connect, and it does not check that Redis is reachable. A
// process must be able to start with Redis down -- refusing to boot over a
// cache would make the cache critical, which is the one thing this layer must
// never be.
func New(cfg Config, log *slog.Logger) (*Cache, error) {
	opts, err := redis.ParseURL(cfg.URL)
	if err != nil {
		return nil, err
	}

	opts.PoolSize = cfg.PoolSize
	opts.MinIdleConns = cfg.MinIdleConns
	opts.DialTimeout = cfg.DialTimeout
	opts.ReadTimeout = cfg.ReadTimeout
	opts.WriteTimeout = cfg.WriteTimeout

	// No retries. The library's default is three attempts with backoff, which
	// against an unreachable Redis means three dial timeouts stacked inside a
	// request. One attempt, one deadline, then fall through to PostgreSQL.
	opts.MaxRetries = -1

	return &Cache{
		rdb: redis.NewClient(opts),
		log: log.With("component", "cache"),
	}, nil
}

// WithMetrics attaches counters.
func (c *Cache) WithMetrics(m Metrics) *Cache {
	if c == nil {
		return nil
	}
	c.metrics = m
	return c
}

// Close releases the connection pool.
func (c *Cache) Close() error {
	if c == nil || c.rdb == nil {
		return nil
	}
	return c.rdb.Close()
}

// Ping reports whether Redis is reachable. It is what /readyz uses to decide
// between "ok" and "degraded" -- never between "ready" and "not ready".
func (c *Cache) Ping(ctx context.Context) error {
	if c == nil || c.rdb == nil {
		return errors.New("cache is not configured")
	}
	ctx, cancel := context.WithTimeout(ctx, Timeout)
	defer cancel()
	return c.rdb.Ping(ctx).Err()
}

// enabled reports whether there is anything to talk to.
func (c *Cache) enabled() bool { return c != nil && c.rdb != nil }

// call runs one Redis operation under the timeout.
//
// It converts a failure into a logged miss. The error is deliberately not
// returned: giving callers an error to handle is giving them a chance to
// handle it wrongly, and the only correct handling is to ignore it.
//
// success is the label recorded when the operation works -- "hit" for a
// lookup, "ok" for a write -- because a hit ratio computed over writes is not
// a hit ratio.
func (c *Cache) call(
	ctx context.Context, name, success string, fn func(context.Context) error,
) bool {
	if !c.enabled() {
		return false
	}

	ctx, cancel := context.WithTimeout(ctx, Timeout)
	defer cancel()

	err := fn(ctx)
	switch {
	case err == nil:
		c.metrics.record(name, success)
		return true
	case errors.Is(err, redis.Nil):
		// Not an error: the key is simply not there.
		c.metrics.record(name, ResultMiss)
		return false
	default:
		c.metrics.record(name, ResultError)
		c.log.Warn("cache operation failed; falling through",
			"operation", name, "error", err)
		return false
	}
}
