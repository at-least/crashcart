package store_test

import (
	"bytes"
	"context"
	"crypto/rand"
	"errors"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/at-least/crashcart/internal/blob"
	"github.com/at-least/crashcart/internal/config"
	"github.com/at-least/crashcart/internal/db/sqlc"
	"github.com/at-least/crashcart/internal/retention"
	"github.com/at-least/crashcart/internal/sentry"
	"github.com/at-least/crashcart/internal/store"
	"github.com/at-least/crashcart/internal/testdb"
)

// insertPayloads writes events with the given raw payloads at t (one
// minute apart) through st.InsertEvents, the way ingest does.
func insertPayloads(t *testing.T, st *store.Store, pid int64, at time.Time, raws ...[]byte) []sqlc.Event {
	t.Helper()
	ctx := context.Background()
	var rows []store.EventInsert
	for i, raw := range raws {
		rows = append(rows, store.EventInsert{
			OccurredAt: at.Add(time.Duration(i) * time.Minute), ProjectID: pid, EventID: sentry.DerivedID(raw),
			Level: "error", Message: "m", Tags: []byte("{}"), Payload: store.Gzip(raw),
		})
	}
	if err := pgx.BeginFunc(ctx, st.Pool, func(tx pgx.Tx) error { return st.InsertEvents(ctx, tx, rows) }); err != nil {
		t.Fatal(err)
	}
	var out []sqlc.Event
	for _, r := range rows {
		e, err := st.GetEventAt(ctx, sqlc.GetEventAtParams{ProjectID: pid, EventID: r.EventID, OccurredAt: r.OccurredAt})
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, e)
	}
	return out
}

