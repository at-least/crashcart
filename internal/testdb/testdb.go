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
	"io/fs"
	"net/url"
	"os"
	"sort"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/peterldowns/pgtestdb"
	"github.com/pressly/goose/v3"

	"github.com/at-least/crashcart/internal/db"
	"github.com/at-least/crashcart/internal/store"
)

// New returns a Store on a database cloned from a template migrated by
// schemaMigrator. Every test gets its own real database, not a shared one
// with a per-test schema. The test is skipped when TEST_DATABASE_URL is
// unset.
func New(t testing.TB) *store.Store {
	t.Helper()
	return NewWithMaxConns(t, 0)
}

// NewWithMaxConns is New with Pool's MaxConns pinned to n (pgxpool's own
// default when n is 0) — for tests that need to constrain the *query* pool
// specifically, such as proving RunAsLeader can't starve it regardless of
// how small it is (github.com/at-least/crashcart/issues/1).
func NewWithMaxConns(t testing.TB, n int32) *store.Store {
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
	cfg, err := pgxpool.ParseConfig(inst.URL())
	if err != nil {
		t.Fatal(err)
	}
	if n > 0 {
		cfg.MaxConns = n
	}
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	st, err := store.New(ctx, pool)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(st.Close)
	return st
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

// schemaMigrator provisions a pgtestdb template by running the same goose
// migrations db.Init applies in production, against a database nothing
// else can see yet (pgtestdb already serializes template creation, so
// there's no need for db.Init's own advisory lock or legacy-bootstrap
// logic here — every template starts empty).
type schemaMigrator struct{}

// Hash identifies the template by every migration file's content, so
// adding or editing a migration gets a new template instead of reusing a
// stale one.
func (schemaMigrator) Hash() (string, error) {
	var names []string
	err := fs.WalkDir(db.Migrations(), ".", func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		names = append(names, path)
		return nil
	})
	if err != nil {
		return "", err
	}
	sort.Strings(names)
	h := sha256.New()
	for _, name := range names {
		b, err := fs.ReadFile(db.Migrations(), name)
		if err != nil {
			return "", err
		}
		h.Write(b)
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func (schemaMigrator) Migrate(ctx context.Context, sqlDB *sql.DB, _ pgtestdb.Config) error {
	provider, err := goose.NewProvider(goose.DialectPostgres, sqlDB, db.Migrations())
	if err != nil {
		return err
	}
	_, err = provider.Up(ctx)
	return err
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
