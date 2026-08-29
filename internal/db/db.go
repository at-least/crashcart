// Package db owns the schema: one embedded schema.sql, created on the first
// start against an empty database. There is no migration history — the
// schema is the file.
package db

import (
	"context"
	_ "embed"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5/pgxpool"
)

//go:embed schema.sql
var schema string

// initLock is the advisory lock key so replicas can start together.
const initLock = 0x6372617368 // "crash"

// ErrNoTimescale is returned when the database cannot run the Community
// (TSL-licensed) build of TimescaleDB, which the schema needs for
// compression and continuous aggregates. Most managed Postgres hosts ship
// the Apache-2 build (CREATE EXTENSION succeeds, those features fail) or
// none at all.
var ErrNoTimescale = errors.New("CrashCart needs TimescaleDB (Community build): use the timescale/timescaledb image, Tiger Cloud, or install the timescaledb package")

// Init creates the schema when the database is empty (no projects table)
// and reports whether it did. Safe to call from every replica at startup.
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

	if err := requireTimescale(ctx, conn); err != nil {
		return false, err
	}
	var exists bool
	if err := conn.QueryRow(ctx, "SELECT to_regclass('projects') IS NOT NULL").Scan(&exists); err != nil {
		return false, err
	}
	if exists {
		return false, nil
	}
	tx, err := conn.Begin(ctx)
	if err != nil {
		return false, err
	}
	defer tx.Rollback(context.WithoutCancel(ctx))
	if _, err := tx.Exec(ctx, schema); err != nil {
		return false, fmt.Errorf("create schema: %w", err)
	}
	return true, tx.Commit(ctx)
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
