package db_test

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/at-least/crashcart/internal/db"
	"github.com/at-least/crashcart/internal/testdb"
)

func TestInitIdempotent(t *testing.T) {
	p := testdb.New(t).Pool // already migrated once, by pgtestdb's own tern run
	ctx := context.Background()
	if created, err := db.Init(ctx, p); err != nil || created {
		t.Fatalf("Init on an already-migrated database: created=%v err=%v", created, err)
	}
	var v int32
	if err := p.QueryRow(ctx, "SELECT version FROM "+db.VersionTable).Scan(&v); err != nil || v != latestMigration {
		t.Fatalf("migration version: %d %v (want %d)", v, err, latestMigration)
	}
}

func TestInitRefusesWhenDatabaseAheadOfBinary(t *testing.T) {
	p := testdb.New(t).Pool
	ctx := context.Background()
	if _, err := p.Exec(ctx, "UPDATE "+db.VersionTable+" SET version = 99"); err != nil {
		t.Fatal(err)
	}
	_, err := db.Init(ctx, p)
	if !errors.Is(err, db.ErrDatabaseAhead) {
		t.Fatalf("Init against a database ahead of this binary: %v", err)
	}
	if !strings.Contains(err.Error(), "99") {
		t.Fatalf("message should name the database's version: %v", err)
	}
}

func TestInitFreshDatabase(t *testing.T) {
	pool := emptyDatabase(t)
	ctx := context.Background()
	created, err := db.Init(ctx, pool)
	if err != nil || !created {
		t.Fatalf("Init on an empty database: created=%v err=%v", created, err)
	}
	var hasProjects bool
	if err := pool.QueryRow(ctx, "SELECT to_regclass('projects') IS NOT NULL").Scan(&hasProjects); err != nil || !hasProjects {
		t.Fatalf("projects table after Init: exists=%v err=%v", hasProjects, err)
	}
	var v int64
	if err := pool.QueryRow(ctx, "SELECT version FROM "+db.VersionTable).Scan(&v); err != nil || v != latestMigration {
		t.Fatalf("%s after Init: %d %v (want %d)", db.VersionTable, v, err, latestMigration)
	}
}

// latestMigration is the highest file in internal/db/migrations; bump it
// with every new migration so TestInitFreshDatabase keeps proving Init
// reaches the end.
const latestMigration = 3

// TestInitSingleConnection: Init must not need more than one pool
// connection at a time. Before the fix it pinned a connection for the
// advisory lock and ran migrations through a second acquisition on the
// same pool — with MaxConns=1 that is a self-deadlock (see
// https://github.com/at-least/crashcart/issues/1, the same shape as
// RunAsLeader's).
func TestInitSingleConnection(t *testing.T) {
	pool := emptyDatabase(t)
	cfg := pool.Config().Copy()
	cfg.MaxConns = 1
	ctx := context.Background()
	onePool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer onePool.Close()

	initCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	created, err := db.Init(initCtx, onePool)
	if err != nil || !created {
		t.Fatalf("Init with MaxConns=1: created=%v err=%v", created, err)
	}
}

// A new project accepts everything by default: sampling is what bounds
// the database.
func TestProjectDefaults(t *testing.T) {
	p := testdb.New(t).Pool
	ctx := context.Background()
	var rate float64
	if err := p.QueryRow(ctx, "INSERT INTO projects (slug, name, public_key) VALUES ('d', 'D', 'k') RETURNING sample_rate").Scan(&rate); err != nil {
		t.Fatal(err)
	}
	if rate != 1 {
		t.Fatalf("defaults: sample_rate=%v (want 1)", rate)
	}
}

func TestConnect(t *testing.T) {
	url := os.Getenv("TEST_DATABASE_URL")
	if url == "" {
		t.Skip("TEST_DATABASE_URL not set")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	pool, err := db.Connect(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	pool.Close()
	if _, err := db.Connect(ctx, "postgres://nobody:nothing@127.0.0.1:1/nowhere?sslmode=disable&connect_timeout=2"); err == nil {
		t.Error("an unreachable server must fail Connect, not the first query")
	}
	if _, err := db.Connect(ctx, "::not a url::"); err == nil {
		t.Error("an unparseable URL must fail")
	}
}

// emptyDatabase creates a fresh, empty database on the TEST_DATABASE_URL
// server (not one of testdb's pgtestdb-cloned databases, which are already
// migrated) and returns a pool connected to it, dropped at cleanup.
func emptyDatabase(t *testing.T) *pgxpool.Pool {
	t.Helper()
	adminURL := os.Getenv("TEST_DATABASE_URL")
	if adminURL == "" {
		t.Skip("TEST_DATABASE_URL not set")
	}
	ctx := context.Background()
	admin, err := pgxpool.New(ctx, adminURL)
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Close()
	name := fmt.Sprintf("legacy_%d_%d", os.Getpid(), time.Now().UnixNano())
	if _, err := admin.Exec(ctx, "CREATE DATABASE "+name); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		a, err := pgxpool.New(context.Background(), adminURL)
		if err != nil {
			return
		}
		defer a.Close()
		a.Exec(context.Background(), "DROP DATABASE IF EXISTS "+name+" WITH (FORCE)")
	})

	u, err := url.Parse(adminURL)
	if err != nil {
		t.Fatal(err)
	}
	u.Path = "/" + name
	pool, err := pgxpool.New(ctx, u.String())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	return pool
}
