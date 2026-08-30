package db

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"os"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

// pool opens a throwaway schema on TEST_DATABASE_URL (internal/testdb
// cannot be used here: it imports this package).
func pool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	url := os.Getenv("TEST_DATABASE_URL")
	if url == "" {
		t.Skip("TEST_DATABASE_URL not set")
	}
	ctx := context.Background()
	schema := fmt.Sprintf("t_%d_%d", os.Getpid(), rand.Int64N(1<<31))
	admin, err := pgxpool.New(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := admin.Exec(ctx, "CREATE SCHEMA "+schema); err != nil {
		t.Fatal(err)
	}
	cfg, err := pgxpool.ParseConfig(url)
	if err != nil {
		t.Fatal(err)
	}
	cfg.ConnConfig.RuntimeParams["search_path"] = schema + ",public"
	p, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		p.Close()
		admin.Exec(ctx, "DROP SCHEMA "+schema+" CASCADE")
		admin.Close()
	})
	return p
}

func TestInitVersion(t *testing.T) {
	p := pool(t)
	ctx := context.Background()
	if created, err := Init(ctx, p); err != nil || !created {
		t.Fatalf("first Init: created=%v err=%v", created, err)
	}
	if created, err := Init(ctx, p); err != nil || created {
		t.Fatalf("second Init: created=%v err=%v", created, err)
	}
	var v int
	if err := p.QueryRow(ctx, "SELECT version FROM crashcart_schema").Scan(&v); err != nil || v != SchemaVersion {
		t.Fatalf("version row: %d %v (want %d)", v, err, SchemaVersion)
	}

	// Another version: refused, and the message says how to move the data.
	if _, err := p.Exec(ctx, "UPDATE crashcart_schema SET version = version + 1"); err != nil {
		t.Fatal(err)
	}
	_, err := Init(ctx, p)
	if !errors.Is(err, ErrSchemaVersion) {
		t.Fatalf("Init on another version: %v", err)
	}
	if want := fmt.Sprintf("has schema version %d, this crashcart needs %d", SchemaVersion+1, SchemaVersion); err == nil || !contains(err.Error(), want) || !contains(err.Error(), "crashcart export") {
		t.Fatalf("message: %v", err)
	}

	// A database from before the version table: version 0, refused too.
	if _, err := p.Exec(ctx, "DROP TABLE crashcart_schema"); err != nil {
		t.Fatal(err)
	}
	if _, err := Init(ctx, p); !errors.Is(err, ErrSchemaVersion) || !contains(err.Error(), "has schema version 0") {
		t.Fatalf("Init without version table: %v", err)
	}
}

func contains(s, sub string) bool { return strings.Contains(s, sub) }
