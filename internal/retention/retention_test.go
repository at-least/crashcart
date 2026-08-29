package retention

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/crashcartapp/crashcart/internal/blob"
	"github.com/crashcartapp/crashcart/internal/config"
	"github.com/crashcartapp/crashcart/internal/db/sqlc"
	"github.com/crashcartapp/crashcart/internal/ingest"
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

func TestPackPayloads(t *testing.T) {
	st := testdb.New(t)
	ctx := context.Background()
	p, err := st.CreateProject(ctx, sqlc.CreateProjectParams{Slug: "app", Name: "App", PublicKey: "k"})
	if err != nil {
		t.Fatal(err)
	}
	in := &ingest.Ingester{Store: st, Cfg: config.Config{}, Log: slog.Default()}
	now := time.Now().UTC()
	event := func(id string) []byte {
		return []byte("{}\n" + `{"type":"event"}` + "\n" + `{"event_id":"` + id + `","level":"error","message":"boom ` + id + `","timestamp":"` + now.Format(time.RFC3339) + `"}` + "\n")
	}
	get := func(id string) sqlc.Event {
		t.Helper()
		ev, err := st.GetEvent(ctx, sqlc.GetEventParams{ProjectID: p.ID, EventID: sentry.ID(id)})
		if err != nil {
			t.Fatal(err)
		}
		return ev
	}
	ref := func(id string) (key string, off int64) {
		t.Helper()
		ev := get(id)
		if ev.PayloadRef == nil {
			t.Fatalf("%s has no payload_ref", id)
		}
		key, off, _, ok := blob.ParseRef(*ev.PayloadRef)
		if !ok {
			t.Fatalf("bad ref %q", *ev.PayloadRef)
		}
		return key, off
	}
	const a, b, c = "a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1", "b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2", "c3c3c3c3c3c3c3c3c3c3c3c3c3c3c3c3"
	if _, err := in.Ingest(ctx, p, sentry.Parse(event(a), now), now); err != nil {
		t.Fatal(err)
	}
	// The row carries its ref from the start, at offset 0 of a fresh pack;
	// the payload is read from the spool while the pack is open.
	keyA, offA := ref(a)
	if offA != 0 || !strings.HasPrefix(keyA, blob.PrefixEvents) {
		t.Fatalf("first ref = %s#%d", keyA, offA)
	}
	if bs, err := st.Payload(ctx, get(a)); err != nil || !strings.Contains(string(bs), `"message":"boom a1`) {
		t.Fatalf("payload from spool: %q %v", bs, err)
	}
	// An open pack is not uploaded.
	if n, err := PackPayloads(ctx, st); err != nil || n != 0 {
		t.Fatalf("packed the open pack: %d %v", n, err)
	}
	// A rolled-back envelope returns its bytes: the next one continues
	// right after the first, in the same pack.
	err = st.Tx(ctx, func(ctx context.Context, tx pgx.Tx, q *sqlc.Queries) error {
		if _, err := store.SpoolPayloads(ctx, q, [][]byte{make([]byte, 500)}); err != nil {
			return err
		}
		return errors.New("quota")
	})
	if err == nil {
		t.Fatal("rollback expected")
	}
	if _, err := in.Ingest(ctx, p, sentry.Parse(event(b), now), now); err != nil {
		t.Fatal(err)
	}
	gzA := blob.Gzip(sentry.Parse(event(a), now).Events[0].Raw)
	if keyB, offB := ref(b); keyB != keyA || offB != int64(len(gzA)) {
		t.Fatalf("second ref = %s#%d, want %s#%d", keyB, offB, keyA, len(gzA))
	}
	// Two envelopes at once take two different packs (the row lock is held
	// to commit), and both refs stay valid.
	var otherKey string
	err = st.Tx(ctx, func(ctx context.Context, tx pgx.Tx, q *sqlc.Queries) error {
		refs, err := store.SpoolPayloads(ctx, q, [][]byte{blob.Gzip([]byte("{}"))})
		if err != nil {
			return err
		}
		otherKey, _, _, _ = blob.ParseRef(string(refs[0]))
		// While this holds pack A (or B), another transaction goes elsewhere.
		if _, err := in.Ingest(ctx, p, sentry.Parse(event(c), now), now); err != nil {
			return err
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if keyC, _ := ref(c); keyC == otherKey {
		t.Fatalf("concurrent envelopes shared pack %s", keyC)
	}
	// Closing (the CLI's PackAll) uploads every pack: spool empty, refs
	// unchanged, payloads now come from the objects.
	if err := PackAll(ctx, st); err != nil {
		t.Fatal(err)
	}
	if n, _ := st.CountSpool(ctx); n != 0 {
		t.Fatalf("spool rows after packing = %d", n)
	}
	if keys := st.Blobs.(*blob.Memory).Keys(blob.PrefixEvents); len(keys) != 2 {
		t.Fatalf("pack objects = %v", keys)
	}
	for _, id := range []string{a, b, c} {
		if bs, err := st.Payload(ctx, get(id)); err != nil || !strings.Contains(string(bs), `"message":"boom `+id[:2]) {
			t.Fatalf("payload %s from pack: %q %v", id, bs, err)
		}
	}
	if n, err := PackPayloads(ctx, st); err != nil || n != 0 {
		t.Fatalf("nothing left to pack, got %d %v", n, err)
	}

	// A failing object store leaves the spool, the packs and the refs
	// intact, and the payload readable.
	st2 := testdb.New(t)
	p2, _ := st2.CreateProject(ctx, sqlc.CreateProjectParams{Slug: "app", Name: "App", PublicKey: "k"})
	in2 := &ingest.Ingester{Store: st2, Cfg: config.Config{}, Log: slog.Default()}
	if _, err := in2.Ingest(ctx, p2, sentry.Parse(event(a), now), now); err != nil {
		t.Fatal(err)
	}
	st2.Blobs = failingStore{blob.NewMemory()}
	if err := PackAll(ctx, st2); err == nil {
		t.Fatal("pack with a failing store succeeded")
	}
	if n, _ := st2.CountSpool(ctx); n != 1 {
		t.Fatalf("spool rows after failed pack = %d", n)
	}
	ev2, _ := st2.GetEvent(ctx, sqlc.GetEventParams{ProjectID: p2.ID, EventID: a})
	if bs, err := st2.Payload(ctx, ev2); err != nil || !strings.Contains(string(bs), "boom a1") {
		t.Fatalf("payload still readable from spool: %q %v", bs, err)
	}
}

type failingStore struct{ *blob.Memory }

func (failingStore) PutRaw(context.Context, string, []byte) error { return errors.New("bucket down") }
