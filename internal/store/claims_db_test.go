package store_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/at-least/crashcart/internal/db/sqlc"
	"github.com/at-least/crashcart/internal/sentry"
	"github.com/at-least/crashcart/internal/store"
	"github.com/at-least/crashcart/internal/testdb"
)

// Tests of the documented store claims (ARCHITECTURE.md / CLAUDE.md),
// each written so that it fails if the claim were false.

func insertEvents(t *testing.T, st *store.Store, rows []store.EventInsert) {
	t.Helper()
	ctx := context.Background()
	tx, err := st.Pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.InsertEvents(ctx, tx, rows); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
}

// TestCursorPagingIdenticalTimestamps: keyset paging on (occurred_at,
// event_id) returns every row exactly once, in the list's own order, even
// when several rows share a timestamp — and the cursor survives its URL
// encoding, while garbage is refused.
func TestCursorPagingIdenticalTimestamps(t *testing.T) {
	st := testdb.New(t)
	testdb.Projects(t, st, 1)
	ctx := context.Background()
	at := time.Date(2026, 8, 30, 10, 0, 0, 123456000, time.UTC)
	var rows []store.EventInsert
	for i := 0; i < 5; i++ { // five rows at the same instant
		rows = append(rows, store.EventInsert{OccurredAt: at, ProjectID: 1, EventID: sentry.DerivedID([]byte(fmt.Sprint("same", i))), Level: "error", Message: "m", Tags: []byte("{}")})
	}
	rows = append(rows, store.EventInsert{OccurredAt: at.Add(time.Second), ProjectID: 1, EventID: sentry.DerivedID([]byte("newer")), Level: "error", Message: "m", Tags: []byte("{}")})
	rows = append(rows, store.EventInsert{OccurredAt: at.Add(-time.Second), ProjectID: 1, EventID: sentry.DerivedID([]byte("older")), Level: "error", Message: "m", Tags: []byte("{}")})
	insertEvents(t, st, rows)

	all, more, err := st.ListEvents(ctx, store.EventFilter{ProjectID: 1, From: at.Add(-time.Hour), To: at.Add(time.Hour)})
	if err != nil || more || len(all) != 7 {
		t.Fatalf("all: %d more=%v err=%v", len(all), more, err)
	}
	if all[0].EventID != sentry.DerivedID([]byte("newer")) || all[6].EventID != sentry.DerivedID([]byte("older")) {
		t.Fatalf("newest first: %v … %v", all[0].EventID, all[6].EventID)
	}
	for i := 1; i < 6; i++ { // ties break on event_id DESC
		if !(all[i].EventID < all[i-1].EventID) && all[i].OccurredAt.Equal(all[i-1].OccurredAt) {
			t.Fatalf("tie order at %d: %v after %v", i, all[i].EventID, all[i-1].EventID)
		}
	}
	var paged []sentry.ID
	var cursor store.Cursor
	for pages := 0; ; pages++ {
		f := store.EventFilter{ProjectID: 1, From: at.Add(-time.Hour), To: at.Add(time.Hour), Limit: 2}
		if !cursor.IsZero() {
			c, ok := store.ParseCursor(cursor.String()) // through the URL form
			if !ok || !c.At.Equal(cursor.At) || c.EventID != cursor.EventID {
				t.Fatalf("cursor round trip: %q → %+v ok=%v", cursor.String(), c, ok)
			}
			f.Before = c
		}
		page, more, err := st.ListEvents(ctx, f)
		if err != nil {
			t.Fatal(err)
		}
		for _, r := range page {
			paged = append(paged, r.EventID)
		}
		if !more {
			if pages != 3 || len(page) != 1 {
				t.Fatalf("pages=%d last page=%d rows (want 4 pages of 2,2,2,1)", pages+1, len(page))
			}
			break
		}
		if len(page) != 2 {
			t.Fatalf("page %d has %d rows", pages, len(page))
		}
		cursor = store.CursorOf(page[len(page)-1])
	}
	if len(paged) != 7 {
		t.Fatalf("paging returned %d rows, want 7 (each exactly once)", len(paged))
	}
	for i, r := range all {
		if paged[i] != r.EventID {
			t.Fatalf("paged order differs from the list at %d: %v vs %v", i, paged[i], r.EventID)
		}
	}
	for _, bad := range []string{"garbage", "2026-08-30T10:00:00Z_", "_" + string(all[0].EventID), "notatime_" + string(all[0].EventID), "2026-08-30T10:00:00Z_zz"} {
		if _, ok := store.ParseCursor(bad); ok {
			t.Errorf("ParseCursor(%q) accepted", bad)
		}
	}
	if c, ok := store.ParseCursor(""); !ok || !c.IsZero() {
		t.Error("the empty cursor is the zero cursor")
	}
}

