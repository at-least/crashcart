// Package db owns the schema: versioned goose migrations under
// migrations/, applied on every start. internal/db/migrations/00001_baseline.sql
// is the whole schema as of the first goose release; every change after
// that is a new migration file.
package db

import (
	"context"
	"embed"
	"errors"
	"fmt"
	"io/fs"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jackc/pgx/v5/stdlib"
	"github.com/pressly/goose/v3"
	"github.com/pressly/goose/v3/lock"
)

//go:embed migrations/*.sql
var migrationsFS embed.FS

// migrationsDir is migrationsFS rooted at its migrations/ subdirectory, so
// goose sees plain "00001_baseline.sql"-style paths.
var migrationsDir = mustSub(migrationsFS, "migrations")

func mustSub(fsys embed.FS, dir string) fs.FS {
	sub, err := fs.Sub(fsys, dir)
	if err != nil {
		panic(err)
	}
	return sub
}

// Migrations exposes the embedded migration files, rooted so goose-style
// names appear directly ("00001_baseline.sql", not "migrations/…") — for
// callers that read their raw content: cmd/gendocs (enum checks against
// every migration, not just the baseline) and internal/testdb (hashing
// them to key the pgtestdb template).
func Migrations() fs.FS { return migrationsDir }

// ErrDatabaseAhead: the database has migrations applied that this binary's
// embedded migrations/ doesn't know about — an older binary against a
// newer database.
var ErrDatabaseAhead = errors.New("database schema is ahead of this binary")

// Init applies pending migrations and reports whether the database was
// empty beforehand (the baseline migration was the one applied). Safe to
// call from every replica at startup: goose's session locker (below)
// serializes them onto one connection at a time, so this needs no
// advisory lock of its own.
func Init(ctx context.Context, pool *pgxpool.Pool) (created bool, err error) {
	// OpenDBFromPool, not sql.Open(pool.Config().ConnString()): the pool's
	// ConnString() is the string it was originally parsed from and does not
	// reflect RuntimeParams set on it afterward (e.g. testdb's per-test
	// search_path) — OpenDBFromPool instead draws real connections from the
	// pool itself, so migrations run against the same database/schema this
	// process actually talks to.
	sqlDB := stdlib.OpenDBFromPool(pool)
	defer sqlDB.Close()

	locker, err := lock.NewPostgresSessionLocker()
	if err != nil {
		return false, err
	}
	// WithSessionLocker: goose takes one *sql.Conn from sqlDB, locks the
	// session on it (pg_try_advisory_lock, retried for up to 5 minutes),
	// and runs every migration on that same connection — never a second
	// one from the pool. A hand-rolled advisory lock held on a connection
	// pinned separately from the one migrations run on can self-deadlock
	// when the pool has no headroom to spare (MaxConns=1: the lock holder
	// and the migration runner would each need their own connection from
	// the same exhausted pool). See https://github.com/at-least/crashcart/issues/1
	// (RunAsLeader hit the same shape) and TestInitSingleConnection.
	provider, err := goose.NewProvider(goose.DialectPostgres, sqlDB, migrationsDir, goose.WithSessionLocker(locker))
	if err != nil {
		return false, err
	}
	// GetVersions doesn't take the session lock (goose's own doc note), so
	// two replicas racing here could each read a stale version — but this
	// is just a "refuse rather than run against a schema I don't know"
	// guard; a stale read only means Up (which does lock) proceeds and
	// does the right idempotent thing, never a wrong one.
	current, target, err := provider.GetVersions(ctx)
	if err != nil {
		return false, err
	}
	if current > target {
		return false, fmt.Errorf("%w: database is at migration %d, this binary only knows up to %d — upgrade the binary first",
			ErrDatabaseAhead, current, target)
	}
	results, err := provider.Up(ctx)
	if err != nil {
		return false, err
	}
	// created means the baseline (version 1, the empty-database case) was
	// applied by this call, not merely that some migration was — a later
	// migration reaching an existing, populated database is an upgrade,
	// not a creation.
	for _, r := range results {
		if r.Source != nil && r.Source.Version == 1 {
			created = true
		}
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
