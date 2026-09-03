package retention

import (
	"context"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/crashcartapp/crashcart/internal/config"
	"github.com/crashcartapp/crashcart/internal/sentry"
	"github.com/crashcartapp/crashcart/internal/store"
	"github.com/crashcartapp/crashcart/internal/testdb"
)

func TestWeekStart(t *testing.T) {
	// 2026-08-30 is a Sunday; the week starts Monday the 24th.
	got := weekStart(time.Date(2026, 8, 30, 15, 0, 0, 0, time.UTC))
	if want := time.Date(2026, 8, 24, 0, 0, 0, 0, time.UTC); !got.Equal(want) {
		t.Fatalf("weekStart = %v, want %v", got, want)
	}
	if got := weekStart(time.Date(2026, 8, 24, 0, 0, 0, 0, time.UTC)); !got.Equal(time.Date(2026, 8, 24, 0, 0, 0, 0, time.UTC)) {
		t.Fatalf("weekStart(monday) = %v", got)
	}
}

func TestPartitions(t *testing.T) {
	st := testdb.New(t)
	testdb.Projects(t, st, 1)
	ctx := context.Background()
	log := slog.Default()
	cfg := config.Config{RetentionDays: 14}
	now := time.Date(2026, 8, 30, 12, 0, 0, 0, time.UTC)

	// A row written before any partition exists lands in the default
	// partition and is still found through the parent.
	stray := now.Add(-24 * time.Hour)
	if _, err := st.Pool.Exec(ctx, `INSERT INTO events (occurred_at, project_id, event_id, level, message) VALUES ($1, 1, 'e1e1e1e1e1e1e1e1e1e1e1e1e1e1e1e1', 'fatal', 'm')`, stray); err != nil {
		t.Fatal(err)
	}
	if err := EnsurePartitions(ctx, st, cfg, now); err != nil {
		t.Fatal(err)
	}
	parts, err := Partitions(ctx, st, "events")
	if err != nil {
		t.Fatal(err)
	}
	// From the week holding the start of the retention window to two
	// weeks ahead: 2026-08-10 … 2026-09-07 inclusive = 5 weekly partitions
	// (a week earlier would be dropped by the same sweep: churn).
	if len(parts) != 5 || !parts[0].Start.Equal(time.Date(2026, 8, 10, 0, 0, 0, 0, time.UTC)) || !parts[4].Start.Equal(time.Date(2026, 9, 7, 0, 0, 0, 0, time.UTC)) {
		t.Fatalf("partitions = %+v", parts)
	}
	// The stray row moved into its week's partition; the default is empty.
	count := func(q string, args ...any) int64 {
		var n int64
		if err := st.Pool.QueryRow(ctx, q, args...).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	if n := count("SELECT count(*) FROM events_default"); n != 0 {
		t.Fatalf("events_default = %d rows", n)
	}
	if n := count("SELECT count(*) FROM events_p20260824"); n != 1 {
		t.Fatalf("events_p20260824 = %d rows", n)
	}
	if n := count("SELECT count(*) FROM events WHERE project_id = 1"); n != 1 {
		t.Fatalf("events = %d rows", n)
	}
	// The moved partition carries the parent's indexes and keys.
	if n := count("SELECT count(*) FROM pg_indexes WHERE tablename = 'events_p20260824'"); n < 5 {
		t.Fatalf("indexes on the attached partition = %d", n)
	}
	// Idempotent.
	if err := EnsurePartitions(ctx, st, cfg, now); err != nil {
		t.Fatal(err)
	}
	// Two weeks later (cutoff 08-30 12:00) the partitions ending by then
	// have expired; the one holding the event ends on 08-31 and stays.
	later := now.Add(2 * PartitionWidth)
	if err := EnsurePartitions(ctx, st, cfg, later); err != nil {
		t.Fatal(err)
	}
	if err := dropExpiredPartitions(ctx, st, cfg, later, log); err != nil {
		t.Fatal(err)
	}
	parts, _ = Partitions(ctx, st, "events")
	if len(parts) == 0 || parts[0].Start.Before(time.Date(2026, 8, 24, 0, 0, 0, 0, time.UTC)) {
		t.Fatalf("after drop: oldest partition %+v", parts[0])
	}
	if n := count("SELECT count(*) FROM events WHERE project_id = 1"); n != 1 {
		t.Fatalf("events after drop = %d rows", n)
	}
	// Another week and it is gone with its partition.
	if err := dropExpiredPartitions(ctx, st, cfg, later.Add(PartitionWidth), log); err != nil {
		t.Fatal(err)
	}
	if n := count("SELECT count(*) FROM events WHERE project_id = 1"); n != 0 {
		t.Fatalf("events after expiry = %d rows", n)
	}
}

func TestRollup(t *testing.T) {
	st := testdb.New(t)
	testdb.Projects(t, st, 1)
	ctx := context.Background()
	// Two events in an hour two days ago and one in the current hour, marked
	// dirty as ingest does. A crash that arrived days late is the normal
	// case for mobile SDKs: it must count in its own hour.
	old := time.Now().UTC().Add(-48 * time.Hour).Truncate(time.Hour).Add(10 * time.Minute)
	cur := time.Now().UTC()
	insert := func(at time.Time, id string, level string) {
		t.Helper()
		// A fatal row is a crash: unhandled (exception.mechanism.handled = false), like the SDKs send it.
		if _, err := st.Pool.Exec(ctx, `INSERT INTO events (occurred_at, project_id, event_id, level, message, fingerprint, release, handled) VALUES ($1, 1, $2, $3, 'm', 'f1f1f1f1f1f1f1f1f1f1f1f1f1f1f1f1', '1.0', CASE WHEN $3::event_level = 'fatal' THEN false END)`, at, id, level); err != nil {
			t.Fatal(err)
		}
		if err := store.MarkEventStatsDirty(ctx, st.Pool, 1, []time.Time{at.Truncate(time.Hour)}); err != nil {
			t.Fatal(err)
		}
	}
	insert(old, "e1e1e1e1e1e1e1e1e1e1e1e1e1e1e1e1", "fatal")
	insert(old.Add(time.Minute), "e2e2e2e2e2e2e2e2e2e2e2e2e2e2e2e2", "error")
	insert(cur, "e3e3e3e3e3e3e3e3e3e3e3e3e3e3e3e3", "fatal")

	from, to := old.Add(-72*time.Hour).Truncate(24*time.Hour), cur.Add(time.Hour)
	totals := func() store.TotalsRow {
		t.Helper()
		r, err := store.Totals(ctx, st.Pool, 1, from, to)
		if err != nil {
			t.Fatal(err)
		}
		return r
	}
	// Before any rollup the views compute the dirty hours live: exact.
	if r := totals(); r.Events != 3 || r.Unhandled != 2 {
		t.Fatalf("live totals = %+v", r)
	}
	// One pass rolls the past hour up and leaves the current hour dirty.
	n, err := Rollup(ctx, st, config.Config{})
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("rolled %d keys, want 1", n)
	}
	dirty, _ := store.CountDirtyStats(ctx, st.Pool)
	if dirty != 1 {
		t.Fatalf("dirty after rollup = %d, want 1 (the current hour)", dirty)
	}
	var rolledRows int64
	st.Pool.QueryRow(ctx, "SELECT count(*) FROM event_stats_hourly_rolled").Scan(&rolledRows)
	if rolledRows != 2 { // fatal + error rows of the old hour
		t.Fatalf("rolled rows = %d", rolledRows)
	}
	if r := totals(); r.Events != 3 || r.Unhandled != 2 {
		t.Fatalf("totals after rollup = %+v", r)
	}
	// The per-issue counts came along.
	spark, err := store.IssueTimeline(ctx, st.Pool, 1, "f1f1f1f1f1f1f1f1f1f1f1f1f1f1f1f1", from, to, 3600)
	if err != nil {
		t.Fatal(err)
	}
	var issueEvents int64
	for _, b := range spark {
		issueEvents += b.Events
	}
	if issueEvents != 3 {
		t.Fatalf("issue timeline events = %d", issueEvents)
	}
	// A late event into the rolled hour marks it again and the numbers
	// follow, before and after the next pass.
	insert(old.Add(2*time.Minute), "e4e4e4e4e4e4e4e4e4e4e4e4e4e4e4e4", "fatal")
	if r := totals(); r.Events != 4 || r.Unhandled != 3 {
		t.Fatalf("live totals after late event = %+v", r)
	}
	if err := RollupAll(ctx, st, config.Config{}); err != nil {
		t.Fatal(err)
	}
	if r := totals(); r.Events != 4 || r.Unhandled != 3 {
		t.Fatalf("totals after second rollup = %+v", r)
	}
	// Nothing left but the current hour.
	if n, _ := Rollup(ctx, st, config.Config{}); n != 0 {
		t.Fatalf("third pass rolled %d keys", n)
	}

	// Sessions: an aggregate row, rolled up, then its hour recomputed after
	// a status update (a crash reported on the next launch).
	sat := old
	if _, err := st.Pool.Exec(ctx, `INSERT INTO sessions (started_at, project_id, sid, release, status, count) VALUES ($1, 1, 's1', '1.0', 'ok', 5)`, sat); err != nil {
		t.Fatal(err)
	}
	if err := store.MarkSessionStatsDirty(ctx, st.Pool, 1, []time.Time{sat.Truncate(time.Hour)}); err != nil {
		t.Fatal(err)
	}
	if err := RollupAll(ctx, st, config.Config{}); err != nil {
		t.Fatal(err)
	}
	health := func() (total, crashed int64) {
		rows, err := store.ReleaseHealth(ctx, st.Pool, 1, from, to)
		if err != nil {
			t.Fatal(err)
		}
		for _, r := range rows {
			total += r.Total
			crashed += r.Crashed
		}
		return
	}
	if total, crashed := health(); total != 5 || crashed != 0 {
		t.Fatalf("health = %d/%d", crashed, total)
	}
	if _, err := st.Pool.Exec(ctx, `UPDATE sessions SET status = 'crashed' WHERE sid = 's1'`); err != nil {
		t.Fatal(err)
	}
	if err := store.MarkSessionStatsDirty(ctx, st.Pool, 1, []time.Time{sat.Truncate(time.Hour)}); err != nil {
		t.Fatal(err)
	}
	if total, crashed := health(); total != 5 || crashed != 5 {
		t.Fatalf("live health after update = %d/%d", crashed, total)
	}
	if err := RollupAll(ctx, st, config.Config{}); err != nil {
		t.Fatal(err)
	}
	if total, crashed := health(); total != 5 || crashed != 5 {
		t.Fatalf("rolled health after update = %d/%d", crashed, total)
	}
}

