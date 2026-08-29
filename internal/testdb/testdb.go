// Package testdb gives DB-backed tests a fresh schema on TEST_DATABASE_URL.
package testdb

import (
	"context"
	"fmt"
	"math/rand/v2"
	"os"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/crashcartapp/crashcart/internal/db"
	"github.com/crashcartapp/crashcart/internal/store"
)

// New returns a Store on a throwaway schema (dropped at cleanup). The test is
// skipped when TEST_DATABASE_URL is unset.
func New(t testing.TB) *store.Store {
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
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	// TEST_PLAIN=1 runs the suite on the plain-Postgres schema variant.
	mode := db.Timescale
	if os.Getenv("TEST_PLAIN") != "" {
		mode = db.Plain
	}
	_, plain, err := db.MigrateMode(ctx, pool, mode)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		pool.Close()
		// Continuous aggregates must go before their hypertables.
		admin.Exec(ctx, "DROP SCHEMA "+schema+" CASCADE")
		admin.Close()
	})
	st := store.New(pool)
	st.Plain = plain
	return st
}
