package retention

import (
	"context"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/crashcartapp/crashcart/internal/config"
	"github.com/crashcartapp/crashcart/internal/sentry"
	"github.com/crashcartapp/crashcart/internal/store"
	"github.com/crashcartapp/crashcart/internal/testdb"
)

// Tests of the documented retention claims (ARCHITECTURE.md), each one
// written so that it fails if the claim were false.

var quiet = slog.New(slog.NewTextHandler(io.Discard, nil))

func count(t *testing.T, st *store.Store, q string, args ...any) int64 {
	t.Helper()
	var n int64
	if err := st.Pool.QueryRow(context.Background(), q, args...).Scan(&n); err != nil {
		t.Fatalf("%s: %v", q, err)
	}
	return n
}

// TestPartitionsAllTablesInStep: attachments and sessions are partitioned
// exactly like events — the same weekly set is created and the same
// partitions are dropped, and a row one day past RETENTION_DAYS survives
// while the partition holding it has not ended.
func TestPartitionsAllTablesInStep(t *testing.T) {
	st := testdb.New(t)
	testdb.Projects(t, st, 1)
	ctx := context.Background()
	cfg := config.Config{RetentionDays: 14}
	now := time.Date(2026, 8, 30, 12, 0, 0, 0, time.UTC) // Sunday
	if err := EnsurePartitions(ctx, st, cfg, now); err != nil {
		t.Fatal(err)
	}
	starts := func(table string) []time.Time {
		parts, err := Partitions(ctx, st, table)
		if err != nil {
			t.Fatal(err)
		}
		var out []time.Time
		for _, p := range parts {
			if p.Name != table+"_p"+p.Start.Format("20060102") {
				t.Errorf("partition name %q is not <table>_pYYYYMMDD of its start", p.Name)
			}
			if p.Start.Weekday() != time.Monday || !p.Start.Equal(p.Start.Truncate(24*time.Hour)) {
				t.Errorf("partition %s does not start on a Monday midnight UTC", p.Name)
			}
			out = append(out, p.Start)
		}
		return out
	}
	ev, at, se := starts("events"), starts("attachments"), starts("sessions")
	if len(ev) != 5 || len(at) != len(ev) || len(se) != len(ev) {
		t.Fatalf("partition counts: events=%d attachments=%d sessions=%d", len(ev), len(at), len(se))
	}
	for i := range ev {
		if !at[i].Equal(ev[i]) || !se[i].Equal(ev[i]) {
			t.Fatalf("partition sets differ at %d: %v %v %v", i, ev[i], at[i], se[i])
		}
	}
	// A session 15 days old (one day past retention) lies in the week of
	// 08-10..08-17; that partition ends before the cutoff only once the
	// cutoff passes 08-17.
	sessionAt := now.Add(-15 * 24 * time.Hour) // 08-15 12:00
	if _, err := st.Pool.Exec(ctx, `INSERT INTO sessions (started_at, project_id, sid, release, status) VALUES ($1, 1, 's', '1.0', 'ok')`, sessionAt); err != nil {
		t.Fatal(err)
	}
	if _, err := st.Pool.Exec(ctx, `INSERT INTO attachments (occurred_at, project_id, event_id, n, filename, content_type, attachment_type, size, data)
		VALUES ($1, 1, $2, 0, 'a', 'text/plain', 'event.attachment', 1, 'x')`, sessionAt, sentry.DerivedID([]byte("a"))); err != nil {
		t.Fatal(err)
	}
	if err := dropExpiredPartitions(ctx, st, cfg, now, quiet); err != nil {
		t.Fatal(err)
	}
	if n := count(t, st, "SELECT count(*) FROM sessions"); n != 1 {
		t.Fatalf("session one day past retention was dropped early (rows=%d): partitions must live until they end", n)
	}
	if n := count(t, st, "SELECT count(*) FROM attachments"); n != 1 {
		t.Fatalf("attachment one day past retention was dropped early (rows=%d)", n)
	}
	// Cutoff past 08-17 (now = 09-01 12:00 → cutoff 08-18 12:00): the
	// 08-10 partition ended before it and goes from every table (08-17
	// ends on 08-24, after the cutoff, and stays).
	later := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)
	if err := dropExpiredPartitions(ctx, st, cfg, later, quiet); err != nil {
		t.Fatal(err)
	}
	if n := count(t, st, "SELECT count(*) FROM sessions"); n != 0 {
		t.Fatalf("session still present after its partition ended before the cutoff: %d", n)
	}
	if n := count(t, st, "SELECT count(*) FROM attachments"); n != 0 {
		t.Fatalf("attachment still present after its partition ended before the cutoff: %d", n)
	}
	ev, at, se = starts("events"), starts("attachments"), starts("sessions")
	if len(ev) != 4 || len(at) != 4 || len(se) != 4 || !ev[0].Equal(time.Date(2026, 8, 17, 0, 0, 0, 0, time.UTC)) {
		t.Fatalf("after drop: events=%v attachments=%v sessions=%v", ev, at, se)
	}
}

