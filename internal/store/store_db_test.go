package store_test

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/at-least/crashcart/internal/db/sqlc"
	"github.com/at-least/crashcart/internal/sentry"
	"github.com/at-least/crashcart/internal/store"
	"github.com/at-least/crashcart/internal/testdb"
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
	if err := st.InsertEvents(ctx, tx, rows); err != nil {
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
	crash.Handled = "false"
	if n := count(crash); n != 2 {
		t.Errorf("crash = %d (the fatal and the unhandled error; a handled error and a message are not unhandled)", n)
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

// TestRunAsLeaderDoesNotStarveQueryPool: regression test for
// https://github.com/at-least/crashcart/issues/1 — every distinct
// RunAsLeader key that cmd/crashcart ticks concurrently used to Acquire its
// advisory-lock connection from the same pool fn does its real work
// through. With Pool pinned to a single connection here, the pre-fix
// RunAsLeader deadlocks immediately: it holds the pool's only connection
// for the lock, then fn's own query has nowhere to Acquire from. Post-fix,
// RunAsLeader's lock lives on its own pool, so fn always gets Pool's one
// connection regardless of how many keys are held at once.
func TestRunAsLeaderDoesNotStarveQueryPool(t *testing.T) {
	st := testdb.NewWithMaxConns(t, 1)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	keys := []int64{store.LeaderSpikeCheck, store.LeaderSweep, store.LeaderRollup, store.LeaderIgnoreCheck, store.LeaderMonitorCheck, store.LeaderPack}
	var wg sync.WaitGroup
	var ran atomic.Int32
	for _, key := range keys {
		wg.Add(1)
		go func(key int64) {
			defer wg.Done()
			r, err := st.RunAsLeader(ctx, key, func() {
				if _, err := st.Pool.Exec(ctx, "SELECT 1"); err != nil {
					t.Errorf("fn's own query for key %d: %v", key, err)
					return
				}
				ran.Add(1)
			})
			if err != nil {
				t.Errorf("RunAsLeader(%d): %v", key, err)
			} else if !r {
				t.Errorf("RunAsLeader(%d): did not run — a distinct key must never contend with another", key)
			}
		}(key)
	}
	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-ctx.Done():
		t.Fatal("timed out: RunAsLeader deadlocked the query pool (see issue #1)")
	}
	if n := ran.Load(); n != int32(len(keys)) {
		t.Fatalf("only %d/%d fn ran", n, len(keys))
	}
}

// TestLogPoolStats: both pools appear in the log, by name, with their own
// MaxConns — Pool's is whatever the caller configured, lockPool's is the
// fixed maxLeaderLocks (5) regardless. This is the signal issue #1 was
// missing: nothing logged pool state before the process hung.
func TestLogPoolStats(t *testing.T) {
	st := testdb.NewWithMaxConns(t, 3)
	var buf bytes.Buffer
	log := slog.New(slog.NewTextHandler(&buf, nil))
	st.LogPoolStats(log)

	lines := strings.Split(strings.TrimSpace(buf.String()), "\n")
	if len(lines) != 2 {
		t.Fatalf("want 2 log lines (query, lock), got %d:\n%s", len(lines), buf.String())
	}
	seen := map[string]bool{}
	for _, l := range lines {
		if !strings.Contains(l, `msg="pool stats"`) {
			t.Errorf("not a pool-stats line: %s", l)
		}
		switch {
		case strings.Contains(l, "pool=query"):
			seen["query"] = true
			if !strings.Contains(l, "max=3") {
				t.Errorf("query pool max should be the configured 3: %s", l)
			}
		case strings.Contains(l, "pool=lock"):
			seen["lock"] = true
			if !strings.Contains(l, "max=6") {
				t.Errorf("lock pool max should be maxLeaderLocks (6): %s", l)
			}
		default:
			t.Errorf("unexpected pool line: %s", l)
		}
	}
	if !seen["query"] || !seen["lock"] {
		t.Fatalf("missing a pool's line: %v", seen)
	}
}

// TestCreateFirstUser: only one of many concurrent first-user creations
// succeeds — the check and the insert are one serialized transaction.
func TestCreateFirstUser(t *testing.T) {
	st := testdb.New(t)
	ctx := context.Background()
	var wg sync.WaitGroup
	var created atomic.Int32
	for i := range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			u, ok, err := st.CreateFirstUser(ctx, sqlc.CreateUserParams{Email: fmt.Sprintf("u%d@example.com", i), Name: "u", PasswordHash: "x"})
			if err != nil {
				t.Error(err)
			}
			if ok {
				created.Add(1)
				if u.ID == 0 {
					t.Error("created without a row")
				}
			}
		}()
	}
	wg.Wait()
	if n, _ := st.CountUsers(ctx); created.Load() != 1 || n != 1 {
		t.Fatalf("created=%d users=%d", created.Load(), n)
	}
}
