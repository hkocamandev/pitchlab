// Package db owns the PostgreSQL connection pool and the transactional
// building blocks the rest of the system uses.
package db

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// PoolConfig is the tunable surface of the connection pool. Defaults are set
// for a workload that mixes short analytical reads with a steady stream of
// small writes from the Kafka consumer.
type PoolConfig struct {
	URL string

	MaxConns int32
	MinConns int32

	// MaxConnLifetime bounds how long a connection is reused. Recycling
	// connections lets a restarted or failed-over database be picked up
	// without restarting the application.
	MaxConnLifetime time.Duration
	MaxConnIdleTime time.Duration

	ConnectTimeout time.Duration

	// Tracer observes every query. Typed as pgx's own interface so this
	// package stays free of any metrics dependency: the observability layer
	// supplies an implementation, and a test supplies none.
	Tracer pgx.QueryTracer
}

// DefaultPoolConfig returns sensible defaults for the given URL.
func DefaultPoolConfig(url string) PoolConfig {
	return PoolConfig{
		URL:             url,
		MaxConns:        20,
		MinConns:        2,
		MaxConnLifetime: time.Hour,
		MaxConnIdleTime: 15 * time.Minute,
		ConnectTimeout:  5 * time.Second,
	}
}

// NewPool opens and verifies a connection pool.
//
// The ping is deliberate: a pool that is configured but unreachable should
// fail at startup, where it is obvious, rather than on the first request.
func NewPool(ctx context.Context, cfg PoolConfig) (*pgxpool.Pool, error) {
	pcfg, err := pgxpool.ParseConfig(cfg.URL)
	if err != nil {
		return nil, fmt.Errorf("parse database url: %w", err)
	}

	pcfg.MaxConns = cfg.MaxConns
	pcfg.MinConns = cfg.MinConns
	pcfg.MaxConnLifetime = cfg.MaxConnLifetime
	pcfg.MaxConnIdleTime = cfg.MaxConnIdleTime
	pcfg.ConnConfig.ConnectTimeout = cfg.ConnectTimeout
	if cfg.Tracer != nil {
		pcfg.ConnConfig.Tracer = cfg.Tracer
	}

	pool, err := pgxpool.NewWithConfig(ctx, pcfg)
	if err != nil {
		return nil, fmt.Errorf("create pool: %w", err)
	}

	pingCtx, cancel := context.WithTimeout(ctx, cfg.ConnectTimeout)
	defer cancel()
	if err := pool.Ping(pingCtx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("ping database: %w", err)
	}

	return pool, nil
}
