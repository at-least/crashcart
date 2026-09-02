package db_test

import (
	"context"
	"fmt"
	"math/rand/v2"
	"os"
	"strings"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/crashcartapp/crashcart/internal/db"
	"github.com/crashcartapp/crashcart/internal/testdb"
)

// TestProjectTablesCascade: every table with a project_id column has a
// foreign key to projects with ON DELETE CASCADE — a table added without
// one fails here, and deleting a project really removes its rows.
func TestProjectTablesCascade(t *testing.T) {
	st := testdb.New(t)
	ctx := context.Background()
	// pg_catalog, not information_schema: the latter is minutes on a
	// server with many test schemas.
	rows, err := st.Pool.Query(ctx, `
		SELECT c.relname,
		       EXISTS (SELECT 1 FROM pg_constraint k
		               WHERE k.conrelid = c.oid AND k.contype = 'f' AND k.confdeltype = 'c'
		                 AND k.confrelid = (SELECT oid FROM pg_class WHERE relname = 'projects' AND relnamespace = c.relnamespace)
		                 AND k.conkey = ARRAY[a.attnum])
		FROM pg_class c
		JOIN pg_namespace n ON n.oid = c.relnamespace
		JOIN pg_attribute a ON a.attrelid = c.oid AND a.attname = 'project_id' AND NOT a.attisdropped
		WHERE n.nspname = current_schema() AND c.relkind IN ('r', 'p') AND c.relname <> 'projects'
		ORDER BY 1`)
	if err != nil {
		t.Fatal(err)
	}
	var tables []string
	for rows.Next() {
		var name string
		var ok bool
		if err := rows.Scan(&name, &ok); err != nil {
			t.Fatal(err)
		}
		tables = append(tables, name)
		if !ok {
			t.Errorf("%s has a project_id without a FOREIGN KEY … REFERENCES projects ON DELETE CASCADE", name)
		}
	}
	rows.Close()
	// The enumeration itself must see the tables ARCHITECTURE.md lists
	// (partitions included: they inherit the constraint).
	for _, want := range []string{"events", "events_default", "attachments", "sessions", "releases", "issues", "symbol_files", "project_usage", "jobs", "alert_rules", "alert_channels", "event_stats_dirty", "session_stats_dirty", "event_stats_hourly_rolled", "issue_stats_hourly_rolled", "release_health_hourly_rolled"} {
		found := false
		for _, have := range tables {
			found = found || have == want
		}
		if !found {
			t.Errorf("%s not found among the project tables: %v", want, tables)
		}
	}
	// And the cascade acts: rows of a deleted project vanish, another
	// project's stay.
	testdb.Projects(t, st, 1, 2)
	for _, pid := range []int{1, 2} {
		if _, err := st.Pool.Exec(ctx, fmt.Sprintf(`INSERT INTO events (occurred_at, project_id, event_id, level, message) VALUES (now(), %d, gen_random_uuid(), 'error', 'm')`, pid)); err != nil {
			t.Fatal(err)
		}
		if _, err := st.Pool.Exec(ctx, fmt.Sprintf(`INSERT INTO issues (project_id, fingerprint, title, level, first_seen, last_seen) VALUES (%d, gen_random_uuid(), 't', 'error', now(), now())`, pid)); err != nil {
			t.Fatal(err)
		}
		if _, err := st.Pool.Exec(ctx, fmt.Sprintf(`INSERT INTO jobs (kind, project_id) VALUES ('alert', %d)`, pid)); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := st.Pool.Exec(ctx, "DELETE FROM projects WHERE id = 1"); err != nil {
		t.Fatal(err)
	}
	for _, tbl := range []string{"events", "issues", "jobs", "event_stats_dirty"} {
		var n1, n2 int64
		if err := st.Pool.QueryRow(ctx, "SELECT count(*) FILTER (WHERE project_id = 1), count(*) FILTER (WHERE project_id = 2) FROM "+tbl).Scan(&n1, &n2); err != nil {
			t.Fatal(err)
		}
		if n1 != 0 {
			t.Errorf("%s: %d rows of the deleted project remain", tbl, n1)
		}
		if tbl != "event_stats_dirty" && n2 != 1 {
			t.Errorf("%s: the other project's row went too (%d)", tbl, n2)
		}
	}
}

// TestEnumRejectsBadValue: enum columns are Postgres types, so a value
// outside the set is refused by the database itself (issue_status has no
// "triaged"; event_level no "critical"; job_kind no "cleanup").
func TestEnumRejectsBadValue(t *testing.T) {
	st := testdb.New(t)
	testdb.Projects(t, st, 1)
	ctx := context.Background()
	for _, c := range []struct{ name, sql string }{
		{"issues.status", `INSERT INTO issues (project_id, fingerprint, title, level, status, first_seen, last_seen) VALUES (1, gen_random_uuid(), 't', 'error', 'triaged', now(), now())`},
		{"events.level", `INSERT INTO events (occurred_at, project_id, event_id, level, message) VALUES (now(), 1, gen_random_uuid(), 'critical', 'm')`},
		{"jobs.kind", `INSERT INTO jobs (kind, project_id) VALUES ('cleanup', 1)`},
		{"sessions.status", `INSERT INTO sessions (started_at, project_id, sid, release, status) VALUES (now(), 1, 's', '1', 'unknown')`},
		{"alert_rules.type", `INSERT INTO alert_rules (project_id, type) VALUES (1, 'crash_spike')`},
	} {
		_, err := st.Pool.Exec(ctx, c.sql)
		if err == nil {
			t.Errorf("%s: a value outside the enum was accepted", c.name)
		} else if !(strings.Contains(err.Error(), "invalid input value for enum")) {
			t.Errorf("%s: refused for another reason: %v", c.name, err)
		}
	}
	// And the allowed values are exactly the documented set.
	rows, err := st.Pool.Query(ctx, `SELECT t.typname, array_agg(e.enumlabel ORDER BY e.enumsortorder)::text FROM pg_type t JOIN pg_enum e ON e.enumtypid = t.oid
		JOIN pg_namespace n ON n.oid = t.typnamespace WHERE n.nspname = current_schema() GROUP BY 1 ORDER BY 1`)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]string{}
	for rows.Next() {
		var name, labels string
		rows.Scan(&name, &labels)
		got[name] = labels
	}
	rows.Close()
	for name, want := range map[string]string{
		"event_level":    "{fatal,error,warning,info,debug}",
		"session_status": "{ok,exited,crashed,errored,abnormal}",
		"issue_status":   "{unresolved,resolved,ignored,regression}",
		"symbol_kind":    "{proguard,sourcemap,dsym}",
		"job_kind":       "{symbolicate,resymbolicate,alert}",
		"alert_type":     "{new_issue,regression,unhandled_spike,escalating}",
		"channel_kind":   "{webhook,telegram}",
	} {
		if got[name] != want {
			t.Errorf("enum %s = %s, want %s", name, got[name], want)
		}
	}
}

