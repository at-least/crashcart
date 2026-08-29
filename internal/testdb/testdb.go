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
	if _, err := db.Init(ctx, pool); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		pool.Close()
		// Continuous aggregates must go before their hypertables.
		admin.Exec(ctx, "DROP SCHEMA "+schema+" CASCADE")
		admin.Close()
	})
	return store.New(pool)
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
