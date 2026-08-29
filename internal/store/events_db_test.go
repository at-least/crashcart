package store_test

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/crashcartapp/crashcart/internal/sentry"
	"github.com/crashcartapp/crashcart/internal/store"
	"github.com/crashcartapp/crashcart/internal/testdb"
)

func TestTagsFilterAndBreakdowns(t *testing.T) {
	st := testdb.New(t)
	testdb.Projects(t, st, 1)
	ctx := context.Background()
	now := time.Now().UTC()
	var rows []store.EventInsert
	for i := 0; i < 6; i++ {
		build := "42"
		if i%3 == 0 {
			build = "43"
		}
		rel := "1.0"
		if i%2 == 0 {
			rel = "1.1"
		}
		tags, _ := json.Marshal(map[string]string{"build": build, "device_id": fmt.Sprintf("d%d", i)})
		rows = append(rows, store.EventInsert{
			OccurredAt: now.Add(-time.Duration(i) * time.Minute), ProjectID: 1, EventID: sentry.DerivedID([]byte(fmt.Sprintf("e%d", i))), Level: "error", Message: "m",
			Release: &rel, Tags: tags, Payload: []byte("{}"),
		})
	}
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

	f := store.EventFilter{ProjectID: 1, From: now.Add(-time.Hour), To: now.Add(time.Hour), Tags: map[string]string{"build": "42"}}
	list, _, err := st.ListEvents(ctx, f)
	if err != nil || len(list) != 4 {
		t.Fatalf("tag filter: %d %v", len(list), err)
	}
	// The tag filter is a containment test so the GIN index can serve it
	// (on its own here: with six rows the planner prefers the time index).
	where, args := store.WhereForTest(store.EventFilter{Tags: map[string]string{"build": "42"}})
	where = strings.TrimPrefix(where, "project_id = $1 AND ")
	if _, err := st.Pool.Exec(ctx, "SET enable_seqscan = off"); err != nil {
		t.Fatal(err)
	}
	r, err := st.Pool.Query(ctx, "EXPLAIN SELECT event_id FROM events WHERE "+strings.ReplaceAll(where, "$2", "$1"), args[1:]...)
	var plan []string
	if err == nil {
		plan, err = pgx.CollectRows(r, pgx.RowTo[string])
	}
	if err != nil {
		t.Fatal(err)
	}
	st.Pool.Exec(ctx, "RESET enable_seqscan")
	if !strings.Contains(strings.Join(plan, "\n"), "events_tags") {
		t.Errorf("tag filter cannot use the GIN index:\n%s", strings.Join(plan, "\n"))
	}

	bds, err := st.Breakdowns(ctx, store.EventFilter{ProjectID: 1, From: now.Add(-time.Hour), To: now.Add(time.Hour)}, []string{"release", "tags.build", "os_version"}, 5)
	if err != nil {
		t.Fatal(err)
	}
	if got := bds["release"]; len(got) != 2 || got[0].Value != "1.0" && got[0].Value != "1.1" || got[0].Count != 3 {
		t.Errorf("release breakdown = %+v", got)
	}
	if got := bds["tags.build"]; len(got) != 2 || got[0].Value != "42" || got[0].Count != 4 || got[1].Value != "43" || got[1].Count != 2 {
		t.Errorf("tags.build breakdown = %+v", got)
	}
	if got := bds["os_version"]; len(got) != 1 || got[0].Value != "" || got[0].Count != 6 {
		t.Errorf("os_version breakdown = %+v", got)
	}
	if _, err := st.Breakdowns(ctx, f, []string{"payload"}, 5); err == nil {
		t.Error("disallowed column accepted")
	}
}

func TestBucketHelpers(t *testing.T) {
	st := testdb.New(t)
	ctx := context.Background()
	// Every bucket start in [from, to): the last, partial bucket included.
	var n int
	if err := st.Pool.QueryRow(ctx, "SELECT count(*) FROM crashcart_buckets('2026-01-01 00:00Z', '2026-01-01 02:30Z', 3600)").Scan(&n); err != nil || n != 3 {
		t.Errorf("buckets = %d %v, want 3", n, err)
	}
	if err := st.Pool.QueryRow(ctx, "SELECT count(*) FROM crashcart_buckets('2026-01-01 00:00Z', '2026-01-01 00:00Z', 3600)").Scan(&n); err != nil || n != 0 {
		t.Errorf("empty window = %d %v", n, err)
	}
	var b time.Time
	if err := st.Pool.QueryRow(ctx, "SELECT crashcart_bucket('2026-01-01 13:47Z', 4*3600)").Scan(&b); err != nil || !b.Equal(time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)) {
		t.Errorf("bucket = %v %v", b, err)
	}
}