// TestDefaultPartitionSweep: the default partition is swept row by row —
// a row older than the cutoff goes, a younger stray stays, and rows in
// real partitions are untouched by that DELETE.
func TestDefaultPartitionSweep(t *testing.T) {
	st := testdb.New(t)
	testdb.Projects(t, st, 1)
	ctx := context.Background()
	cfg := config.Config{RetentionDays: 14}
	now := time.Date(2026, 8, 30, 12, 0, 0, 0, time.UTC)
	ins := func(at time.Time, name string) {
		t.Helper()
		if _, err := st.Pool.Exec(ctx, `INSERT INTO events (occurred_at, project_id, event_id, level, message) VALUES ($1, 1, $2, 'error', $3)`, at, sentry.DerivedID([]byte(name)), name); err != nil {
			t.Fatal(err)
		}
		if _, err := st.Pool.Exec(ctx, `INSERT INTO sessions (started_at, project_id, sid, release, status) VALUES ($1, 1, $2, '1.0', 'ok')`, at, name); err != nil {
			t.Fatal(err)
		}
	}
	// No partitions at all: everything lands in the default partition.
	ins(now.Add(-40*24*time.Hour), "old-clock")
	ins(now.Add(-2*time.Hour), "recent")
	if n := count(t, st, "SELECT count(*) FROM events_default"); n != 2 {
		t.Fatalf("rows in events_default = %d, want 2 (an insert must never fail for want of a partition)", n)
	}
	if err := dropExpiredPartitions(ctx, st, cfg, now, quiet); err != nil {
		t.Fatal(err)
	}
	if n := count(t, st, "SELECT count(*) FROM events_default"); n != 1 {
		t.Fatalf("events_default after sweep = %d rows, want 1 (only the row older than the cutoff goes)", n)
	}
	if n := count(t, st, "SELECT count(*) FROM sessions_default"); n != 1 {
		t.Fatalf("sessions_default after sweep = %d rows, want 1", n)
	}
	if n := count(t, st, "SELECT count(*) FROM events WHERE message = 'recent'"); n != 1 {
		t.Fatal("the recent stray row must survive the default-partition sweep")
	}
	// Creating the week's partition moves the stray into it; the move
	// keeps the row addressable by its key.
	if err := EnsurePartitions(ctx, st, cfg, now); err != nil {
		t.Fatal(err)
	}
	var in string
	if err := st.Pool.QueryRow(ctx, "SELECT tableoid::regclass::text FROM events WHERE message = 'recent'").Scan(&in); err != nil || in != "events_p20260824" {
		t.Fatalf("stray row is in %q (%v), want events_p20260824", in, err)
	}
	if n := count(t, st, "SELECT count(*) FROM events_default"); n != 0 {
		t.Fatalf("events_default still holds %d rows after the move", n)
	}
	var sin string
	if err := st.Pool.QueryRow(ctx, "SELECT tableoid::regclass::text FROM sessions WHERE sid = 'recent'").Scan(&sin); err != nil || sin != "sessions_p20260824" {
		t.Fatalf("stray session is in %q (%v)", sin, err)
	}
}

