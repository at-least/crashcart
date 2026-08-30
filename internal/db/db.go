// Package db owns the schema: one embedded schema.sql, created on the first
// start against an empty database. There is no migration history — the
// schema is the file, and it carries a version (crashcart_schema) that
// Init checks so a binary never runs against a database of another one.
package db

import (
	"context"
	_ "embed"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

//go:embed schema.sql
var schema string

// SchemaVersion is the version of schema.sql this binary carries. Bump it
// with every change to the schema; Init writes it into crashcart_schema on
// creation and refuses a database at any other version.
const SchemaVersion = 10

// ErrSchemaVersion: the database was created by a binary with another
// schema. It wraps the message an operator needs.
var ErrSchemaVersion = errors.New("schema version mismatch")

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
		return false, checkVersion(ctx, conn.Conn())
	}
	tx, err := conn.Begin(ctx)
	if err != nil {
		return false, err
	}
	defer tx.Rollback(context.WithoutCancel(ctx))
	if _, err := tx.Exec(ctx, schema); err != nil {
		return false, fmt.Errorf("create schema: %w", err)
	}
	if _, err := tx.Exec(ctx, "INSERT INTO crashcart_schema (version) VALUES ($1)", SchemaVersion); err != nil {
		return false, err
	}
	return true, tx.Commit(ctx)
}

// checkVersion compares the database's schema version with SchemaVersion.
// A database without the version table predates it and is treated as
// version 0.
func checkVersion(ctx context.Context, conn *pgx.Conn) error {
	var have int
	err := conn.QueryRow(ctx, "SELECT version FROM crashcart_schema LIMIT 1").Scan(&have)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) && !isUndefinedTable(err) {
		return err
	}
	if have == SchemaVersion {
		return nil
	}
	return fmt.Errorf("%w: database has schema version %d, this crashcart needs %d — there are no migrations: "+
		"run `crashcart export` with the old version, `crashcart import` into an empty database with this one, then point DATABASE_URL at it",
		ErrSchemaVersion, have, SchemaVersion)
}

func isUndefinedTable(err error) bool {
	var pgErr interface{ SQLState() string }
	return errors.As(err, &pgErr) && pgErr.SQLState() == "42P01"
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