// TestInitConcurrently: replicas starting together against an empty
// database create the schema exactly once (the advisory lock), the rest
// find it and pass the version check.
func TestInitConcurrently(t *testing.T) {
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
	t.Cleanup(func() {
		admin.Exec(ctx, "DROP SCHEMA "+schema+" CASCADE")
		admin.Close()
	})
	cfg, err := pgxpool.ParseConfig(url)
	if err != nil {
		t.Fatal(err)
	}
	cfg.ConnConfig.RuntimeParams["search_path"] = schema + ",public"
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	var wg sync.WaitGroup
	var mu sync.Mutex
	created := 0
	for range 6 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			c, err := db.Init(ctx, pool)
			if err != nil {
				t.Errorf("Init: %v", err)
				return
			}
			mu.Lock()
			if c {
				created++
			}
			mu.Unlock()
		}()
	}
	wg.Wait()
	if created != 1 {
		t.Fatalf("schema created %d times, want 1", created)
	}
	var n int
	if err := pool.QueryRow(ctx, "SELECT count(*) FROM crashcart_schema").Scan(&n); err != nil || n != 1 {
		t.Fatalf("version rows = %d %v", n, err)
	}
	var v int
	if err := pool.QueryRow(ctx, "SELECT version FROM crashcart_schema").Scan(&v); err != nil || v != db.SchemaVersion {
		t.Fatalf("version = %d %v", v, err)
	}
}
