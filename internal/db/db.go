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

// initLock is the advisory lock key so replicas can start together.
const initLock = 0x6372617368 // "crash"

// Init applies pending migrations and reports whether the database was
// empty beforehand. Safe to call from every replica at startup.
func Init(ctx context.Context, pool *pgxpool.Pool) (created bool, err error) {
	conn, err := pool.Acquire(ctx)
	if err != nil {
		return false, err
	}
	defer conn.Release()

	if _, err := conn.Exec(ctx, "SELECT pg_advisory_lock($1)", initLock); err != nil {
		return false, err
	}
	defer conn.Exec(context.WithoutCancel(ctx), "SELECT pg_advisory_unlock($1)", initLock)

	var hasProjects bool
	if err := conn.QueryRow(ctx, "SELECT to_regclass('projects') IS NOT NULL").Scan(&hasProjects); err != nil {
		return false, err
	}
	created = !hasProjects

	// OpenDBFromPool, not sql.Open(pool.Config().ConnString()): the pool's
	// ConnString() is the string it was originally parsed from and does not
	// reflect RuntimeParams set on it afterward (e.g. testdb's per-test
	// search_path) — OpenDBFromPool instead draws real connections from the
	// pool itself, so it always sees the same database/schema Init just
	// checked above.
	sqlDB := stdlib.OpenDBFromPool(pool)
	defer sqlDB.Close()

	provider, err := goose.NewProvider(goose.DialectPostgres, sqlDB, migrationsDir)
	if err != nil {
		return false, err
	}
	current, target, err := provider.GetVersions(ctx)
	if err != nil {
		return false, err
	}
	if current > target {
		return false, fmt.Errorf("%w: database is at migration %d, this binary only knows up to %d — upgrade the binary first",
			ErrDatabaseAhead, current, target)
	}
	if _, err := provider.Up(ctx); err != nil {
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
