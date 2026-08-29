package retention

import (
	"context"
	"log/slog"
	"testing"
	"time"

	"github.com/crashcartapp/crashcart/internal/config"
	"github.com/crashcartapp/crashcart/internal/db/sqlc"
	"github.com/crashcartapp/crashcart/internal/testdb"
)

func TestReconcile(t *testing.T) {
	st := testdb.New(t)
	testdb.Projects(t, st, 1)
	ctx := context.Background()
	log := slog.Default()
	cfg := config.Config{RetentionDays: 14, CompressAfter: 36 * time.Hour}
	if err := Reconcile(ctx, st, cfg, log); err != nil {
		t.Fatal(err)
	}
	// Idempotent: a second run leaves the same set of jobs.
	if err := Reconcile(ctx, st, cfg, log); err != nil {
		t.Fatal(err)
	}
	count := func(q string, args ...any) int64 {
		var n int64
		if err := st.Pool.QueryRow(ctx, q, args...).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	// Continuous-aggregate policies are reported under the view's own name
	// and schema, so both groups are filtered by name.
	hyper := `SELECT count(*) FROM timescaledb_information.jobs WHERE proc_name = $1 AND hypertable_schema = current_schema() AND hypertable_name IN ('events', 'sessions')`
	if n := count(hyper, "policy_retention"); n != 2 {
		t.Errorf("hypertable retention policies = %d", n)
	}
	if n := count(hyper, "policy_compression"); n != 2 {
		t.Errorf("compression policies = %d", n)
	}
	agg := `SELECT count(*) FROM timescaledb_information.jobs WHERE proc_name = 'policy_retention' AND hypertable_schema = current_schema()
	        AND hypertable_name IN ('event_stats_hourly', 'issue_stats_hourly', 'release_health_daily')`
	if n := count(agg); n != 3 {
		t.Errorf("aggregate retention policies = %d", n)
	}
	// The configured windows landed as intervals.
	window := func(proc, key string) string {
		var v string
		if err := st.Pool.QueryRow(ctx, `SELECT (config->>$2)::interval::text FROM timescaledb_information.jobs WHERE proc_name = $1 AND hypertable_schema = current_schema() AND hypertable_name = 'events'`, proc, key).Scan(&v); err != nil {
			t.Fatal(err)
		}
		return v
	}
	if got := window("policy_retention", "drop_after"); got != "14 days" {
		t.Errorf("drop_after = %q, want 14 days", got)
	}
	if got := window("policy_compression", "compress_after"); got != "36:00:00" {
		t.Errorf("compress_after = %q, want 36:00:00", got)
	}
	// Changing the configuration replaces the policies.
	cfg.RetentionDays = 7
	if err := Reconcile(ctx, st, cfg, log); err != nil {
		t.Fatal(err)
	}
	if n := count(hyper, "policy_retention"); n != 2 {
		t.Errorf("after change: retention policies = %d", n)
	}
	if got := window("policy_retention", "drop_after"); got != "7 days" {
		t.Errorf("drop_after after change = %q", got)
	}
}

func TestSweep(t *testing.T) {
	st := testdb.New(t)
	testdb.Projects(t, st, 1)
	ctx := context.Background()
	cfg := config.Config{RetentionDays: 30}
	now := time.Now()
	old := now.Add(-40 * 24 * time.Hour)
	fresh := now

	mk := func(fp string, seen time.Time, status string) {
		if _, err := st.UpsertIssue(ctx, sqlc.UpsertIssueParams{ProjectID: 1, Fingerprint: fp, Title: fp, Level: "error", EventCount: 1, StoredCount: 1, FirstSeen: seen, LastSeen: seen}); err != nil {
			t.Fatal(err)
		}
		if _, err := st.SetIssueStatus(ctx, sqlc.SetIssueStatusParams{ProjectID: 1, Fingerprint: fp, Status: sqlc.IssueStatus(status)}); err != nil {
			t.Fatal(err)
		}
	}
	mk("old-resolved", old, "resolved")
	mk("old-ignored", old, "ignored")
	mk("old-open", old, "unresolved")
	mk("new-resolved", fresh, "resolved")

	for i, j := range []struct {
		attempts int
		age      string
	}{{8, "1 hour"}, {0, "8 days"}, {0, "1 hour"}} {
		if _, err := st.Pool.Exec(ctx, `INSERT INTO jobs (kind, project_id, args, attempts, created_at) VALUES ('alert', 1, jsonb_build_object('n', $3::int), $1, now() - $2::interval)`, j.attempts, j.age, i); err != nil {
			t.Fatal(err)
		}
	}
	for _, age := range []string{"90 days", "1 day"} {
		if _, err := st.Pool.Exec(ctx, `INSERT INTO symbol_files (project_id, kind, release, filename, size, data, uploaded_at) VALUES (1, 'proguard', $1, 'mapping.txt', 1, '\x00'::bytea, now() - $2::interval)`, age, age); err != nil {
			t.Fatal(err)
		}
	}

	if err := Sweep(ctx, st, cfg, slog.Default()); err != nil {
		t.Fatal(err)
	}
	var issues, jobs, symbols int64
	st.Pool.QueryRow(ctx, "SELECT count(*) FROM issues").Scan(&issues)
	st.Pool.QueryRow(ctx, "SELECT count(*) FROM jobs").Scan(&jobs)
	st.Pool.QueryRow(ctx, "SELECT count(*) FROM symbol_files").Scan(&symbols)
	// Jobs: the week-old one is gone; the dead one (8 attempts) stays
	// visible with its error but is never claimed again.
	if issues != 2 || jobs != 2 || symbols != 1 {
		t.Errorf("after sweep: issues=%d jobs=%d symbol_files=%d", issues, jobs, symbols)
	}
	if dead, _ := st.DeadJobs(ctx, 1); len(dead) != 1 {
		t.Errorf("dead jobs = %d", len(dead))
	}
	if claimed, _ := st.ClaimJobs(ctx, sqlc.ClaimJobsParams{Max: 10, LockedUntil: time.Now().Add(time.Minute)}); len(claimed) != 1 {
		t.Errorf("claimable jobs = %d, want 1 (the dead one must not be claimed)", len(claimed))
	}
	left, _ := st.ListSymbolFiles(ctx, 1)
	if len(left) != 1 || left[0].Release == nil || *left[0].Release != "1 day" {
		t.Errorf("symbol files left = %+v", left)
	}
}

func TestRefreshAggregates(t *testing.T) {
	st := testdb.New(t)
	testdb.Projects(t, st, 1)
	ctx := context.Background()
	// An event written directly (as import does) two days ago: outside the
	// policy window, invisible until refreshed.
	old := time.Now().Add(-48 * time.Hour)
	if _, err := st.Pool.Exec(ctx, `INSERT INTO events (occurred_at, project_id, event_id, level, message, payload) VALUES ($1, 1, 'e1', 'fatal', 'm', '{}')`, old); err != nil {
		t.Fatal(err)
	}
	if err := RefreshAggregates(ctx, st); err != nil {
		t.Fatal(err)
	}
	var crashes int64
	if err := st.Pool.QueryRow(ctx, `SELECT coalesce(sum(crashes), 0) FROM event_stats_hourly WHERE project_id = 1`).Scan(&crashes); err != nil {
		t.Fatal(err)
	}
	if crashes != 1 {
		t.Fatalf("crashes after refresh = %d, want 1", crashes)
	}
}