// TestCountEventsMatchesListUnderFilters: the count and the list share the
// WHERE for the filters TestCountEventsAndSearch does not cover — tags
// (containment), release, environment, fingerprint — and the window is
// [from, to): a row at `from` counts, a row at `to` does not.
func TestCountEventsMatchesListUnderFilters(t *testing.T) {
	st := testdb.New(t)
	testdb.Projects(t, st, 1)
	ctx := context.Background()
	from := time.Date(2026, 8, 30, 10, 0, 0, 0, time.UTC)
	to := from.Add(time.Hour)
	fp := sentry.DerivedID([]byte("fp"))
	mk := func(name string, at time.Time, rel, env string, tags map[string]string, withFP bool) store.EventInsert {
		tj, _ := json.Marshal(tags)
		e := store.EventInsert{OccurredAt: at, ProjectID: 1, EventID: sentry.DerivedID([]byte(name)), Level: "error", Message: name, Release: &rel, Environment: &env, Tags: tj}
		if withFP {
			e.Fingerprint = &fp
		}
		return e
	}
	insertEvents(t, st, []store.EventInsert{
		mk("at-from", from, "1.0", "prod", map[string]string{"build": "42", "x": "y"}, true),
		mk("mid", from.Add(30*time.Minute), "1.0", "staging", map[string]string{"build": "43"}, false),
		mk("mid2", from.Add(31*time.Minute), "2.0", "prod", map[string]string{"build": "42"}, true),
		mk("at-to", to, "1.0", "prod", map[string]string{"build": "42"}, true),
		mk("before", from.Add(-time.Nanosecond*1000), "1.0", "prod", map[string]string{"build": "42"}, true),
	})
	check := func(name string, f store.EventFilter, want int) {
		t.Helper()
		n, err := st.CountEvents(ctx, f)
		if err != nil {
			t.Fatal(err)
		}
		list, _, err := st.ListEvents(ctx, f)
		if err != nil {
			t.Fatal(err)
		}
		if int(n) != want || len(list) != want {
			t.Errorf("%s: count=%d list=%d want %d", name, n, len(list), want)
		}
	}
	base := store.EventFilter{ProjectID: 1, From: from, To: to}
	check("window [from,to)", base, 3)
	tagged := base
	tagged.Tags = map[string]string{"build": "42"}
	check("tag build=42", tagged, 2)
	two := base
	two.Tags = map[string]string{"build": "42", "x": "y"}
	check("two tags (both must match)", two, 1)
	partial := base
	partial.Tags = map[string]string{"build": "4"}
	check("tag value is exact, not a prefix", partial, 0)
	rel := base
	rel.Release = "1.0"
	check("release", rel, 2)
	env := base
	env.Environment = "prod"
	check("environment", env, 2)
	fpf := base
	fpf.Fingerprint = fp
	check("fingerprint", fpf, 2)
	both := base
	both.Release, both.Tags, both.Environment = "1.0", map[string]string{"build": "42"}, "prod"
	check("release + tag + environment", both, 1)
	open := store.EventFilter{ProjectID: 1, From: from.Add(-time.Hour)}
	check("no upper bound", open, 5)
}

