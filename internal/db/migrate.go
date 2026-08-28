// Package db owns the schema: embedded migrations and the migrator.
package db

import (
	"context"
	"embed"
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

// Migrate applies every pending migration in filename order. Each file runs
// in its own transaction (TimescaleDB DDL is transactional).
func Migrate(ctx context.Context, pool *pgxpool.Pool) ([]string, error) {
	conn, err := pool.Acquire(ctx)
	if err != nil {
		return nil, err
	}
	defer conn.Release()

	if _, err := conn.Exec(ctx, "SELECT pg_advisory_lock($1)", migrationLock); err != nil {
		return nil, err
	}
	defer conn.Exec(context.WithoutCancel(ctx), "SELECT pg_advisory_unlock($1)", migrationLock)

	if _, err := conn.Exec(ctx, `CREATE TABLE IF NOT EXISTS schema_migrations (
		name TEXT PRIMARY KEY, applied_at TIMESTAMPTZ NOT NULL DEFAULT now())`); err != nil {
		return nil, err
	}
	applied := map[string]bool{}
	rows, err := conn.Query(ctx, "SELECT name FROM schema_migrations")
	if err != nil {
		return nil, err
	}
	names, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return nil, err
	}
	for _, n := range names {
		applied[n] = true
	}

	files, err := fs.ReadDir(migrations, "migrations")
	if err != nil {
		return nil, err
	}
	sort.Slice(files, func(i, j int) bool { return files[i].Name() < files[j].Name() })
	var ran []string
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
