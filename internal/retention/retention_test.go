package retention

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"testing"
	"time"

	"github.com/crashcartapp/crashcart/internal/config"
	"github.com/crashcartapp/crashcart/internal/db/sqlc"
	"github.com/crashcartapp/crashcart/internal/pk"
	"github.com/crashcartapp/crashcart/internal/testdb"
)

func TestReconcile(t *testing.T) {
	st := testdb.New(t)
	if st.Plain {
		t.Skip("plain Postgres has no policies")
	}
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
	// The configured windows landed in id units.
	var raw []byte
	if err := st.Pool.QueryRow(ctx, `SELECT config FROM timescaledb_information.jobs WHERE proc_name = 'policy_retention' AND hypertable_schema = current_schema() AND hypertable_name = 'events'`).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	var c struct {
		DropAfter int64 `json:"drop_after"`
	}
	json.Unmarshal(raw, &c)
	if c.DropAfter != 14*pk.Day {
		t.Errorf("drop_after = %d, want %d", c.DropAfter, 14*pk.Day)
	}
	if err := st.Pool.QueryRow(ctx, `SELECT config FROM timescaledb_information.jobs WHERE proc_name = 'policy_compression' AND hypertable_schema = current_schema() AND hypertable_name = 'events'`).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	var cc struct {
		CompressAfter int64 `json:"compress_after"`
	}
	json.Unmarshal(raw, &cc)
	if cc.CompressAfter != 36*pk.Hour {
		t.Errorf("compress_after = %d, want %d", cc.CompressAfter, 36*pk.Hour)
	}
	// Changing the configuration replaces the policies.
	cfg.RetentionDays = 7
	if err := Reconcile(ctx, st, cfg, log); err != nil {
		t.Fatal(err)
	}
	if n := count(hyper, "policy_retention"); n != 2 {
		t.Errorf("after change: retention policies = %d", n)
	}
	st.Pool.QueryRow(ctx, `SELECT config FROM timescaledb_information.jobs WHERE proc_name = 'policy_retention' AND hypertable_schema = current_schema() AND hypertable_name = 'events'`).Scan(&raw)
	json.Unmarshal(raw, &c)
	if c.DropAfter != 7*pk.Day {
		t.Errorf("drop_after after change = %d", c.DropAfter)
	}
}