// TestPayloadRoundTrip: the gzipped payload written at ingest is read
// back byte for byte through store.Payload, and a row stored without one
// (an import) yields nil, nil — not an error and not empty bytes.
func TestPayloadRoundTrip(t *testing.T) {
	st := testdb.New(t)
	testdb.Projects(t, st, 1)
	ctx := context.Background()
	raw := []byte(`{"event_id":"abc","message":"héllo","big":"` + string(bytes.Repeat([]byte("x"), 10000)) + `"}`)
	gz := store.Gzip(raw)
	if len(gz) >= len(raw) {
		t.Fatalf("Gzip did not compress: %d → %d bytes", len(raw), len(gz))
	}
	now := time.Now().UTC()
	insertEvents(t, st, []store.EventInsert{
		{OccurredAt: now, ProjectID: 1, EventID: sentry.DerivedID([]byte("with")), Level: "error", Message: "m", Tags: []byte("{}"), Payload: gz},
		{OccurredAt: now, ProjectID: 1, EventID: sentry.DerivedID([]byte("without")), Level: "error", Message: "m", Tags: []byte("{}")},
	})
	e, err := st.GetEvent(ctx, sqlc.GetEventParams{ProjectID: 1, EventID: sentry.DerivedID([]byte("with"))})
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(e.Payload, gz) {
		t.Fatal("the stored bytes are not what Gzip produced (the column must hold the gzip as is)")
	}
	got, err := store.Payload(e)
	if err != nil || !bytes.Equal(got, raw) {
		t.Fatalf("Payload: %v, equal=%v", err, bytes.Equal(got, raw))
	}
	e, err = st.GetEvent(ctx, sqlc.GetEventParams{ProjectID: 1, EventID: sentry.DerivedID([]byte("without"))})
	if err != nil {
		t.Fatal(err)
	}
	if got, err := store.Payload(e); err != nil || got != nil {
		t.Fatalf("Payload of a row without one = %v, %v (want nil, nil)", got, err)
	}
	if _, err := store.Gunzip([]byte("not gzip")); err == nil {
		t.Error("Gunzip must fail on bytes that are not gzip")
	}
}

// TestInsertEventsDedupesAndMarksDirty: a resent event (same key) is a
// no-op, and every hour a batch touches — and only those — is marked
// dirty for its project in the same transaction.
func TestInsertEventsDedupesAndMarksDirty(t *testing.T) {
	st := testdb.New(t)
	testdb.Projects(t, st, 1, 2)
	ctx := context.Background()
	h := time.Date(2026, 8, 30, 10, 0, 0, 0, time.UTC)
	rows := []store.EventInsert{
		{OccurredAt: h.Add(5 * time.Minute), ProjectID: 1, EventID: sentry.DerivedID([]byte("a")), Level: "error", Message: "first", Tags: []byte("{}")},
		{OccurredAt: h.Add(70 * time.Minute), ProjectID: 1, EventID: sentry.DerivedID([]byte("b")), Level: "error", Message: "m", Tags: []byte("{}")},
		{OccurredAt: h.Add(5 * time.Minute), ProjectID: 2, EventID: sentry.DerivedID([]byte("c")), Level: "error", Message: "m", Tags: []byte("{}")},
	}
	insertEvents(t, st, rows)
	type key struct {
		pid int64
		b   time.Time
	}
	dirty := func() map[key]int64 {
		r, err := st.Pool.Query(ctx, "SELECT project_id, bucket, gen FROM event_stats_dirty")
		if err != nil {
			t.Fatal(err)
		}
		defer r.Close()
		out := map[key]int64{}
		for r.Next() {
			var k key
			var g int64
			if err := r.Scan(&k.pid, &k.b, &g); err != nil {
				t.Fatal(err)
			}
			out[key{k.pid, k.b.UTC()}] = g
		}
		return out
	}
	got := dirty()
	want := map[key]int64{{1, h}: 1, {1, h.Add(time.Hour)}: 1, {2, h}: 1}
	if len(got) != len(want) {
		t.Fatalf("dirty keys = %v, want %v", got, want)
	}
	for k, g := range want {
		if got[k] != g {
			t.Fatalf("dirty %v gen=%d, want %d (all: %v)", k, got[k], g, got)
		}
	}
	// Resend of "a" with another message: the row is unchanged, the hour
	// is marked again (gen bumps), nothing else changes.
	rows[0].Message = "resent"
	insertEvents(t, st, rows[:1])
	var msg string
	if err := st.Pool.QueryRow(ctx, "SELECT message FROM events WHERE event_id = $1", sentry.DerivedID([]byte("a"))).Scan(&msg); err != nil || msg != "first" {
		t.Fatalf("resent row message = %q %v (want the first write kept: ON CONFLICT DO NOTHING)", msg, err)
	}
	if n := dirty(); n[key{1, h}] != 2 || n[key{1, h.Add(time.Hour)}] != 1 || n[key{2, h}] != 1 {
		t.Fatalf("dirty after resend = %v", n)
	}
	// Sessions: a terminal status is never downgraded to ok; ok → crashed
	// is applied; both mark the session hour.
	ins := func(status string) {
		t.Helper()
		tx, _ := st.Pool.Begin(ctx)
		if err := store.InsertSessions(ctx, tx, []store.SessionInsert{{StartedAt: h.Add(time.Minute), ProjectID: 1, Sid: "s", Release: "1.0", Status: status, Count: 1}}); err != nil {
			t.Fatal(err)
		}
		if err := tx.Commit(ctx); err != nil {
			t.Fatal(err)
		}
	}
	status := func() string {
		var s string
		if err := st.Pool.QueryRow(ctx, "SELECT status::text FROM sessions WHERE sid = 's'").Scan(&s); err != nil {
			t.Fatal(err)
		}
		return s
	}
	ins("ok")
	ins("crashed")
	if s := status(); s != "crashed" {
		t.Fatalf("ok → crashed: %s", s)
	}
	ins("ok")
	if s := status(); s != "crashed" {
		t.Fatalf("crashed → ok must not downgrade: %s", s)
	}
	ins("exited")
	if s := status(); s != "exited" {
		t.Fatalf("crashed → exited (another terminal status) is applied: %s", s)
	}
	var sgen int64
	if err := st.Pool.QueryRow(ctx, "SELECT gen FROM session_stats_dirty WHERE project_id = 1 AND bucket = $1", h).Scan(&sgen); err != nil || sgen != 4 {
		t.Fatalf("session hour gen = %d %v, want 4 (one per write)", sgen, err)
	}
}

