package store_test

import (
	"context"
	"testing"
	"time"

	"github.com/crashcartapp/crashcart/internal/sentry"
	"github.com/crashcartapp/crashcart/internal/store"
	"github.com/crashcartapp/crashcart/internal/testdb"
)

func ptr[T any](v T) *T { return &v }

func TestClipAndEscapeLike(t *testing.T) {
	if got := store.ClipForTest("héllo wörld", 5); got != "héllo" {
		t.Errorf("clip counts runes, not bytes: %q", got)
	}
	if got := store.ClipForTest("ééé", 4); got != "ééé" {
		t.Errorf("clip must not cut a string whose rune count fits: %q", got)
	}
	if got := store.ClipForTest("abc", 3); got != "abc" {
		t.Errorf("clip at the bound: %q", got)
	}
	if got := store.EscapeLikeForTest(`100%_\`); got != `100\%\_\\` {
		t.Errorf("escapeLike = %q", got)
	}
}

// TestCountEventsAndSearch: CountEvents applies the same filter as the
// list, and the free-text search treats % and _ literally.
func TestCountEventsAndSearch(t *testing.T) {
	st := testdb.New(t)
	testdb.Projects(t, st, 1, 2)
	ctx := context.Background()
	now := time.Now().UTC()
	var rows []store.EventInsert
	for i, m := range []struct {
		level, msg string
		handled    *bool
	}{
		{"error", "cart failed at 100%", ptr(true)}, {"info", "cart opened", nil}, {"fatal", "under_score", ptr(false)}, {"error", "underscore", ptr(false)},
	} {
		rows = append(rows, store.EventInsert{
			OccurredAt: now.Add(-time.Duration(i) * time.Minute), ProjectID: 1, EventID: sentry.DerivedID([]byte(m.msg)),
			Level: m.level, Message: m.msg, Handled: m.handled, Tags: []byte("{}"),
		})
	}
	// Another project's row must never be counted.
	rows = append(rows, store.EventInsert{OccurredAt: now, ProjectID: 2, EventID: sentry.DerivedID([]byte("other")), Level: "error", Message: "cart failed at 100%", Tags: []byte("{}")})
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
	base := store.EventFilter{ProjectID: 1, From: now.Add(-time.Hour), To: now.Add(time.Hour)}
	count := func(f store.EventFilter) int64 {
		t.Helper()
		n, err := st.CountEvents(ctx, f)
		if err != nil {
			t.Fatal(err)
		}
		list, _, err := st.ListEvents(ctx, f)
		if err != nil {
			t.Fatal(err)
		}
		if int64(len(list)) != n {
			t.Errorf("count %d disagrees with the list (%d rows) for %+v", n, len(list), f)
		}
		return n
	}
	with := func(q string) store.EventFilter { f := base; f.Query = q; return f }
	if n := count(base); n != 4 {
		t.Errorf("all = %d", n)
	}
	if n := count(with("cart")); n != 2 {
		t.Errorf("cart = %d", n)
	}
	if n := count(with("100%")); n != 1 {
		t.Errorf("a literal %% must not be a wildcard: %d", n)
	}
	if n := count(with("under_")); n != 1 {
		t.Errorf("a literal _ must not be a wildcard: %d", n)
	}
	if n := count(with("CART")); n != 2 {
		t.Errorf("search is case-insensitive: %d", n)
	}
	crash := base
	crash.Crash = true
	if n := count(crash); n != 2 {
		t.Errorf("crash = %d (the fatal and the unhandled error; a handled error and a message are not crashes)", n)
	}
	lvl := base
	lvl.Level = "error"
	if n := count(lvl); n != 2 {
		t.Errorf("level = %d", n)
	}
	outside := store.EventFilter{ProjectID: 1, From: now.Add(-2 * time.Hour), To: now.Add(-time.Hour)}
	if n := count(outside); n != 0 {
		t.Errorf("window = %d", n)
	}
	// Breakdown (single column) is Breakdowns' one-column form.
	bd, err := st.Breakdown(ctx, base, "level", 5)
	if err != nil || len(bd) != 3 || bd[0].Value != "error" || bd[0].Count != 2 {
		t.Errorf("breakdown = %+v %v", bd, err)
	}
	if _, err := st.Breakdown(ctx, base, "message", 5); err == nil {
		t.Error("message is not a breakdown column")
	}
}

// TestRunAsLeader: the advisory lock is per deployment — a second holder
// (another replica, or another pool connection here) is told it did not
// run, and the lock is released when fn returns.
func TestRunAsLeader(t *testing.T) {
	st := testdb.New(t)
	ctx := context.Background()
	const key = store.LeaderSweep
	outer := false
	ran, err := st.RunAsLeader(ctx, key, func() {
		outer = true
		inner, err := st.RunAsLeader(ctx, key, func() { t.Error("second holder must not run while the first holds the lock") })
		if err != nil || inner {
			t.Errorf("nested: ran=%v err=%v", inner, err)
		}
	})
	if err != nil || !ran || !outer {
		t.Fatalf("first: ran=%v outer=%v err=%v", ran, outer, err)
	}
	// Released: the next caller becomes leader; another key is independent.
	again := false
	if ran, err := st.RunAsLeader(ctx, key, func() { again = true }); err != nil || !ran || !again {
		t.Fatalf("after release: ran=%v again=%v err=%v", ran, again, err)
	}
	if ran, err := st.RunAsLeader(ctx, key, func() {
		if other, err := st.RunAsLeader(ctx, store.LeaderRollup, func() {}); err != nil || !other {
			t.Errorf("other key: ran=%v err=%v", other, err)
		}
	}); err != nil || !ran {
		t.Fatal(ran, err)
	}
}
