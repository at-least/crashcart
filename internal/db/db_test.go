package db_test

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/crashcartapp/crashcart/internal/db"
	"github.com/crashcartapp/crashcart/internal/testdb"
)

func TestInitVersion(t *testing.T) {
	p := testdb.New(t).Pool // Init ran once, creating the schema
	ctx := context.Background()
	if created, err := db.Init(ctx, p); err != nil || created {
		t.Fatalf("second Init: created=%v err=%v", created, err)
	}
	var v int
	if err := p.QueryRow(ctx, "SELECT version FROM crashcart_schema").Scan(&v); err != nil || v != db.SchemaVersion {
		t.Fatalf("version row: %d %v (want %d)", v, err, db.SchemaVersion)
	}

	// Another version: refused, and the message says how to move the data.
	if _, err := p.Exec(ctx, "UPDATE crashcart_schema SET version = version + 1"); err != nil {
		t.Fatal(err)
	}
	_, err := db.Init(ctx, p)
	if !errors.Is(err, db.ErrSchemaVersion) {
		t.Fatalf("Init on another version: %v", err)
	}
	if want := fmt.Sprintf("has schema version %d, this crashcart needs %d", db.SchemaVersion+1, db.SchemaVersion); err == nil || !strings.Contains(err.Error(), want) || !strings.Contains(err.Error(), "crashcart export") {
		t.Fatalf("message: %v", err)
	}

	// A database from before the version table: version 0, refused too.
	if _, err := p.Exec(ctx, "DROP TABLE crashcart_schema"); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Init(ctx, p); !errors.Is(err, db.ErrSchemaVersion) || !strings.Contains(err.Error(), "has schema version 0") {
		t.Fatalf("Init without version table: %v", err)
	}
}

// A new project accepts everything by default: sampling bounds the
// database, a daily quota is an explicit cost cap.
func TestProjectDefaults(t *testing.T) {
	p := testdb.New(t).Pool
	ctx := context.Background()
	var quota int32
	var rate float64
	if err := p.QueryRow(ctx, "INSERT INTO projects (slug, name, public_key) VALUES ('d', 'D', 'k') RETURNING daily_quota, sample_rate").Scan(&quota, &rate); err != nil {
		t.Fatal(err)
	}
	if quota != 0 || rate != 1 {
		t.Fatalf("defaults: daily_quota=%d sample_rate=%v (want 0 = unlimited, 1)", quota, rate)
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
