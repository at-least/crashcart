// Package db owns the schema: versioned goose migrations under
// migrations/, applied on every start. internal/db/migrations/00001_baseline.sql
// is everything up to the last pre-migration release (schema version 15);
// every change after that is a new migration file.
package db

import (
	"context"
	"embed"
	"errors"
	"fmt"
	"io/fs"

	"github.com/jackc/pgx/v5"
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

// lastPreMigrationVersion is the schema version (crashcart_schema.version)
// the last binary before goose migrations wrote. A database at this
// version is bootstrapped in place: 00001_baseline.sql is exactly that
// schema, so it is marked applied without being re-run.
const lastPreMigrationVersion = 15

// ErrLegacySchemaVersion: an existing database predates goose migrations
// but isn't at the one version (lastPreMigrationVersion) this binary knows
// how to bootstrap in place.
var ErrLegacySchemaVersion = errors.New("legacy schema version mismatch")

// ErrDatabaseAhead: the database has migrations applied that this binary's
// embedded migrations/ doesn't know about — an older binary against a
// newer database.
var ErrDatabaseAhead = errors.New("database schema is ahead of this binary")

// initLock is the advisory lock key so replicas can start together.
const initLock = 0x6372617368 // "crash"

// Init applies pending migrations, bootstrapping a legacy (pre-migration)
// database in place first if needed, and reports whether the database was
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

	if hasProjects {
		if err := bootstrapLegacy(ctx, conn.Conn()); err != nil {
			return false, err
		}
	}

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

// bootstrapLegacy marks 00001_baseline.sql as already applied on a
// database created by a pre-migration binary (crashcart_schema.version ==
// lastPreMigrationVersion), instead of re-running its DDL against tables
// that already exist. Refuses any other legacy version: this binary only
// knows how to bootstrap from the one version immediately before it.
func bootstrapLegacy(ctx context.Context, conn *pgx.Conn) error {
	var isLegacy bool
	if err := conn.QueryRow(ctx, "SELECT to_regclass('crashcart_schema') IS NOT NULL").Scan(&isLegacy); err != nil {
		return err
	}
	if !isLegacy {
		return nil // already on goose (or a from-scratch fresh database, handled by Up itself)
	}

	var version int
	if err := conn.QueryRow(ctx, "SELECT version FROM crashcart_schema LIMIT 1").Scan(&version); err != nil {
		return fmt.Errorf("read legacy schema version: %w", err)
	}
	if version != lastPreMigrationVersion {
		return fmt.Errorf("%w: database is at legacy schema version %d, this binary can only bootstrap from version %d — "+
			"upgrade through the last pre-migration release first",
			ErrLegacySchemaVersion, version, lastPreMigrationVersion)
	}

	tx, err := conn.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(context.WithoutCancel(ctx))

	// The exact table goose itself creates (internal/dialects/postgres.go),
	// so provider.GetVersions/Up see the baseline as already applied.
	if _, err := tx.Exec(ctx, `CREATE TABLE goose_db_version (
		id integer PRIMARY KEY GENERATED BY DEFAULT AS IDENTITY,
		version_id bigint NOT NULL,
		is_applied boolean NOT NULL,
		tstamp timestamp NOT NULL DEFAULT now()
	)`); err != nil {
		return fmt.Errorf("create goose_db_version: %w", err)
	}
	if _, err := tx.Exec(ctx, `INSERT INTO goose_db_version (version_id, is_applied) VALUES (0, true), (1, true)`); err != nil {
		return fmt.Errorf("seed goose_db_version: %w", err)
	}
	if _, err := tx.Exec(ctx, "DROP TABLE crashcart_schema"); err != nil {
		return fmt.Errorf("drop crashcart_schema: %w", err)
	}
	return tx.Commit(ctx)
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