func spoolCount(t *testing.T, st *store.Store) int64 {
	t.Helper()
	n, err := st.SpoolCount(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return n
}

func payloadOf(t *testing.T, st *store.Store, e sqlc.Event) []byte {
	t.Helper()
	b, err := st.Payload(context.Background(), nil, e)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// randomPayload is n bytes gzip cannot shrink (InsertEvents stores what it
// is given; nothing parses it here), so a pack's size is what the test
// says it is.
func randomPayload(n int) []byte {
	b := make([]byte, n)
	rand.Read(b)
	return b
}

// TestPacksSpoolAndPack: with a store, ingest spools; Payload serves the
// spool; Pack moves the spool into an object and Payload reads the range;
// a resend spools nothing; the age rule packs a lone event.
func TestPacksSpoolAndPack(t *testing.T) {
	st := testdb.New(t)
	testdb.Projects(t, st, 1)
	mem := &blob.Memory{}
	st.Blobs = mem
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Second)

	raws := [][]byte{[]byte(`{"event_id":"a","message":"one"}`), []byte(`{"event_id":"b","message":"two"}`)}
	evs := insertPayloads(t, st, 1, now.Add(-2*time.Hour), raws...)
	for i, e := range evs {
		if e.Payload != nil {
			t.Fatalf("event %d: payload column written although a store is configured", i)
		}
		if got := payloadOf(t, st, e); !bytes.Equal(got, raws[i]) {
			t.Fatalf("event %d from the spool: %q", i, got)
		}
	}
	if n := spoolCount(t, st); n != 2 {
		t.Fatalf("spool rows = %d", n)
	}
	// A resent envelope: the row conflicts, nothing is spooled again.
	insertPayloads(t, st, 1, now.Add(-2*time.Hour), raws[0])
	if n := spoolCount(t, st); n != 2 {
		t.Fatalf("spool rows after a resend = %d", n)
	}
	// Too young and too small: nothing packs yet.
	if n, err := st.Pack(ctx, now); err != nil || n != 0 {
		t.Fatalf("early pack: %d %v", n, err)
	}
	// Old enough: one pack for the (project, week), both events in it.
	n, err := st.Pack(ctx, now.Add(store.PackAge+time.Second))
	if err != nil || n != 2 {
		t.Fatalf("pack: %d %v", n, err)
	}
	if n := spoolCount(t, st); n != 0 {
		t.Fatalf("spool rows after pack = %d", n)
	}
	keys := mem.Keys()
	if len(keys) != 1 || !strings.HasPrefix(keys[0], "events/1/") {
		t.Fatalf("objects: %v", keys)
	}
	for i, e := range evs {
		if got := payloadOf(t, st, e); !bytes.Equal(got, raws[i]) {
			t.Fatalf("event %d from the pack: %q", i, got)
		}
	}
	var packed int64
	if err := st.Pool.QueryRow(ctx, "SELECT count(*) FROM event_packs").Scan(&packed); err != nil || packed != 2 {
		t.Fatalf("event_packs rows = %d %v", packed, err)
	}
	// Idempotent: nothing left to pack.
	if n, err := st.Pack(ctx, now.Add(time.Hour)); err != nil || n != 0 {
		t.Fatalf("second pack: %d %v", n, err)
	}
	// Without a store the packed row is an error, not a silent nil.
	st.Blobs = nil
	if _, err := st.Payload(ctx, nil, evs[0]); err == nil || !strings.Contains(err.Error(), "BLOB_STORE") {
		t.Fatalf("packed row without a store: %v", err)
	}
	st.Blobs = mem
	// A row with a payload column (written before the store) still reads
	// from the column: mixed state.
	old := store.EventInsert{OccurredAt: now.Add(-3 * time.Hour), ProjectID: 1, EventID: sentry.DerivedID([]byte("old")), Level: "error", Message: "m", Tags: []byte("{}"), Payload: store.Gzip([]byte(`{"old":true}`))}
	st.Blobs = nil
	if err := pgx.BeginFunc(ctx, st.Pool, func(tx pgx.Tx) error { return st.InsertEvents(ctx, tx, []store.EventInsert{old}) }); err != nil {
		t.Fatal(err)
	}
	st.Blobs = mem
	e, _ := st.GetEventAt(ctx, sqlc.GetEventAtParams{ProjectID: 1, EventID: old.EventID, OccurredAt: old.OccurredAt})
	if got := payloadOf(t, st, e); string(got) != `{"old":true}` {
		t.Fatalf("column row: %q", got)
	}
}

// TestPacksSizeAndProjects: a pack closes at PackBytes; an oversized
// payload is a pack of its own; projects and weeks never share a pack.
func TestPacksSizeAndProjects(t *testing.T) {
	st := testdb.New(t)
	testdb.Projects(t, st, 1, 2)
	mem := &blob.Memory{}
	st.Blobs = mem
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Second)
	// Three 3.5 MB payloads: the first two fit one pack, the third opens
	// another; a 9 MB one is a pack of its own; project 2 and last week
	// are their own.
	insertPayloads(t, st, 1, now.Add(-time.Hour), randomPayload(3_500_000), randomPayload(3_500_000), randomPayload(3_500_000))
	insertPayloads(t, st, 1, now.Add(-30*time.Minute), randomPayload(9_000_000)) // > PackBytes
	insertPayloads(t, st, 2, now.Add(-time.Hour), []byte(`{"p":2}`))
	insertPayloads(t, st, 1, now.Add(-14*24*time.Hour), []byte(`{"lastweek":true}`))
	n, err := st.Drain(ctx)
	if err != nil || n != 6 {
		t.Fatalf("drain: %d %v", n, err)
	}
	keys := mem.Keys()
	if len(keys) != 5 {
		t.Fatalf("packs: %v", keys)
	}
	byPrefix := map[string]int{}
	for _, k := range keys {
		byPrefix[k[:strings.LastIndex(k, "/")]]++
	}
	// project 1 this week: 3 packs (2 events, 1 event, the oversized one)
	if len(byPrefix) != 3 {
		t.Fatalf("pack prefixes (project/week): %v", byPrefix)
	}
	for prefix, n := range byPrefix {
		switch {
		case strings.HasPrefix(prefix, "events/2/") && n != 1:
			t.Errorf("project 2: %d packs", n)
		case strings.HasPrefix(prefix, "events/1/") && n != 1 && n != 3:
			t.Errorf("project 1 %s: %d packs", prefix, n)
		}
	}
	// Every payload reads back through its range.
	rows, _ := st.Pool.Query(ctx, "SELECT project_id, event_id, occurred_at FROM events")
	var evs []sqlc.Event
	for rows.Next() {
		var e sqlc.Event
		rows.Scan(&e.ProjectID, &e.EventID, &e.OccurredAt)
		evs = append(evs, e)
	}
	rows.Close()
	if len(evs) != 6 {
		t.Fatalf("events = %d", len(evs))
	}
	reader := st.NewPackReader()
	for _, e := range evs {
		b, err := reader.Payload(ctx, nil, e)
		if err != nil || len(b) == 0 {
			t.Fatalf("payload of %s: %d bytes %v", e.EventID, len(b), err)
		}
	}
	if gets := mem.Gets(); gets != 5 {
		t.Fatalf("pack reader made %d gets for 5 packs", gets)
	}
}

