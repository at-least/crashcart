// Package store is the data-access layer: a pgx pool plus the sqlc-generated
// queries, and the few dynamic queries sqlc cannot express (event listing
// with optional filters, tag breakdowns).
package store

import (
	"context"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/at-least/crashcart/internal/db/sqlc"
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

// Close closes the pool.
func (s *Store) Close() { s.Pool.Close() }