// TestPartitionStorageExternal: payload / data are STORAGE EXTERNAL on
// the parent, on a partition created as PARTITION OF, and on one created
// standalone (the move path) — TOAST must not re-compress gzip / PNG.
func TestPartitionStorageExternal(t *testing.T) {
	st := testdb.New(t)
	testdb.Projects(t, st, 1)
	ctx := context.Background()
	cfg := config.Config{RetentionDays: 14}
	now := time.Date(2026, 8, 30, 12, 0, 0, 0, time.UTC)
	// A stray row makes the 08-24 partition take the standalone path.
	if _, err := st.Pool.Exec(ctx, `INSERT INTO events (occurred_at, project_id, event_id, level, message) VALUES ($1, 1, $2, 'error', 'm')`, now.Add(-time.Hour), sentry.DerivedID([]byte("s"))); err != nil {
		t.Fatal(err)
	}
	if _, err := st.Pool.Exec(ctx, `INSERT INTO attachments (occurred_at, project_id, event_id, n, filename, content_type, attachment_type, size, data)
		VALUES ($1, 1, $2, 0, 'a', 'text/plain', 'event.attachment', 1, 'x')`, now.Add(-time.Hour), sentry.DerivedID([]byte("s"))); err != nil {
		t.Fatal(err)
	}
	if err := EnsurePartitions(ctx, st, cfg, now); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct{ table, column string }{
		{"events", "payload"}, {"events_default", "payload"}, {"events_p20260824", "payload"}, {"events_p20260817", "payload"},
		{"attachments", "data"}, {"attachments_default", "data"}, {"attachments_p20260824", "data"}, {"attachments_p20260817", "data"},
	} {
		var storage string
		if err := st.Pool.QueryRow(ctx, `SELECT attstorage::text FROM pg_attribute WHERE attrelid = $1::regclass AND attname = $2`, c.table, c.column).Scan(&storage); err != nil {
			t.Fatalf("%s.%s: %v", c.table, c.column, err)
		}
		if storage != "e" {
			t.Errorf("%s.%s storage = %q, want e (EXTERNAL)", c.table, c.column, storage)
		}
	}
}

// TestRollupKeepsKeyMarkedMeanwhile: a dirty key whose gen moved between
// the rollup's read and its delete survives the pass (and is picked up by
// the next one); a key that did not move is cleared.
func TestRollupKeepsKeyMarkedMeanwhile(t *testing.T) {
	st := testdb.New(t)
	testdb.Projects(t, st, 1)
	ctx := context.Background()
	hour := time.Now().UTC().Add(-3 * time.Hour).Truncate(time.Hour)
	mark := func() {
		t.Helper()
		if err := store.MarkEventStatsDirty(ctx, st.Pool, 1, []time.Time{hour}); err != nil {
			t.Fatal(err)
		}
	}
	mark()
	var gen int64
	st.Pool.QueryRow(ctx, "SELECT gen FROM event_stats_dirty").Scan(&gen)
	if gen != 1 {
		t.Fatalf("first mark gen = %d, want 1", gen)
	}
	mark()
	st.Pool.QueryRow(ctx, "SELECT gen FROM event_stats_dirty").Scan(&gen)
	if gen != 2 {
		t.Fatalf("second mark gen = %d, want 2 (every mark bumps it)", gen)
	}
	cutoff := time.Now().Add(-30 * 24 * time.Hour)
	// A recompute that is interleaved with an ingest mark.
	n, err := rollup(ctx, st, "event_stats_dirty", cutoff, func(ctx context.Context, tx pgx.Tx, pids []int64, buckets []time.Time, lo, hi time.Time) error {
		mark()
		return rollupEvents(ctx, tx, pids, buckets, lo, hi)
	})
	if err != nil || n != 1 {
		t.Fatalf("rollup: n=%d err=%v", n, err)
	}
	if left := count(t, st, "SELECT count(*) FROM event_stats_dirty WHERE bucket = $1", hour); left != 1 {
		t.Fatalf("a key marked during the rollup was cleared (left=%d): the delete must be conditional on gen", left)
	}
	// Without interference the next pass clears it.
	n, err = rollup(ctx, st, "event_stats_dirty", cutoff, rollupEvents)
	if err != nil || n != 1 {
		t.Fatalf("second rollup: n=%d err=%v", n, err)
	}
	if left := count(t, st, "SELECT count(*) FROM event_stats_dirty"); left != 0 {
		t.Fatalf("key left after an undisturbed pass: %d", left)
	}
}

