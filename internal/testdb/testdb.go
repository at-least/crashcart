// Package testdb gives DB-backed tests a real, separate database on
// TEST_DATABASE_URL: a pgtestdb template (migrated once per schema version)
// cloned fresh for each test.
package testdb

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"fmt"
	"net/url"
	"os"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jackc/pgx/v5/stdlib"
	"github.com/peterldowns/pgtestdb"

	"github.com/at-least/crashcart/internal/db"
	"github.com/at-least/crashcart/internal/store"
)

// New returns a Store on a database cloned from a template migrated by
// schemaMigrator. Every test gets its own real database, not a shared one
// with a per-test schema. The test is skipped when TEST_DATABASE_URL is
// unset.
func New(t testing.TB) *store.Store {
	t.Helper()
	url := os.Getenv("TEST_DATABASE_URL")
	if url == "" {
		t.Skip("TEST_DATABASE_URL not set")
	}
	admin, err := adminConfig(url)
	if err != nil {
		t.Fatal(err)
	}
	inst := pgtestdb.Custom(t, admin, schemaMigrator{})
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, inst.URL())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	return store.New(pool)
}

// adminConfig turns TEST_DATABASE_URL into the connection details pgtestdb
// uses to reach the server and manage template/instance databases.
func adminConfig(dsn string) (pgtestdb.Config, error) {
	u, err := url.Parse(dsn)
	if err != nil {
		return pgtestdb.Config{}, err
	}
	pass, _ := u.User.Password()
	port := u.Port()
	if port == "" {
		port = "5432"
	}
	return pgtestdb.Config{
		DriverName: "pgx",
		Host:       u.Hostname(),
		Port:       port,
		User:       u.User.Username(),
		Password:   pass,
		Database:   strings.TrimPrefix(u.Path, "/"),
		Options:    u.RawQuery,
	}, nil
}

// schemaMigrator provisions a pgtestdb template by running schema.sql
// exactly as db.Init does on a fresh database, minus Init's advisory lock
// (unneeded here: pgtestdb already serializes template creation, and this
// runs against a database nothing else can see yet).
type schemaMigrator struct{}

// Hash identifies the template by the schema and its version, so a schema
// change (a new schema.sql, a version bump) gets a new template instead of
// reusing a stale one.
func (schemaMigrator) Hash() (string, error) {
	sum := sha256.Sum256([]byte(fmt.Sprintf("%s\x00%d", db.Schema(), db.SchemaVersion)))
	return hex.EncodeToString(sum[:]), nil
}

func (schemaMigrator) Migrate(ctx context.Context, sqlDB *sql.DB, _ pgtestdb.Config) error {
	conn, err := sqlDB.Conn(ctx)
	if err != nil {
		return err
	}
	defer conn.Close()
	return conn.Raw(func(driverConn any) error {
		pgxConn := driverConn.(*stdlib.Conn).Conn()
		if _, err := pgxConn.Exec(ctx, db.Schema()); err != nil {
			return fmt.Errorf("create schema: %w", err)
		}
		_, err := pgxConn.Exec(ctx, "INSERT INTO crashcart_schema (version) VALUES ($1)", db.SchemaVersion)
		return err
	})
}

// Projects creates placeholder projects with the given ids so rows that
// reference them (events, issues, jobs, …) satisfy the foreign keys.
func Projects(t testing.TB, st *store.Store, ids ...int64) {
	t.Helper()
	for _, id := range ids {
		_, err := st.Pool.Exec(context.Background(),
			`INSERT INTO projects (id, slug, name, public_key) OVERRIDING SYSTEM VALUE VALUES ($1, $2, $2, $2) ON CONFLICT DO NOTHING`,
			id, fmt.Sprintf("p%d", id))
		if err != nil {
			t.Fatal(err)
		}
	}
}
