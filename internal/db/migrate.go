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

// Mode selects the schema variant: TimescaleDB (hypertables, continuous
// aggregates) or plain Postgres (rolled-up stats tables behind views).
type Mode int

const (
	// Auto uses TimescaleDB when the extension can be created, else plain.
	Auto Mode = iota
	// Timescale requires the extension (migration fails without it).
	Timescale
	// Plain never touches TimescaleDB, even when it is installed.
	Plain
)

// ParseMode reads the TIMESCALE setting: "auto" (default), "on", "off".
func ParseMode(s string) (Mode, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "", "auto":
		return Auto, nil
	case "on", "true", "1", "timescale":
		return Timescale, nil
	case "off", "false", "0", "plain":
		return Plain, nil
	}
	return Auto, fmt.Errorf("TIMESCALE must be auto, on or off (got %q)", s)
}

// Variant files carry the mode in their name: NNNN_timescale.sql runs only
// on TimescaleDB, NNNN_plain.sql only on plain Postgres; every other file
// runs everywhere. A database keeps the variant it was created with.
const (
	timescaleSuffix = "_timescale.sql"
	plainSuffix     = "_plain.sql"
)

// Migrate applies every pending migration in filename order (Auto mode).
func Migrate(ctx context.Context, pool *pgxpool.Pool) ([]string, error) {
	ran, _, err := MigrateMode(ctx, pool, Auto)
	return ran, err
}

// IsPlain reports whether the database was migrated as plain Postgres.
func IsPlain(ctx context.Context, pool *pgxpool.Pool) (bool, error) {
	var n int
	err := pool.QueryRow(ctx, "SELECT count(*) FROM schema_migrations WHERE name LIKE '%'||$1", plainSuffix).Scan(&n)
	return n > 0, err
}

// MigrateMode applies every pending migration in filename order and reports
// whether the database is plain Postgres. Each file runs in its own
// transaction (TimescaleDB DDL is transactional).
func MigrateMode(ctx context.Context, pool *pgxpool.Pool, mode Mode) (ran []string, plain bool, err error) {
	conn, err := pool.Acquire(ctx)
	if err != nil {
		return nil, false, err
	}
	defer conn.Release()

	if _, err := conn.Exec(ctx, "SELECT pg_advisory_lock($1)", migrationLock); err != nil {
		return nil, false, err
	}
	defer conn.Exec(context.WithoutCancel(ctx), "SELECT pg_advisory_unlock($1)", migrationLock)

	if _, err := conn.Exec(ctx, `CREATE TABLE IF NOT EXISTS schema_migrations (
		name TEXT PRIMARY KEY, applied_at TIMESTAMPTZ NOT NULL DEFAULT now())`); err != nil {
		return nil, false, err
	}
	applied := map[string]bool{}
	rows, err := conn.Query(ctx, "SELECT name FROM schema_migrations")
	if err != nil {
		return nil, false, err
	}
	names, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return nil, false, err
	}
	for _, n := range names {
		applied[n] = true
	}

	// The variant is fixed by the first variant migration applied; a fresh
	// database decides by mode (Auto probes the extension).
	variant := ""
	for n := range applied {
		if strings.HasSuffix(n, plainSuffix) {
			variant = plainSuffix
		} else if strings.HasSuffix(n, timescaleSuffix) && variant == "" {
			variant = timescaleSuffix
		}
	}
	if variant == "" {
		switch mode {
		case Plain:
			variant = plainSuffix
		case Timescale:
			variant = timescaleSuffix
		default:
			if timescaleUsable(ctx, conn) {
				variant = timescaleSuffix
			} else {
				variant = plainSuffix
			}
		}
	} else if (mode == Plain && variant == timescaleSuffix) || (mode == Timescale && variant == plainSuffix) {
		return nil, false, fmt.Errorf("database was migrated as %s but TIMESCALE asks for %s", variantName(variant), variantName(otherVariant(variant)))
	}
	plain = variant == plainSuffix

	files, err := fs.ReadDir(migrations, "migrations")
	if err != nil {
		return nil, plain, err
	}
	sort.Slice(files, func(i, j int) bool { return files[i].Name() < files[j].Name() })
	for _, f := range files {
		name := f.Name()
		if !strings.HasSuffix(name, ".sql") || applied[name] {
			continue
		}
		if strings.HasSuffix(name, otherVariant(variant)) {
			continue
		}
		body, err := migrations.ReadFile("migrations/" + name)
		if err != nil {
			return ran, plain, err
		}
		tx, err := conn.Begin(ctx)
		if err != nil {
			return ran, plain, err
		}
		if _, err := tx.Exec(ctx, string(body)); err != nil {
			tx.Rollback(ctx)
			return ran, plain, fmt.Errorf("migration %s: %w", name, err)
		}
		if _, err := tx.Exec(ctx, "INSERT INTO schema_migrations (name) VALUES ($1)", name); err != nil {
			tx.Rollback(ctx)
			return ran, plain, err
		}
		if err := tx.Commit(ctx); err != nil {
			return ran, plain, err
		}
		ran = append(ran, name)
	}
	return ran, plain, nil
}

func otherVariant(v string) string {
	if v == plainSuffix {
		return timescaleSuffix
	}
	return plainSuffix
}

func variantName(v string) string {
	if v == plainSuffix {
		return "plain Postgres"
	}
	return "TimescaleDB"
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

// timescaleUsable reports whether the full TimescaleDB is available: the
// extension can be created and it runs under the "timescale" (TSL) license.
// Hosts such as Neon ship the Apache-2 build, where CREATE EXTENSION succeeds
// but compression and continuous aggregates — everything 0002_timescale.sql
// needs — fail with "functionality not supported under the current license".
func timescaleUsable(ctx context.Context, conn *pgxpool.Conn) bool {
	if _, err := conn.Exec(ctx, "CREATE EXTENSION IF NOT EXISTS timescaledb"); err != nil {
		return false
	}
	var license string
	if err := conn.QueryRow(ctx, "SELECT current_setting('timescaledb.license', true)").Scan(&license); err != nil {
		return false
	}
	return license == "timescale"
}
