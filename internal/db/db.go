// Package db owns the schema: versioned tern migrations under
// migrations/, applied on every start. internal/db/migrations/00001_baseline.sql
// is the whole schema as of the first tern release; every change after
// that is a new migration file.
package db

import (
	"context"
	"embed"
	"errors"
	"fmt"
	"io/fs"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jackc/tern/v2/migrate"
)

//go:embed migrations/*.sql
var migrationsFS embed.FS

// migrationsDir is migrationsFS rooted at its migrations/ subdirectory, so
// tern sees plain "00001_baseline.sql"-style paths.
var migrationsDir = mustSub(migrationsFS, "migrations")

func mustSub(fsys embed.FS, dir string) fs.FS {
	sub, err := fs.Sub(fsys, dir)
	if err != nil {
		panic(err)
	}
	return sub
}

// Migrations exposes the embedded migration files, rooted so tern-style
// names appear directly ("00001_baseline.sql", not "migrations/…") — for
// callers that read their raw content: cmd/gendocs (enum checks against
// every migration, not just the baseline) and internal/testdb (hashing
// them to key the pgtestdb template).
func Migrations() fs.FS { return migrationsDir }

// ErrDatabaseAhead: the database has migrations applied that this binary's
// embedded migrations/ doesn't know about — an older binary against a
// newer database.
var ErrDatabaseAhead = errors.New("database schema is ahead of this binary")

// VersionTable must stay unqualified (not schema-qualified, despite
// tern's own recommendation): it is created and read through whatever
// connection a Migrator runs on, so it lands in that connection's
// search_path — the same schema its migrations do. A schema-qualified
// name would pin every caller (including every concurrently-schema'd
// test database, and internal/testdb's own template databases) to the
// same row instead of one per schema. See TestInitConcurrently.
const VersionTable = "schema_version"

// initLockTimeout bounds Init's wait for tern's pg_advisory_lock, which
// otherwise blocks indefinitely (no built-in retry-then-give-up, unlike
// goose's session locker before it). Without this, a caller's own ctx
// (cmd/crashcart's is only canceled by SIGINT/SIGTERM) would let a
// replica stuck mid-migration wedge every other replica's startup
// forever with no error.
const initLockTimeout = 5 * time.Minute

// Init applies pending migrations and reports whether the database was
// empty beforehand (the baseline migration was the one applied). Safe to
// call from every replica at startup: tern's Migrator takes one
// connection out of the pool for its own advisory lock and every
// migration statement, so this needs no locking of its own, and never
// contends the pool for a second connection while holding the lock — a
// lock held on a connection pinned separately from the one migrations
// run on can self-deadlock when the pool has no headroom to spare
// (MaxConns=1: the lock holder and the migration runner would each need
// their own connection from the same exhausted pool). See
// https://github.com/at-least/crashcart/issues/1 (RunAsLeader hit the
// same shape) and TestInitSingleConnection.
func Init(ctx context.Context, pool *pgxpool.Pool) (created bool, err error) {
	ctx, cancel := context.WithTimeout(ctx, initLockTimeout)
	defer cancel()

	pc, err := pool.Acquire(ctx)
	if err != nil {
		return false, err
	}
	conn := pc.Hijack() // ours until closed: tern's session-level advisory lock cannot go back to the pool
	defer conn.Close(context.WithoutCancel(ctx))

	m, err := migrate.NewMigrator(ctx, conn, VersionTable)
	if err != nil {
		return false, err
	}
	if err := m.LoadMigrations(migrationsDir); err != nil {
		return false, err
	}

	// GetCurrentVersion doesn't take the advisory lock (it's a plain
	// SELECT), so two replicas racing here could each read a stale
	// version — but this is just a "refuse rather than run against a
	// schema I don't know" guard; a stale read only means Migrate (which
	// does lock) proceeds and does the right idempotent thing, never a
	// wrong one.
	before, err := m.GetCurrentVersion(ctx)
	if err != nil {
		return false, err
	}
	target := int32(len(m.Migrations))
	if before > target {
		return false, fmt.Errorf("%w: database is at migration %d, this binary only knows up to %d — upgrade the binary first",
			ErrDatabaseAhead, before, target)
	}

	// created means the baseline was applied by *this* call, not merely
	// that the database ended up at version >= 1 — under
	// TestInitConcurrently every racing caller's pre-lock GetCurrentVersion
	// above reads the same stale 0, so `before == 0` can't tell the winner
	// from the five callers who found the schema already there and
	// applied nothing. OnStart only fires for a migration this call's
	// Migrator actually runs (tern re-reads the version after acquiring
	// its advisory lock; a losing caller's loop body never executes), so
	// it's the one signal scoped to this call. Sequence 1 is specifically
	// the baseline — a future 1→2 upgrade must report created=false.
	m.OnStart = func(sequence int32, name, direction, sql string) {
		if sequence == 1 {
			created = true
		}
	}
	if err := m.Migrate(ctx); err != nil {
		return false, err
	}
	return created, nil
}

// Connect opens a pool and pings it.
func Connect(ctx context.Context, url string) (*pgxpool.Pool, error) {
	cfg, err := pgxpool.ParseConfig(url)
	if err != nil {
		return nil, err
	}
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, err
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, err
	}
	return pool, nil
}
