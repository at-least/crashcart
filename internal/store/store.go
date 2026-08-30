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
	LeaderSpikeCheck  int64 = 0x63726173 + 1 // "cras" + n
	LeaderSweep       int64 = 0x63726173 + 2
	LeaderRollup      int64 = 0x63726173 + 3
	LeaderPartitions  int64 = 0x63726173 + 4 // transaction-scoped: one partition creation at a time
	LeaderSetup       int64 = 0x63726173 + 5 // transaction-scoped: the first user is created once
	LeaderIgnoreCheck int64 = 0x63726173 + 6 // ignored issues: time / count expiry and escalation
)

// CreateFirstUser creates u only while the users table is empty, under a
// transaction-scoped advisory lock so two concurrent setup posts cannot
// both succeed. created is false when a user already existed.
func (s *Store) CreateFirstUser(ctx context.Context, u sqlc.CreateUserParams) (user sqlc.User, created bool, err error) {
	err = s.Tx(ctx, func(ctx context.Context, tx pgx.Tx, q *sqlc.Queries) error {
		if _, err := tx.Exec(ctx, "SELECT pg_advisory_xact_lock($1)", LeaderSetup); err != nil {
			return err
		}
		n, err := q.CountUsers(ctx)
		if err != nil || n > 0 {
			return err
		}
		user, err = q.CreateUser(ctx, u)
		created = err == nil
		return err
	})
	return user, created, err
}

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
