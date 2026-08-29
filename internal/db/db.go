// Package db owns the schema: one embedded schema.sql, created on the first
// start against an empty database. There is no migration history — the
// schema is the file.
package db

import (
	"context"
	_ "embed"
	"fmt"

	"github.com/jackc/pgx/v5/pgxpool"
)

//go:embed schema.sql
var schema string

// initLock is the advisory lock key so replicas can start together.
const initLock = 0x6372617368 // "crash"

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
