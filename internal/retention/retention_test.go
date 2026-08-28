package retention

import (
	"context"
	"encoding/json"
	"log/slog"
	"testing"
	"time"

	"github.com/newlix/crashcart/internal/config"
	"github.com/newlix/crashcart/internal/db/sqlc"
	"github.com/newlix/crashcart/internal/pk"
	"github.com/newlix/crashcart/internal/testdb"
)

func TestReconcile(t *testing.T) {
	st := testdb.New(t)
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