// TestRollupFingerprintMove: after symbolication moves an event to another
// issue (UPDATE fingerprint + re-mark), the per-issue counts follow — live
// while the hour is dirty and again after the rollup, without double
// counting.
func TestRollupFingerprintMove(t *testing.T) {
	st := testdb.New(t)
	testdb.Projects(t, st, 1)
	ctx := context.Background()
	hour := time.Now().UTC().Add(-5 * time.Hour).Truncate(time.Hour)
	a, b := sentry.DerivedID([]byte("issue-a")), sentry.DerivedID([]byte("issue-b"))
	for i, name := range []string{"e1", "e2"} {
		if _, err := st.Pool.Exec(ctx, `INSERT INTO events (occurred_at, project_id, event_id, level, message, fingerprint) VALUES ($1, 1, $2, 'error', 'm', $3)`,
			hour.Add(time.Duration(i+1)*time.Minute), sentry.DerivedID([]byte(name)), a); err != nil {
			t.Fatal(err)
		}
	}
	mark := func() {
		t.Helper()
		if err := store.MarkEventStatsDirty(ctx, st.Pool, 1, []time.Time{hour}); err != nil {
			t.Fatal(err)
		}
	}
	mark()
	timeline := func(fp sentry.ID) int64 {
		t.Helper()
		rows, err := store.IssueTimeline(ctx, st.Pool, 1, fp, hour.Add(-time.Hour), hour.Add(2*time.Hour), 3600)
		if err != nil {
			t.Fatal(err)
		}
		var n int64
		for _, r := range rows {
			n += r.Events
		}
		return n
	}
	if timeline(a) != 2 || timeline(b) != 0 {
		t.Fatalf("live before rollup: a=%d b=%d", timeline(a), timeline(b))
	}
	if err := RollupAll(ctx, st, config.Config{}); err != nil {
		t.Fatal(err)
	}
	if timeline(a) != 2 || timeline(b) != 0 {
		t.Fatalf("rolled: a=%d b=%d", timeline(a), timeline(b))
	}
	if _, err := st.Pool.Exec(ctx, `UPDATE events SET fingerprint = $1 WHERE event_id = $2`, b, sentry.DerivedID([]byte("e2"))); err != nil {
		t.Fatal(err)
	}
	// Before the re-mark the rolled row is still what the view shows.
	if timeline(a) != 2 || timeline(b) != 0 {
		t.Fatalf("without a mark the view must read the rolled row: a=%d b=%d", timeline(a), timeline(b))
	}
	mark()
	if timeline(a) != 1 || timeline(b) != 1 {
		t.Fatalf("live after move: a=%d b=%d (want 1/1: the dirty hour must replace the rolled row, not add to it)", timeline(a), timeline(b))
	}
	if err := RollupAll(ctx, st, config.Config{}); err != nil {
		t.Fatal(err)
	}
	if timeline(a) != 1 || timeline(b) != 1 {
		t.Fatalf("rolled after move: a=%d b=%d", timeline(a), timeline(b))
	}
	if n := count(t, st, "SELECT count(*) FROM issue_stats_hourly_rolled"); n != 2 {
		t.Fatalf("rolled issue rows = %d, want 2 (one per issue)", n)
	}
}