// TestBucketMatchesGoTruncate: crashcart_bucket agrees with Go's
// t.Truncate(width) for the widths the charts use (1 h, 4 h, 6 h, 1 d —
// web.Window, the issue sparkline, the API), so buckets computed on
// either side line up. Only widths that divide a day: crashcart_bucket
// is Unix-epoch-aligned (a 7-day bucket starts on a Thursday) and Go's
// Truncate counts from year 1 (a Monday) — a wider bucket would need
// its own alignment on both sides.
func TestBucketMatchesGoTruncate(t *testing.T) {
	st := testdb.New(t)
	ctx := context.Background()
	ts := []time.Time{
		time.Date(2026, 8, 30, 13, 47, 12, 0, time.UTC),
		time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
		time.Date(2025, 12, 31, 23, 59, 59, 0, time.UTC),
		time.Date(2026, 3, 29, 2, 30, 0, 0, time.UTC), // a DST switch in Europe: irrelevant in UTC
	}
	for _, w := range []time.Duration{time.Hour, 4 * time.Hour, 6 * time.Hour, 24 * time.Hour} {
		for _, at := range ts {
			var got time.Time
			if err := st.Pool.QueryRow(ctx, "SELECT crashcart_bucket($1, $2)", at, int64(w/time.Second)).Scan(&got); err != nil {
				t.Fatal(err)
			}
			if want := at.Truncate(w); !got.Equal(want) {
				t.Errorf("crashcart_bucket(%v, %v) = %v, Go Truncate = %v", at, w, got.UTC(), want)
			}
		}
	}
	// crashcart_buckets yields exactly the Truncate-aligned starts of the
	// buckets that intersect [from, to), from's own bucket included.
	from, to := time.Date(2026, 8, 30, 13, 0, 0, 0, time.UTC), time.Date(2026, 8, 31, 1, 0, 0, 0, time.UTC)
	rows, err := st.Pool.Query(ctx, "SELECT b FROM crashcart_buckets($1, $2, $3) b", from.Truncate(4*time.Hour), to, 4*3600)
	if err != nil {
		t.Fatal(err)
	}
	var got []time.Time
	for rows.Next() {
		var b time.Time
		rows.Scan(&b)
		got = append(got, b.UTC())
	}
	rows.Close()
	want := []time.Time{time.Date(2026, 8, 30, 12, 0, 0, 0, time.UTC), time.Date(2026, 8, 30, 16, 0, 0, 0, time.UTC), time.Date(2026, 8, 30, 20, 0, 0, 0, time.UTC), time.Date(2026, 8, 31, 0, 0, 0, 0, time.UTC)}
	if len(got) != len(want) {
		t.Fatalf("buckets = %v, want %v", got, want)
	}
	for i := range want {
		if !got[i].Equal(want[i]) {
			t.Fatalf("buckets = %v, want %v", got, want)
		}
	}
}