// failingPut is a Memory whose Put fails until cleared.
type failingPut struct {
	blob.Memory
	err error
}

func (f *failingPut) Put(ctx context.Context, key string, data []byte) error {
	if f.err != nil {
		return f.err
	}
	return f.Memory.Put(ctx, key, data)
}

// TestPacksFailures: a store that refuses the object leaves the spool
// intact and readable; a flush that fails after the object was written
// leaves no dangling reference and the next run repacks.
func TestPacksFailures(t *testing.T) {
	st := testdb.New(t)
	testdb.Projects(t, st, 1)
	f := &failingPut{err: errors.New("bucket down")}
	st.Blobs = f
	ctx := context.Background()
	now := time.Now().UTC()
	raw := []byte(`{"still":"served"}`)
	evs := insertPayloads(t, st, 1, now.Add(-time.Hour), raw)
	if n, err := st.Drain(ctx); err == nil || n != 0 || !strings.Contains(err.Error(), "bucket down") {
		t.Fatalf("drain with the store down: %d %v", n, err)
	}
	if n := spoolCount(t, st); n != 1 {
		t.Fatalf("spool after a failed put = %d", n)
	}
	if got := payloadOf(t, st, evs[0]); !bytes.Equal(got, raw) {
		t.Fatalf("payload while the store is down: %q", got)
	}
	var packs int64
	if err := st.Pool.QueryRow(ctx, "SELECT count(*) FROM packs").Scan(&packs); err != nil || packs != 0 {
		t.Fatalf("packs rows after a failed put = %d (the row must be removed again)", packs)
	}
	f.err = nil
	if n, err := st.Drain(ctx); err != nil || n != 1 {
		t.Fatalf("drain once the store is back: %d %v", n, err)
	}
	if got := payloadOf(t, st, evs[0]); !bytes.Equal(got, raw) {
		t.Fatalf("payload after packing: %q", got)
	}
}

// TestPacksRetentionAndProjectDelete: a week's packs go with its
// partition; a project's packs go with the project.
func TestPacksRetentionAndProjectDelete(t *testing.T) {
	st := testdb.New(t)
	testdb.Projects(t, st, 1, 2)
	mem := &blob.Memory{}
	st.Blobs = mem
	ctx := context.Background()
	cfg := config.Config{RetentionDays: 7}
	now := time.Now().UTC()
	quiet := slog.New(slog.NewTextHandler(&bytes.Buffer{}, nil))
	if err := retention.EnsurePartitions(ctx, st, cfg, now); err != nil {
		t.Fatal(err)
	}
	// Two weeks ago is past a 7-day retention; this week is not.
	insertPayloads(t, st, 1, now.Add(-15*24*time.Hour), []byte(`{"expired":1}`))
	insertPayloads(t, st, 1, now.Add(-time.Hour), []byte(`{"live":1}`))
	insertPayloads(t, st, 2, now.Add(-time.Hour), []byte(`{"live":2}`))
	if _, err := st.Drain(ctx); err != nil {
		t.Fatal(err)
	}
	if len(mem.Keys()) != 3 {
		t.Fatalf("packs before the sweep: %v", mem.Keys())
	}
	if err := retention.Sweep(ctx, st, cfg, quiet); err != nil {
		t.Fatal(err)
	}
	keys := mem.Keys()
	if len(keys) != 2 {
		t.Fatalf("packs after the sweep: %v", keys)
	}
	var left int64
	if err := st.Pool.QueryRow(ctx, "SELECT count(*) FROM packs").Scan(&left); err != nil || left != 2 {
		t.Fatalf("packs rows after the sweep = %d", left)
	}
	// Project delete: keys read first, then the project, then the objects.
	packs, err := st.ProjectPacks(ctx, 1)
	if err != nil || len(packs) != 1 {
		t.Fatalf("project 1 packs: %v %v", packs, err)
	}
	if err := st.DeleteProject(ctx, 1); err != nil {
		t.Fatal(err)
	}
	if err := st.DeleteProjectPacks(ctx, 1, packs); err != nil {
		t.Fatal(err)
	}
	if keys := mem.Keys(); len(keys) != 1 || !strings.HasPrefix(keys[0], "events/2/") {
		t.Fatalf("after project delete: %v", keys)
	}
}
