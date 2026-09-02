package store_test

import (
	"context"
	"testing"
	"time"

	"github.com/at-least/crashcart/internal/db/sqlc"
	"github.com/at-least/crashcart/internal/sentry"
	"github.com/at-least/crashcart/internal/store"
	"github.com/at-least/crashcart/internal/testdb"
)

func TestListIssuesDB(t *testing.T) {
	st := testdb.New(t)
	ctx := context.Background()
	p, err := st.CreateProject(ctx, sqlc.CreateProjectParams{Slug: "s", Name: "S", PublicKey: "k"})
	if err != nil {
		t.Fatal(err)
	}
	rel := "1.0"
	for i, fp := range []string{"a", "b", "c"} {
		if _, err := st.UpsertIssue(ctx, sqlc.UpsertIssueParams{
			ProjectID: p.ID, Fingerprint: sentry.DerivedID([]byte(fp)), Title: "Issue " + fp, Level: "error", EventCount: int64(3 - i),
			FirstSeen: time.Unix(int64(100+i), 0), LastSeen: time.Unix(int64(200+i), 0), FirstRelease: &rel,
		}); err != nil {
			t.Fatal(err)
		}
	}
	rows, total, err := st.ListIssues(ctx, store.IssueFilter{ProjectID: p.ID, Sort: "events", Limit: 2})
	if err != nil {
		t.Fatal(err)
	}
	if total != 3 || len(rows) != 2 || rows[0].Fingerprint != sentry.DerivedID([]byte("a")) {
		t.Errorf("events sort: total=%d rows=%v", total, rows)
	}
	rows, _, err = st.ListIssues(ctx, store.IssueFilter{ProjectID: p.ID, Offset: 2})
	if err != nil || len(rows) != 1 || rows[0].Fingerprint != sentry.DerivedID([]byte("a")) {
		t.Errorf("last_seen desc offset 2: %v %v", rows, err)
	}
	_, total, err = st.ListIssues(ctx, store.IssueFilter{ProjectID: p.ID, Query: "issue b", Release: "1.0", From: time.Unix(150, 0)})
	if err != nil || total != 1 {
		t.Errorf("filters: total=%d err=%v", total, err)
	}
}

// TestUpsertIssueRegressFlag: only ingest (regress = true) may flip a
// resolved issue to regression; symbolication moving an old event
// between issues passes false and leaves the status alone.
func TestUpsertIssueRegressFlag(t *testing.T) {
	st := testdb.New(t)
	testdb.Projects(t, st, 1)
	ctx := context.Background()
	fp := sentry.DerivedID([]byte("rf"))
	now := time.Now().UTC()
	base := sqlc.UpsertIssueParams{ProjectID: 1, Fingerprint: fp, Title: "T", Level: "error", EventCount: 1, StoredCount: 1, FirstSeen: now, LastSeen: now, Releases: []string{"1.0"}, Regress: true}
	if _, err := st.UpsertIssue(ctx, base); err != nil {
		t.Fatal(err)
	}
	if _, err := st.SetIssueStatus(ctx, sqlc.SetIssueStatusParams{ProjectID: 1, Fingerprint: fp, Status: "resolved"}); err != nil {
		t.Fatal(err)
	}
	moved := base
	moved.Releases, moved.Regress = []string{"2.0"}, false
	row, err := st.UpsertIssue(ctx, moved)
	if err != nil || row.Status != "resolved" {
		t.Fatalf("regress=false on a new release: status=%s err=%v (want resolved)", row.Status, err)
	}
	ingested := base
	ingested.Releases = []string{"2.0"}
	row, err = st.UpsertIssue(ctx, ingested)
	if err != nil || row.Status != "regression" {
		t.Fatalf("regress=true on a new release: status=%s err=%v (want regression)", row.Status, err)
	}
}