// TestRollupHistoryExpiry: the rollup tables keep AggregateRetentionDays
// (400), independent of RETENTION_DAYS, and the sweep expires beyond it.
func TestRollupHistoryExpiry(t *testing.T) {
	st := testdb.New(t)
	testdb.Projects(t, st, 1)
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Hour)
	keep, drop := now.Add(-399*24*time.Hour), now.Add(-401*24*time.Hour)
	for _, b := range []time.Time{keep, drop} {
		if _, err := st.Pool.Exec(ctx, `INSERT INTO event_stats_hourly_rolled (bucket, project_id, release, platform, level, events, unhandled, errors) VALUES ($1, 1, '', '', 'error', 1, 0, 1)`, b); err != nil {
			t.Fatal(err)
		}
		if _, err := st.Pool.Exec(ctx, `INSERT INTO issue_stats_hourly_rolled (bucket, project_id, fingerprint, events) VALUES ($1, 1, $2, 1)`, b, sentry.DerivedID([]byte("f"))); err != nil {
			t.Fatal(err)
		}
		if _, err := st.Pool.Exec(ctx, `INSERT INTO release_health_hourly_rolled (bucket, project_id, release, total, crashed, errored) VALUES ($1, 1, '1.0', 1, 0, 0)`, b); err != nil {
			t.Fatal(err)
		}
		if _, err := st.Pool.Exec(ctx, `INSERT INTO client_report_counts (project_id, bucket, reason, category, quantity) VALUES (1, $1, 'sample_rate', 'error', 1)`, b); err != nil {
			t.Fatal(err)
		}
	}
	if err := Sweep(ctx, st, config.Config{RetentionDays: 7}, quiet); err != nil {
		t.Fatal(err)
	}
	for _, tbl := range rolled {
		if n := count(t, st, "SELECT count(*) FROM "+tbl); n != 1 {
			t.Errorf("%s: %d rows after sweep, want 1 (399 days kept, 401 dropped; RETENTION_DAYS=7 must not apply)", tbl, n)
		}
		if n := count(t, st, "SELECT count(*) FROM "+tbl+" WHERE bucket = $1", keep); n != 1 {
			t.Errorf("%s: the 399-day row is gone", tbl)
		}
	}
}

// TestSweepRowExpiries: the row-level expiries of the hourly sweep —
// symbol files at twice the retention, upload chunks after a day, user
// sessions at their expiry, dead jobs after a week (live ones never) —
// each with a row on either side.
func TestSweepRowExpiries(t *testing.T) {
	st := testdb.New(t)
	testdb.Projects(t, st, 1)
	ctx := context.Background()
	cfg := config.Config{RetentionDays: 10}
	now := time.Now().UTC()
	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := st.Pool.Exec(ctx, q, args...); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	d := 24 * time.Hour
	exec(`INSERT INTO symbol_files (project_id, kind, release, filename, size, data, uploaded_at) VALUES (1, 'proguard', '1', 'keep', 1, 'x', $1), (1, 'proguard', '1', 'drop', 1, 'x', $2)`, now.Add(-19*d), now.Add(-21*d))
	exec(`INSERT INTO upload_chunks (sha1, data, created_at) VALUES ('keep', 'x', $1), ('drop', 'x', $2)`, now.Add(-23*time.Hour), now.Add(-25*time.Hour))
	exec(`INSERT INTO users (email, password_hash) VALUES ('u@example.com', 'h')`)
	exec(`INSERT INTO user_sessions (token_hash, user_id, expires_at) VALUES ('keep', 1, $1), ('drop', 1, $2)`, now.Add(time.Hour), now.Add(-time.Hour))
	exec(`INSERT INTO jobs (kind, project_id, args, attempts, created_at) VALUES
		('alert', 1, '{"j":"dead-keep"}', 8, $1), ('alert', 1, '{"j":"dead-drop"}', 8, $2), ('alert', 1, '{"j":"live-old"}', 0, $2)`, now.Add(-6*d), now.Add(-8*d))
	if err := Sweep(ctx, st, cfg, quiet); err != nil {
		t.Fatal(err)
	}
	check := func(what, q string, want int64) {
		t.Helper()
		if n := count(t, st, q); n != want {
			t.Errorf("%s: %d, want %d", what, n, want)
		}
	}
	check("symbol files (19 d kept, 21 d dropped at 2×10 d)", "SELECT count(*) FROM symbol_files", 1)
	check("symbol file kept is the young one", "SELECT count(*) FROM symbol_files WHERE filename = 'keep'", 1)
	check("upload chunks (23 h kept, 25 h dropped)", "SELECT count(*) FROM upload_chunks", 1)
	check("upload chunk kept is the young one", "SELECT count(*) FROM upload_chunks WHERE sha1 = 'keep'", 1)
	check("user sessions (expired dropped)", "SELECT count(*) FROM user_sessions", 1)
	check("user session kept is the live one", "SELECT count(*) FROM user_sessions WHERE token_hash = 'keep'", 1)
	check("jobs (dead 6 d kept, dead 8 d dropped, live 8 d kept)", "SELECT count(*) FROM jobs", 2)
	check("dead job dropped", "SELECT count(*) FROM jobs WHERE args->>'j' = 'dead-drop'", 0)
	check("live job kept whatever its age", "SELECT count(*) FROM jobs WHERE args->>'j' = 'live-old'", 1)
}