// TestRollupKeepsExpiredHours: an hour older than RETENTION_DAYS is never
// recomputed from the raw rows (they are gone, or a lone event with a
// wrong clock is all that is left); its rolled row is the record, and the
// dirty key is just cleared.
func TestRollupKeepsExpiredHours(t *testing.T) {
	st := testdb.New(t)
	testdb.Projects(t, st, 1)
	ctx := context.Background()
	cfg := config.Config{RetentionDays: 30}
	old := time.Now().UTC().Add(-60 * 24 * time.Hour).Truncate(time.Hour)
	if _, err := st.Pool.Exec(ctx, `INSERT INTO event_stats_hourly_rolled (bucket, project_id, release, platform, level, events, unhandled, errors)
		VALUES ($1, 1, '1.0', 'android', 'fatal', 1000, 1000, 0)`, old); err != nil {
		t.Fatal(err)
	}
	// A late event with a clock 60 days behind: one raw row, hour marked dirty.
	if _, err := st.Pool.Exec(ctx, `INSERT INTO events (occurred_at, project_id, event_id, level, message, release, platform, handled)
		VALUES ($1, 1, 'aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa', 'fatal', 'late', '1.0', 'android', false)`, old.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	if err := store.MarkEventStatsDirty(ctx, st.Pool, 1, []time.Time{old}); err != nil {
		t.Fatal(err)
	}
	if err := RollupAll(ctx, st, cfg); err != nil {
		t.Fatal(err)
	}
	var events int64
	if err := st.Pool.QueryRow(ctx, `SELECT events FROM event_stats_hourly_rolled WHERE project_id = 1 AND bucket = $1`, old).Scan(&events); err != nil || events != 1000 {
		t.Fatalf("rolled row after late event = %d %v (want 1000, untouched)", events, err)
	}
	if n, _ := store.CountDirtyStats(ctx, st.Pool); n != 0 {
		t.Fatalf("dirty keys left = %d", n)
	}
	// An hour inside the window is recomputed as usual.
	recent := time.Now().UTC().Add(-3 * time.Hour).Truncate(time.Hour)
	if _, err := st.Pool.Exec(ctx, `INSERT INTO events (occurred_at, project_id, event_id, level, message, release, platform, handled)
		VALUES ($1, 1, 'bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb', 'error', 'x', '1.0', 'android', true)`, recent.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	if err := store.MarkEventStatsDirty(ctx, st.Pool, 1, []time.Time{recent}); err != nil {
		t.Fatal(err)
	}
	if err := RollupAll(ctx, st, cfg); err != nil {
		t.Fatal(err)
	}
	if err := st.Pool.QueryRow(ctx, `SELECT events FROM event_stats_hourly_rolled WHERE project_id = 1 AND bucket = $1`, recent).Scan(&events); err != nil || events != 1 {
		t.Fatalf("recent hour rolled = %d %v", events, err)
	}
}

// TestEnsurePartitionsConcurrently: replicas starting together must not
// fail on the partition the other one just created.
func TestEnsurePartitionsConcurrently(t *testing.T) {
	st := testdb.New(t)
	ctx := context.Background()
	cfg := config.Config{RetentionDays: 7}
	now := time.Date(2026, 8, 30, 12, 0, 0, 0, time.UTC)
	errs := make(chan error, 4)
	for i := 0; i < 4; i++ {
		go func() { errs <- EnsurePartitions(ctx, st, cfg, now) }()
	}
	for i := 0; i < 4; i++ {
		if err := <-errs; err != nil {
			t.Fatalf("concurrent EnsurePartitions: %v", err)
		}
	}
	parts, err := Partitions(ctx, st, "events")
	if err != nil || len(parts) != 4 { // the cutoff's week to two weeks ahead
		t.Fatalf("partitions = %d %v", len(parts), err)
	}
}

// TestSweepDoesNotChurnPartitions: an hourly sweep must not create a
// partition it then drops (every such round takes exclusive locks on the
// parent tables).
func TestSweepDoesNotChurnPartitions(t *testing.T) {
	st := testdb.New(t)
	ctx := context.Background()
	cfg := config.Config{RetentionDays: 14}
	now := time.Now().UTC()
	if err := EnsurePartitions(ctx, st, cfg, now); err != nil {
		t.Fatal(err)
	}
	before, _ := Partitions(ctx, st, "events")
	if err := Sweep(ctx, st, cfg, slog.New(slog.NewTextHandler(io.Discard, nil))); err != nil {
		t.Fatal(err)
	}
	after, _ := Partitions(ctx, st, "events")
	if len(before) == 0 || len(after) != len(before) || !after[0].Start.Equal(before[0].Start) {
		t.Fatalf("sweep changed the partition set: %v → %v", before, after)
	}
	for _, p := range after {
		if !p.Start.Add(PartitionWidth).After(now.Add(-cfg.Retention())) {
			t.Errorf("partition %s ends before the retention cutoff: it would be dropped at once", p.Name)
		}
	}
}

// TestExpireIssues: resolved / ignored issues go once their events'
// partitions are gone (the week boundary, not the exact cutoff — an issue
// deleted while its events still list would be re-created as new by the
// next event); unresolved ones go after StaleIssueFactor retentions; both
// take their rollup rows along.
func TestExpireIssues(t *testing.T) {
	st := testdb.New(t)
	testdb.Projects(t, st, 1)
	ctx := context.Background()
	cfg := config.Config{RetentionDays: 30}
	now := time.Date(2026, 8, 30, 12, 0, 0, 0, time.UTC)
	boundary := weekStart(now.Add(-30 * 24 * time.Hour)) // 2026-07-27
	add := func(name, status string, lastSeen time.Time) sentry.ID {
		fp := sentry.DerivedID([]byte(name))
		if _, err := st.Pool.Exec(ctx, `INSERT INTO issues (project_id, fingerprint, title, level, status, first_seen, last_seen)
			VALUES (1, $1, $2, 'error', $3, $4, $4)`, fp, name, status, lastSeen); err != nil {
			t.Fatal(err)
		}
		if _, err := st.Pool.Exec(ctx, `INSERT INTO issue_stats_hourly_rolled (bucket, project_id, fingerprint, events) VALUES ($1, 1, $2, 3)`, lastSeen.Truncate(time.Hour), fp); err != nil {
			t.Fatal(err)
		}
		return fp
	}
	oldResolved := add("old-resolved", "resolved", boundary.Add(-time.Hour))
	edgeResolved := add("edge-resolved", "resolved", boundary.Add(time.Hour)) // inside a surviving partition: kept
	staleOpen := add("stale-open", "unresolved", now.Add(-5*30*24*time.Hour))
	oldOpen := add("old-open", "unresolved", now.Add(-2*30*24*time.Hour)) // 2 retentions: kept
	n, err := ExpireIssues(ctx, st, cfg, now)
	if err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Fatalf("expired %d issues, want 2", n)
	}
	left := map[sentry.ID]bool{}
	rows, _ := st.Pool.Query(ctx, "SELECT fingerprint FROM issues")
	for rows.Next() {
		var fp sentry.ID
		rows.Scan(&fp)
		left[fp] = true
	}
	rows.Close()
	if left[oldResolved] || left[staleOpen] || !left[edgeResolved] || !left[oldOpen] {
		t.Fatalf("issues left = %v", left)
	}
	var rolled int
	st.Pool.QueryRow(ctx, "SELECT count(*) FROM issue_stats_hourly_rolled").Scan(&rolled)
	if rolled != 2 {
		t.Fatalf("rollup rows of deleted issues not removed: %d left, want 2", rolled)
	}
}

// TestAttachmentPartitions: attachments are partitioned like events and a
// row lands in its week's partition.
func TestAttachmentPartitions(t *testing.T) {
	st := testdb.New(t)
	testdb.Projects(t, st, 1)
	ctx := context.Background()
	now := time.Date(2026, 8, 30, 12, 0, 0, 0, time.UTC)
	if err := EnsurePartitions(ctx, st, config.Config{RetentionDays: 14}, now); err != nil {
		t.Fatal(err)
	}
	parts, err := Partitions(ctx, st, "attachments")
	if err != nil || len(parts) != 5 || parts[0].Name != "attachments_p20260810" {
		t.Fatalf("partitions = %+v %v", parts, err)
	}
	if _, err := st.Pool.Exec(ctx, `INSERT INTO attachments (occurred_at, project_id, event_id, n, filename, content_type, attachment_type, size, data)
		VALUES ($1, 1, 'e1e1e1e1e1e1e1e1e1e1e1e1e1e1e1e1', 0, 'screenshot.png', 'image/png', 'event.attachment', 3, 'png')`, now.Add(-time.Hour)); err != nil {
		t.Fatal(err)
	}
	var in string
	if err := st.Pool.QueryRow(ctx, "SELECT tableoid::regclass::text FROM attachments").Scan(&in); err != nil || in != "attachments_p20260824" {
		t.Fatalf("row in %q (%v)", in, err)
	}
}

// TestSweepExpiresOldUserReports: user_reports has no partition to ride
// out with events (schema.sql) — Sweep cuts it on its own received_at
// against the retention window, independent of whether its event exists.
func TestSweepExpiresOldUserReports(t *testing.T) {
	st := testdb.New(t)
	testdb.Projects(t, st, 1)
	ctx := context.Background()
	cfg := config.Config{RetentionDays: 14}
	now := time.Now().UTC()
	add := func(name string, receivedAt time.Time) {
		id := sentry.DerivedID([]byte(name))
		if _, err := st.Pool.Exec(ctx, `INSERT INTO user_reports (project_id, event_id, received_at, comments) VALUES (1, $1, $2, 'x')`, id, receivedAt); err != nil {
			t.Fatal(err)
		}
	}
	add("old", now.Add(-15*24*time.Hour))
	add("recent", now.Add(-time.Hour))
	if err := Sweep(ctx, st, cfg, slog.New(slog.NewTextHandler(io.Discard, nil))); err != nil {
		t.Fatal(err)
	}
	var left int
	if err := st.Pool.QueryRow(ctx, "SELECT count(*) FROM user_reports").Scan(&left); err != nil {
		t.Fatal(err)
	}
	if left != 1 {
		t.Fatalf("user_reports left = %d, want 1", left)
	}
}
