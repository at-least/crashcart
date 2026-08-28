// Package db owns the schema: embedded migrations, the runner that applies
// them, and the sqlc-generated query layer (subpackage sqlc).
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
var migrationFS embed.FS

// Migrate applies every migrations/*.sql not yet recorded in
// schema_migrations, in filename order, each in its own transaction.
// Safe to run concurrently: the version row is inserted first under a
// transactional advisory lock.
func Migrate(ctx context.Context, pool *pgxpool.Pool) ([]string, error) {
	files, err := fs.Glob(migrationFS, "migrations/*.sql")
	if err != nil {
		return nil, err
	}
	sort.Strings(files)

	if _, err := pool.Exec(ctx, `CREATE TABLE IF NOT EXISTS schema_migrations (
		version TEXT PRIMARY KEY, applied_at TIMESTAMPTZ NOT NULL DEFAULT now())`); err != nil {
		return nil, fmt.Errorf("create schema_migrations: %w", err)
	}

	var applied []string
	for _, f := range files {
		version := strings.TrimSuffix(strings.TrimPrefix(f, "migrations/"), ".sql")
		ok, err := applyOne(ctx, pool, version, f)
		if err != nil {
			return applied, fmt.Errorf("migration %s: %w", version, err)
		}
		if ok {
			applied = append(applied, version)
		}
	}
	return applied, nil
}

func applyOne(ctx context.Context, pool *pgxpool.Pool, version, file string) (bool, error) {
	tx, err := pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return false, err
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtext('crashcart_migrations'))`); err != nil {
		return false, err
	}
	var exists bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM schema_migrations WHERE version = $1)`, version).Scan(&exists); err != nil {
		return false, err
	}
	if exists {
		return false, nil
	}
	sql, err := migrationFS.ReadFile(file)
	if err != nil {
		return false, err
	}
	if _, err := tx.Exec(ctx, string(sql)); err != nil {
		return false, err
	}
	if _, err := tx.Exec(ctx, `INSERT INTO schema_migrations (version) VALUES ($1)`, version); err != nil {
		return false, err
	}
	return true, tx.Commit(ctx)
}
