// Package db owns the schema: embedded migrations and the migrator.
package db

import (
	"context"
	"embed"
	"errors"
	"fmt"
	"io/fs"
	"sort"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

//go:embed migrations/*.sql
var migrations embed.FS

// migrationLock is the advisory lock key so replicas can start together.
const migrationLock = 0x6372617368 // "crash"

// ErrNoTimescale is returned when the database cannot run the Community
// (TSL-licensed) build of TimescaleDB, which the schema needs for
// compression and continuous aggregates. Most managed Postgres hosts ship
// the Apache-2 build (CREATE EXTENSION succeeds, those features fail) or
// none at all.
var ErrNoTimescale = errors.New("CrashCart needs TimescaleDB (Community build): use the timescale/timescaledb image, Tiger Cloud, or install the timescaledb package")

// Migrate applies every pending migration in filename order. Each file runs
// in its own transaction (TimescaleDB DDL is transactional).
func Migrate(ctx context.Context, pool *pgxpool.Pool) (ran []string, err error) {
	conn, err := pool.Acquire(ctx)
	if err != nil {
		return nil, err
	}
	defer conn.Release()

	if _, err := conn.Exec(ctx, "SELECT pg_advisory_lock($1)", migrationLock); err != nil {
		return nil, err
	}
	defer conn.Exec(context.WithoutCancel(ctx), "SELECT pg_advisory_unlock($1)", migrationLock)

	if err := requireTimescale(ctx, conn); err != nil {
		return nil, err
	}
	if _, err := conn.Exec(ctx, `CREATE TABLE IF NOT EXISTS schema_migrations (
		name TEXT PRIMARY KEY, applied_at TIMESTAMPTZ NOT NULL DEFAULT now())`); err != nil {
		return nil, err
	}
	rows, err := conn.Query(ctx, "SELECT name FROM schema_migrations")
	if err != nil {
		return nil, err
	}
	names, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return nil, err
	}
	applied := map[string]bool{}
	for _, n := range names {
		applied[n] = true
	}

	files, err := fs.ReadDir(migrations, "migrations")
	if err != nil {
		return nil, err
	}
	sort.Slice(files, func(i, j int) bool { return files[i].Name() < files[j].Name() })
	for _, f := range files {
		name := f.Name()
		if !strings.HasSuffix(name, ".sql") || applied[name] {
			continue
		}
		body, err := migrations.ReadFile("migrations/" + name)
		if err != nil {
			return ran, err
		}
		tx, err := conn.Begin(ctx)
		if err != nil {
			return ran, err
		}
		if _, err := tx.Exec(ctx, string(body)); err != nil {
			tx.Rollback(ctx)
			return ran, fmt.Errorf("migration %s: %w", name, err)
		}
		if _, err := tx.Exec(ctx, "INSERT INTO schema_migrations (name) VALUES ($1)", name); err != nil {
			tx.Rollback(ctx)
			return ran, err
		}
		if err := tx.Commit(ctx); err != nil {
			return ran, err
		}
		ran = append(ran, name)
	}
	return ran, nil
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

// requireTimescale creates the extension and checks it runs under the
// "timescale" (TSL) license — the Apache-2 build loads but refuses
// compression and continuous aggregates.
func requireTimescale(ctx context.Context, conn *pgxpool.Conn) error {
	if _, err := conn.Exec(ctx, "CREATE EXTENSION IF NOT EXISTS timescaledb"); err != nil {
		return fmt.Errorf("%w: %v", ErrNoTimescale, err)
	}
	var license string
	if err := conn.QueryRow(ctx, "SELECT current_setting('timescaledb.license', true)").Scan(&license); err != nil {
		return err
	}
	if license != "timescale" {
		return fmt.Errorf("%w (license is %q)", ErrNoTimescale, license)
	}
	return nil
}
