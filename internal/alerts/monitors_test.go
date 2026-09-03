package alerts

import (
	"context"
	"testing"
	"time"

	"github.com/at-least/crashcart/internal/store"
)

func upsertTestMonitor(t *testing.T, ctx context.Context, st *store.Store, projectID int64, slug string, failureThreshold, recoveryThreshold int32) store.Monitor {
	t.Helper()
	m, err := store.UpsertMonitor(ctx, st.Pool, store.UpsertMonitorParams{
		ProjectID: projectID, Slug: slug, ScheduleType: "interval", ScheduleValue: "1", ScheduleUnit: ptr("hour"),
		Timezone: "UTC", CheckinMarginMin: 1, MaxRuntimeMin: 10, FailureThreshold: failureThreshold, RecoveryThreshold: recoveryThreshold,
	})
	if err != nil {
		t.Fatal(err)
	}
	return m
}

func ptr[T any](v T) *T { return &v }

func countRows(t *testing.T, st *store.Store, q string, args ...any) int64 {
	t.Helper()
	var n int64
	if err := st.Pool.QueryRow(context.Background(), q, args...).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// TestCheckMonitorsMissed: a monitor whose next expected check-in has
// passed gets a synthetic `missed` check-in, its failure counter advances
// and — failure_threshold reached — monitor_failed fires exactly once,
// and next_expected_at moves into the future so it is not re-flagged
// every tick.
func TestCheckMonitorsMissed(t *testing.T) {
	st, p, s, n := setup(t)
	ctx := context.Background()
	m := upsertTestMonitor(t, ctx, st, p.ID, "nightly-backup", 1, 1)
	past := time.Now().UTC().Add(-2 * time.Hour)
	if _, err := st.Pool.Exec(ctx, "UPDATE monitors SET next_expected_at = $1 WHERE project_id = $2 AND slug = $3", past, p.ID, m.Slug); err != nil {
		t.Fatal(err)
	}

	if err := n.CheckMonitors(ctx); err != nil {
		t.Fatal(err)
	}
	if s.count() != 1 {
		t.Fatalf("webhook calls = %d, want 1", s.count())
	}
	if s.payloads[0].Type != TypeMonitorFailed {
		t.Errorf("payload type = %q", s.payloads[0].Type)
	}
	got, err := store.GetMonitor(ctx, st.Pool, p.ID, m.Slug)
	if err != nil {
		t.Fatal(err)
	}
	if got.ConsecutiveFailures != 1 || !got.Alerting || got.LastStatus == nil || *got.LastStatus != "missed" {
		t.Fatalf("monitor state = %+v", got)
	}
	if got.NextExpectedAt == nil || !got.NextExpectedAt.After(time.Now().UTC()) {
		t.Fatalf("next_expected_at not advanced past now: %+v", got.NextExpectedAt)
	}
	if n := countRows(t, st, "SELECT count(*) FROM monitor_checkins WHERE project_id = $1 AND monitor_slug = $2 AND status = 'missed'", p.ID, m.Slug); n != 1 {
		t.Fatalf("missed check-in rows = %d, want 1", n)
	}

	// A second tick, nothing newly due: no re-fire.
	if err := n.CheckMonitors(ctx); err != nil {
		t.Fatal(err)
	}
	if s.count() != 1 {
		t.Fatalf("re-fired on a second tick: %d webhook calls", s.count())
	}
}

// TestCheckMonitorsNotYetDue: a monitor whose next check-in is still in
// the future is left alone.
func TestCheckMonitorsNotYetDue(t *testing.T) {
	st, p, s, n := setup(t)
	ctx := context.Background()
	m := upsertTestMonitor(t, ctx, st, p.ID, "on-time", 1, 1)
	future := time.Now().UTC().Add(time.Hour)
	if _, err := st.Pool.Exec(ctx, "UPDATE monitors SET next_expected_at = $1 WHERE project_id = $2 AND slug = $3", future, p.ID, m.Slug); err != nil {
		t.Fatal(err)
	}
	if err := n.CheckMonitors(ctx); err != nil {
		t.Fatal(err)
	}
	if s.count() != 0 {
		t.Fatalf("webhook calls = %d, want 0", s.count())
	}
	if n := countRows(t, st, "SELECT count(*) FROM monitor_checkins WHERE project_id = $1", p.ID); n != 0 {
		t.Fatalf("check-in rows created for a not-yet-due monitor: %d", n)
	}
}

// TestCheckMonitorsTimeout: an in_progress run that outlived
// max_runtime_min is flipped to timeout and counts as a failure.
func TestCheckMonitorsTimeout(t *testing.T) {
	st, p, s, n := setup(t)
	ctx := context.Background()
	m := upsertTestMonitor(t, ctx, st, p.ID, "long-job", 1, 1)
	started := time.Now().UTC().Add(-time.Hour) // max_runtime_min is 10
	if _, err := st.Pool.Exec(ctx,
		"INSERT INTO monitor_checkins (started_at, project_id, monitor_slug, check_in_id, status) VALUES ($1, $2, $3, gen_random_uuid(), 'in_progress')",
		started, p.ID, m.Slug); err != nil {
		t.Fatal(err)
	}

	if err := n.CheckMonitors(ctx); err != nil {
		t.Fatal(err)
	}
	if s.count() != 1 || s.payloads[0].Type != TypeMonitorFailed {
		t.Fatalf("webhook calls = %d, payload = %+v", s.count(), s.payloads)
	}
	if n := countRows(t, st, "SELECT count(*) FROM monitor_checkins WHERE project_id = $1 AND monitor_slug = $2 AND status = 'timeout'", p.ID, m.Slug); n != 1 {
		t.Fatalf("timeout rows = %d, want 1", n)
	}
	got, err := store.GetMonitor(ctx, st.Pool, p.ID, m.Slug)
	if err != nil {
		t.Fatal(err)
	}
	if got.ConsecutiveFailures != 1 || !got.Alerting {
		t.Fatalf("monitor state = %+v", got)
	}

	// A second tick must not re-flip the same (now terminal) row or refire.
	if err := n.CheckMonitors(ctx); err != nil {
		t.Fatal(err)
	}
	if s.count() != 1 {
		t.Fatalf("re-fired: %d webhook calls", s.count())
	}
}
