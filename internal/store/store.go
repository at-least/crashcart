// Package store is the data-access layer: a pgx pool, hand-written queries,
// and the dynamic queries that were always hand-written (event listing
// with optional filters, tag breakdowns).
//
// Convention for queries: a package-level function per query, `func
// X(ctx, db DB, args...) (Row, error)`, taking DB explicitly rather than
// a *Store method — so a call site inside a Tx callback reads as a
// visible choice between the pool and the transaction (`X(ctx, s.Pool,
// ...)` vs `X(ctx, tx, ...)`) instead of an implicit one via method
// receiver. *Store methods remain only for composite/dynamic operations
// that pick pool-vs-tx internally (ListEvents, RotateProjectKey,
// PackWeek, InsertEvents). Row structs and enum types live in this
// package (no separate models package: every consumer already needs the
// pool/Tx to do anything with them). Scanning uses pgx.CollectRows /
// pgx.CollectExactlyOneRow with pgx.RowToStructByName[T] or a hand-written
// Scan helper — a plain `type X string` needs no Scan/Value methods for
// pgx to decode/encode a Postgres enum column, and CollectRows starts
// from []T{} (empty slice, not nil, preserved on the JSON wire for
// :many-shaped API responses). A struct that crosses the HTTP API
// boundary keeps its exact json:"snake_case" tags.
package store

import (
	"context"
	"log/slog"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/at-least/crashcart/internal/blob"
)

// DB is a pool or a transaction — whichever a hand-written query function
// is handed. Copied from sqlc's generated DBTX so query functions written
// before and after the migration have the same shape.
type DB interface {
	Exec(context.Context, string, ...any) (pgconn.CommandTag, error)
	Query(context.Context, string, ...any) (pgx.Rows, error)
	QueryRow(context.Context, string, ...any) pgx.Row
	SendBatch(context.Context, *pgx.Batch) pgx.BatchResults
}

// Store wraps the pool. Queries are usable directly (auto-commit) or via Tx.
type Store struct {
	Pool *pgxpool.Pool

	// Blobs is the object store symbol files and event payloads are
	// written to when BLOB_STORE=s3; nil (the default) keeps them in their
	// tables. A row is read the way it was written (symbol_files.data xor
	// blob_key; events.payload, else the spool, else a pack — packs.go), so
	// switching is safe at any time. Set once by cmd/crashcart.
	Blobs blob.Store

	// lockPool is a small pool dedicated to RunAsLeader's session-scoped
	// advisory locks, kept apart from Pool. A held lock pins a connection
	// for the whole duration of the caller's fn, and fn does its own work
	// through Pool/Queries — sharing one pool between the two would let
	// enough concurrent leader locks (one per tick() key in cmd/crashcart)
	// exhaust every connection on lock-holding alone, leaving none for fn
	// to actually run: a self-inflicted deadlock, not a load problem. See
	// https://github.com/at-least/crashcart/issues/1.
	lockPool *pgxpool.Pool
}

// maxLeaderLocks bounds lockPool: the number of distinct RunAsLeader keys
// that can be held at once by a single process (one per tick() call in
// cmd/crashcart/main.go — LeaderSpikeCheck, LeaderSweep, LeaderRollup,
// LeaderIgnoreCheck, LeaderMonitorCheck, LeaderPack; LeaderPartitions and
// LeaderSetup are transaction-scoped and never reach RunAsLeader). Each
// key maps to exactly one goroutine, so this can never be exceeded —
// sizing lockPool to it means a lock holder can never itself be starved
// waiting for a lock slot, whatever Pool's own size is.
const maxLeaderLocks = 6

// New builds a Store on an open pool, plus a small pool of its own for
// RunAsLeader's advisory locks (see lockPool).
func New(ctx context.Context, pool *pgxpool.Pool) (*Store, error) {
	lockCfg := pool.Config()
	lockCfg.MaxConns = maxLeaderLocks
	lockPool, err := pgxpool.NewWithConfig(ctx, lockCfg)
	if err != nil {
		return nil, err
	}
	return &Store{Pool: pool, lockPool: lockPool}, nil
}

// Tx runs fn inside a transaction. fn calls hand-written query functions
// with the tx it's given (`X(ctx, tx, ...)`), never s.Pool — that
// distinction is what TestTxCallbacksNeverUseThePool enforces.
func (s *Store) Tx(ctx context.Context, fn func(ctx context.Context, tx pgx.Tx) error) error {
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(context.WithoutCancel(ctx))
	if err := fn(ctx, tx); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// Leader keys for RunAsLeader (pg advisory lock ids).
const (
	LeaderSpikeCheck   int64 = 0x63726173 + 1 // "cras" + n
	LeaderSweep        int64 = 0x63726173 + 2
	LeaderRollup       int64 = 0x63726173 + 3
	LeaderPartitions   int64 = 0x63726173 + 4 // transaction-scoped: one partition creation at a time
	LeaderSetup        int64 = 0x63726173 + 5 // transaction-scoped: the first user is created once
	LeaderIgnoreCheck  int64 = 0x63726173 + 6 // ignored issues: time / count expiry and escalation
	LeaderMonitorCheck int64 = 0x63726173 + 7 // monitors: missed check-ins and timed-out runs
	LeaderPack         int64 = 0x63726173 + 8 // payload spool → packs in the blob store (packs.go)
)

// CreateFirstUser creates a user with the given email/name/passwordHash
// only while the users table is empty, under a transaction-scoped
// advisory lock so two concurrent setup posts cannot both succeed.
// created is false when a user already existed.
func (s *Store) CreateFirstUser(ctx context.Context, email, name, passwordHash string) (user User, created bool, err error) {
	err = s.Tx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, "SELECT pg_advisory_xact_lock($1)", LeaderSetup); err != nil {
			return err
		}
		n, err := CountUsers(ctx, tx)
		if err != nil || n > 0 {
			return err
		}
		user, err = CreateUser(ctx, tx, email, name, passwordHash)
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
	conn, err := s.lockPool.Acquire(ctx)
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

// LogPoolStats logs both pools' connection stats — a rising
// EmptyAcquireCount (an Acquire had to wait because none was immediately
// free) is the leading indicator of the exhaustion issue #1 describes,
// visible before it becomes a full hang. Not leader-elected: pool
// exhaustion is a per-process condition, so every replica logs its own.
func (s *Store) LogPoolStats(log *slog.Logger) {
	for name, p := range map[string]*pgxpool.Pool{"query": s.Pool, "lock": s.lockPool} {
		st := p.Stat()
		log.Info("pool stats", "pool", name, "acquired", st.AcquiredConns(), "idle", st.IdleConns(), "max", st.MaxConns(),
			"empty_acquire_count", st.EmptyAcquireCount(), "empty_acquire_wait", st.EmptyAcquireWaitTime())
	}
}

// Close closes both pools.
func (s *Store) Close() {
	s.lockPool.Close()
	s.Pool.Close()
}
