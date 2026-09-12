package db

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/hkocamandev/pitchlab/internal/db/dbgen"
)

// ErrDuplicate reports that a row already existed and nothing was written.
//
// This is not a failure. Kafka delivers at least once, so the consumer will
// see the same pitch again whenever it dies between doing its work and
// committing its offset. Recognising the duplicate and moving on is how the
// pipeline stays correct without asking the broker for guarantees it cannot
// give.
var ErrDuplicate = errors.New("row already exists")

// Store is the database entry point: a pool, the generated queries, and a
// transaction helper.
type Store struct {
	pool *pgxpool.Pool
	*dbgen.Queries
}

// NewStore wraps a pool.
func NewStore(pool *pgxpool.Pool) *Store {
	return &Store{pool: pool, Queries: dbgen.New(pool)}
}

// Pool exposes the underlying pool for health checks and ad-hoc queries.
func (s *Store) Pool() *pgxpool.Pool { return s.pool }

// Close releases the pool.
func (s *Store) Close() { s.pool.Close() }

// InTx runs fn inside a transaction, rolling back on error or panic.
//
// The rollback is deferred rather than written at each error return, so a
// path added later cannot forget it.
func (s *Store) InTx(ctx context.Context, fn func(*dbgen.Queries) error) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin transaction: %w", err)
	}

	committed := false
	defer func() {
		if !committed {
			// Rollback on an already-finished transaction is a no-op, and the
			// context may already be cancelled, so the error is not actionable.
			_ = tx.Rollback(context.WithoutCancel(ctx))
		}
	}()

	if err := fn(s.Queries.WithTx(tx)); err != nil {
		return err
	}

	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit transaction: %w", err)
	}
	committed = true
	return nil
}

// Healthy reports whether the database answers.
func (s *Store) Healthy(ctx context.Context) error {
	return s.pool.Ping(ctx)
}

// IsNoRows reports whether err means "nothing matched".
//
// Several queries use this as a signal rather than an error: an insert with
// ON CONFLICT DO NOTHING returns no row when the row already existed, and a
// guarded UPDATE returns none when the state transition had already happened.
func IsNoRows(err error) bool {
	return errors.Is(err, pgx.ErrNoRows)
}

// NewID returns a time-ordered identifier.
//
// UUIDv7 rather than v4: the random layout of v4 scatters inserts across a
// B-tree, causing page splits and poor cache locality on a table that is
// almost entirely append-only. v7 sorts by creation time, so inserts land at
// the right edge of the index.
func NewID() uuid.UUID {
	id, err := uuid.NewV7()
	if err != nil {
		// NewV7 only fails if the system entropy source does, which is not a
		// condition this application can meaningfully continue through.
		return uuid.New()
	}
	return id
}

// ActivateModelVersion makes one model version the active one for its variant.
//
// Deactivate and activate are separate statements inside one transaction
// because a data-modifying CTE cannot express "then": every CTE in a
// statement observes the same snapshot, so the deactivation would be
// invisible to the activation and the partial unique index would reject the
// write.
//
// The index still does the real work. Two deployments activating different
// versions concurrently cannot both succeed; one fails, which is the correct
// outcome and is not something application-level sequencing could guarantee.
func (s *Store) ActivateModelVersion(ctx context.Context, id uuid.UUID) (dbgen.ModelVersion, error) {
	var activated dbgen.ModelVersion

	err := s.InTx(ctx, func(q *dbgen.Queries) error {
		target, err := q.GetModelVersion(ctx, id)
		if err != nil {
			return fmt.Errorf("load model version: %w", err)
		}
		if err := q.DeactivateModelVariant(ctx, target.Variant); err != nil {
			return fmt.Errorf("deactivate current %s model: %w", target.Variant, err)
		}
		activated, err = q.MarkModelVersionActive(ctx, id)
		if err != nil {
			return fmt.Errorf("activate model version: %w", err)
		}
		return nil
	})

	return activated, err
}