// TestExpireIssuesFollowsPartitions: the cutoff ExpireIssues uses for
// resolved / ignored issues is exactly the start of the oldest partition
// that survives the same sweep — so an ignored issue whose last event
// still lists is kept, and one whose partition was dropped goes.
func TestExpireIssuesFollowsPartitions(t *testing.T) {
	st := testdb.New(t)
	testdb.Projects(t, st, 1)
	ctx := context.Background()
	cfg := config.Config{RetentionDays: 30}
	now := time.Date(2026, 8, 30, 12, 0, 0, 0, time.UTC)
	if err := EnsurePartitions(ctx, st, cfg, now.Add(-6*PartitionWidth)); err != nil { // old partitions exist
		t.Fatal(err)
	}
	if err := EnsurePartitions(ctx, st, cfg, now); err != nil {
		t.Fatal(err)
	}
	if err := dropExpiredPartitions(ctx, st, cfg, now, quiet); err != nil {
		t.Fatal(err)
	}
	parts, err := Partitions(ctx, st, "events")
	if err != nil || len(parts) == 0 {
		t.Fatal(parts, err)
	}
	oldest := parts[0].Start
	add := func(name, status string, lastSeen time.Time) sentry.ID {
		fp := sentry.DerivedID([]byte(name))
		if _, err := st.Pool.Exec(ctx, `INSERT INTO issues (project_id, fingerprint, title, level, status, first_seen, last_seen)
			VALUES (1, $1, $2, 'error', $3, $4, $4)`, fp, name, status, lastSeen); err != nil {
			t.Fatal(err)
		}
		return fp
	}
	kept := add("ignored-in-oldest-partition", "ignored", oldest.Add(time.Minute))
	gone := add("ignored-before-oldest-partition", "ignored", oldest.Add(-time.Minute))
	open := add("unresolved-before-oldest-partition", "unresolved", oldest.Add(-time.Minute))
	if _, err := ExpireIssues(ctx, st, cfg, now); err != nil {
		t.Fatal(err)
	}
	has := func(fp sentry.ID) bool {
		return count(t, st, "SELECT count(*) FROM issues WHERE fingerprint = $1", fp) == 1
	}
	if !has(kept) {
		t.Error("an ignored issue whose last event's partition survives must be kept")
	}
	if has(gone) {
		t.Error("an ignored issue last seen before the oldest partition must be deleted")
	}
	if !has(open) {
		t.Error("an unresolved issue is not deleted at the partition boundary")
	}
}
