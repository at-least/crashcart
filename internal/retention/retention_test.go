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
	// From the week before the retention window to two weeks ahead:
	// 2026-08-03 … 2026-09-07 inclusive = 6 weekly partitions.
	if len(parts) != 6 || !parts[0].Start.Equal(time.Date(2026, 8, 3, 0, 0, 0, 0, time.UTC)) || !parts[5].Start.Equal(time.Date(2026, 9, 7, 0, 0, 0, 0, time.UTC)) {
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
	// Reconcile and Sweep run end to end (the in-memory object store has
	// no lifecycle to set).
	if err := Reconcile(ctx, st, cfg, log); err != nil {
		t.Fatal(err)
	}
	if err := Sweep(ctx, st, cfg, log); err != nil {
		t.Fatal(err)
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
		if _, err := st.Pool.Exec(ctx, `INSERT INTO events (occurred_at, project_id, event_id, level, message, fingerprint, release) VALUES ($1, 1, $2, $3, 'm', 'f1f1f1f1f1f1f1f1f1f1f1f1f1f1f1f1', '1.0')`, at, id, level); err != nil {
			t.Fatal(err)
		}
		if err := st.MarkEventStatsDirty(ctx, sqlc.MarkEventStatsDirtyParams{ProjectID: 1, Buckets: []time.Time{at.Truncate(time.Hour)}}); err != nil {
			t.Fatal(err)
		}
	}
	insert(old, "e1e1e1e1e1e1e1e1e1e1e1e1e1e1e1e1", "fatal")
	insert(old.Add(time.Minute), "e2e2e2e2e2e2e2e2e2e2e2e2e2e2e2e2", "error")
	insert(cur, "e3e3e3e3e3e3e3e3e3e3e3e3e3e3e3e3", "fatal")

	from, to := old.Add(-72*time.Hour).Truncate(24*time.Hour), cur.Add(time.Hour)
	totals := func() sqlc.TotalsRow {
		t.Helper()
		r, err := st.Totals(ctx, sqlc.TotalsParams{ProjectID: 1, FromAt: from, ToAt: to, Width: 3600})
		if err != nil {
			t.Fatal(err)
		}
		return r
	}
	// Before any rollup the views compute the dirty hours live: exact.
	if r := totals(); r.Events != 3 || r.Crashes != 2 {
		t.Fatalf("live totals = %+v", r)
	}
	// One pass rolls the past hour up and leaves the current hour dirty.
	n, err := Rollup(ctx, st)
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("rolled %d keys, want 1", n)
	}
	dirty, _ := DirtyHours(ctx, st)
	if dirty != 1 {
		t.Fatalf("dirty after rollup = %d, want 1 (the current hour)", dirty)
	}
	var rolledRows int64
	st.Pool.QueryRow(ctx, "SELECT count(*) FROM event_stats_hourly_rolled").Scan(&rolledRows)
	if rolledRows != 2 { // fatal + error rows of the old hour
		t.Fatalf("rolled rows = %d", rolledRows)
	}
	if r := totals(); r.Events != 3 || r.Crashes != 2 {
		t.Fatalf("totals after rollup = %+v", r)
	}
	// The per-issue counts came along.
	spark, err := st.IssueTimeline(ctx, sqlc.IssueTimelineParams{ProjectID: 1, Fingerprint: "f1f1f1f1f1f1f1f1f1f1f1f1f1f1f1f1", FromAt: from, ToAt: to, Width: 3600})
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
	if r := totals(); r.Events != 4 || r.Crashes != 3 {
		t.Fatalf("live totals after late event = %+v", r)
	}
	if err := RollupAll(ctx, st); err != nil {
		t.Fatal(err)
	}
	if r := totals(); r.Events != 4 || r.Crashes != 3 {
		t.Fatalf("totals after second rollup = %+v", r)
	}
	// Nothing left but the current hour.
	if n, _ := Rollup(ctx, st); n != 0 {
		t.Fatalf("third pass rolled %d keys", n)
	}

	// Sessions: an aggregate row, rolled up, then its hour recomputed after
	// a status update (a crash reported on the next launch).
	sat := old
	if _, err := st.Pool.Exec(ctx, `INSERT INTO sessions (started_at, project_id, sid, release, status, count) VALUES ($1, 1, 's1', '1.0', 'ok', 5)`, sat); err != nil {
		t.Fatal(err)
	}
	if err := st.MarkSessionStatsDirty(ctx, sqlc.MarkSessionStatsDirtyParams{ProjectID: 1, Buckets: []time.Time{sat.Truncate(time.Hour)}}); err != nil {
		t.Fatal(err)
	}
	if err := RollupAll(ctx, st); err != nil {
		t.Fatal(err)
	}
	health := func() (total, crashed int64) {
		rows, err := st.ReleaseHealth(ctx, sqlc.ReleaseHealthParams{ProjectID: 1, Bucket: from, Bucket_2: to})
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
	if err := st.MarkSessionStatsDirty(ctx, sqlc.MarkSessionStatsDirtyParams{ProjectID: 1, Buckets: []time.Time{sat.Truncate(time.Hour)}}); err != nil {
		t.Fatal(err)
	}
	if total, crashed := health(); total != 5 || crashed != 5 {
		t.Fatalf("live health after update = %d/%d", crashed, total)
	}
	if err := RollupAll(ctx, st); err != nil {
		t.Fatal(err)
	}
	if total, crashed := health(); total != 5 || crashed != 5 {
		t.Fatalf("rolled health after update = %d/%d", crashed, total)
	}
}