func TestSweep(t *testing.T) {
	st := testdb.New(t)
	ctx := context.Background()
	cfg := config.Config{RetentionDays: 30}
	now := time.Now()
	old := pk.Lower(now.Add(-40 * 24 * time.Hour))
	fresh := pk.Lower(now)

	mk := func(fp string, seen int64, status string) {
		if _, err := st.UpsertIssue(ctx, sqlc.UpsertIssueParams{ProjectID: 1, Fingerprint: fp, Title: fp, Level: "error", EventCount: 1, StoredCount: 1, FirstSeen: seen, LastSeen: seen}); err != nil {
			t.Fatal(err)
		}
		if _, err := st.SetIssueStatus(ctx, sqlc.SetIssueStatusParams{ProjectID: 1, Fingerprint: fp, Status: status}); err != nil {
			t.Fatal(err)
		}
	}
	mk("old-resolved", old, "resolved")
	mk("old-ignored", old, "ignored")
	mk("old-open", old, "unresolved")
	mk("new-resolved", fresh, "resolved")

	for _, j := range []struct {
		attempts int
		age      string
	}{{8, "1 hour"}, {0, "8 days"}, {0, "1 hour"}} {
		if _, err := st.Pool.Exec(ctx, `INSERT INTO jobs (kind, project_id, attempts, created_at) VALUES ('x', 1, $1, now() - $2::interval)`, j.attempts, j.age); err != nil {
			t.Fatal(err)
		}
	}
	for _, w := range []int64{now.Unix() - 600, now.Unix()} {
		if _, err := st.BumpRateLimit(ctx, sqlc.BumpRateLimitParams{RlKey: "k", WindowStart: w - w%60}); err != nil {
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
	var issues, jobs, limits, symbols int64
	st.Pool.QueryRow(ctx, "SELECT count(*) FROM issues").Scan(&issues)
	st.Pool.QueryRow(ctx, "SELECT count(*) FROM jobs").Scan(&jobs)
	st.Pool.QueryRow(ctx, "SELECT count(*) FROM rate_limits").Scan(&limits)
	st.Pool.QueryRow(ctx, "SELECT count(*) FROM symbol_files").Scan(&symbols)
	if issues != 2 || jobs != 1 || limits != 1 || symbols != 1 {
		t.Errorf("after sweep: issues=%d jobs=%d rate_limits=%d symbol_files=%d", issues, jobs, limits, symbols)
	}
	left, _ := st.ListSymbolFiles(ctx, 1)
	if len(left) != 1 || left[0].Release != "1 day" {
		t.Errorf("symbol files left = %+v", left)
	}
}

func TestRefreshAggregates(t *testing.T) {
	st := testdb.New(t)
	ctx := context.Background()
	// An event written directly (as import does) two days ago: outside the
	// policy window, invisible until refreshed.
	old := pk.Lower(time.Now().Add(-48 * time.Hour))
	if _, err := st.Pool.Exec(ctx, `INSERT INTO events (id, project_id, event_id, level, message, payload) VALUES ($1, 1, 'e1', 'fatal', 'm', '{}')`, old); err != nil {
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

func TestRollupPlain(t *testing.T) {
	st := testdb.New(t)
	if !st.Plain {
		t.Skip("TEST_PLAIN=1 only")
	}
	ctx := context.Background()
	now := time.Now().UTC()
	// Two crashes in a complete hour, one in the live hour, one session yesterday.
	insert := func(id int64, level string) {
		if _, err := st.Pool.Exec(ctx, `INSERT INTO events (id, project_id, event_id, level, message, handled, release, payload) VALUES ($1, 1, $2, $3, 'm', false, '1.0', '{}')`, id, fmt.Sprint(id), level); err != nil {
			t.Fatal(err)
		}
	}
	insert(pk.Lower(now.Add(-2*time.Hour)), "fatal")
	insert(pk.Lower(now.Add(-2*time.Hour))+1, "error")
	insert(pk.Lower(now), "fatal")
	if _, err := st.Pool.Exec(ctx, `INSERT INTO sessions (id, project_id, release, status, count) VALUES ($1, 1, '1.0', 'crashed', 3)`, pk.Lower(now.Add(-24*time.Hour))); err != nil {
		t.Fatal(err)
	}
	var live int64
	st.Pool.QueryRow(ctx, "SELECT coalesce(sum(crashes), 0) FROM event_stats_hourly WHERE project_id = 1").Scan(&live)
	if live != 1 {
		t.Fatalf("before rollup only the live hour should show: crashes = %d", live)
	}
	if err := RollupRecent(ctx, st, now); err != nil {
		t.Fatal(err)
	}
	var crashes, rolled int64
	st.Pool.QueryRow(ctx, "SELECT coalesce(sum(crashes), 0) FROM event_stats_hourly WHERE project_id = 1").Scan(&crashes)
	st.Pool.QueryRow(ctx, "SELECT count(*) FROM event_stats_hourly_rolled WHERE project_id = 1").Scan(&rolled)
	if crashes != 3 || rolled != 2 {
		t.Fatalf("after rollup: crashes = %d (want 3), rolled rows = %d (want 2)", crashes, rolled)
	}
	// Idempotent, and RollupAll reaches yesterday's session.
	if err := RollupRecent(ctx, st, now); err != nil {
		t.Fatal(err)
	}
	st.Pool.QueryRow(ctx, "SELECT coalesce(sum(crashes), 0) FROM event_stats_hourly WHERE project_id = 1").Scan(&crashes)
	if crashes != 3 {
		t.Fatalf("second rollup changed the sums: %d", crashes)
	}
	if err := RollupAll(ctx, st, now); err != nil {
		t.Fatal(err)
	}
	var total int64
	st.Pool.QueryRow(ctx, "SELECT coalesce(sum(total), 0) FROM release_health_daily WHERE project_id = 1").Scan(&total)
	if total != 3 {
		t.Fatalf("release health after RollupAll = %d, want 3", total)
	}
	// Sweep deletes by id range on plain.
	cfg := config.Config{RetentionDays: 1}
	if err := Sweep(ctx, st, cfg, slog.Default()); err != nil {
		t.Fatal(err)
	}
	var sessions int64
	st.Pool.QueryRow(ctx, "SELECT count(*) FROM sessions").Scan(&sessions)
	if sessions != 0 {
		t.Fatalf("sessions after sweep = %d, want 0", sessions)
	}
}
