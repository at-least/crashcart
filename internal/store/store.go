// Package store is the data-access layer: a pgx pool plus the sqlc-generated
// queries, and the few dynamic queries sqlc cannot express (event listing
// with optional filters, tag breakdowns).
package store

import (
	"context"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/crashcartapp/crashcart/internal/db/sqlc"
)

// Store wraps the pool. Queries are usable directly (auto-commit) or via Tx.
type Store struct {
	Pool *pgxpool.Pool
	*sqlc.Queries
}

// New builds a Store on an open pool.
func New(pool *pgxpool.Pool) *Store {
	return &Store{Pool: pool, Queries: sqlc.New(pool)}
}

// Payload is an event's raw payload, decoded. nil, nil when the event has
// none (imported without one).
func Payload(e sqlc.Event) ([]byte, error) {
	if len(e.Payload) == 0 {
		return nil, nil
	}
	return Gunzip(e.Payload)
}

// Tx runs fn inside a transaction with a transaction-scoped Queries.
func (s *Store) Tx(ctx context.Context, fn func(ctx context.Context, tx pgx.Tx, q *sqlc.Queries) error) error {
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(context.WithoutCancel(ctx))
	if err := fn(ctx, tx, s.Queries.WithTx(tx)); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// Leader keys for RunAsLeader (pg advisory lock ids).
const (
	LeaderSpikeCheck int64 = 0x63726173 + 1 // "cras" + n
	LeaderSweep      int64 = 0x63726173 + 2
	LeaderRollup     int64 = 0x63726173 + 3
	LeaderPartitions int64 = 0x63726173 + 4 // transaction-scoped: one partition creation at a time
)

// RunAsLeader runs fn while holding the session advisory lock key, and
// reports false without running it when another replica holds the lock —
// so scheduled work (sweeps, spike checks) runs once per
// deployment, not once per replica.
func (s *Store) RunAsLeader(ctx context.Context, key int64, fn func()) (bool, error) {
	conn, err := s.Pool.Acquire(ctx)
	if err != nil {
		return false, err
	}
	defer conn.Release()
	var got bool
	if err := conn.QueryRow(ctx, "SELECT pg_try_advisory_lock($1)", key).Scan(&got); err != nil {
		return false, err
	}
	if !got {
		return false, nil
	}
	defer conn.Exec(context.WithoutCancel(ctx), "SELECT pg_advisory_unlock($1)", key)
	fn()
	return true, nil
}

// Close closes the pool.
func (s *Store) Close() { s.Pool.Close() }
