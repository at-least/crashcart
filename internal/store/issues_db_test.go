package store_test

import (
	"context"
	"testing"

	"github.com/at-least/crashcart/internal/db/sqlc"
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
			ProjectID: p.ID, Fingerprint: fp, Title: "Issue " + fp, Level: "error", EventCount: int64(3 - i),
			FirstSeen: int64(100 + i), LastSeen: int64(200 + i), FirstRelease: &rel,
		}); err != nil {
			t.Fatal(err)
		}
	}
	rows, total, err := st.ListIssues(ctx, store.IssueFilter{ProjectID: p.ID, Sort: "events", Limit: 2})
	if err != nil {
		t.Fatal(err)
	}
	if total != 3 || len(rows) != 2 || rows[0].Fingerprint != "a" {
		t.Errorf("events sort: total=%d rows=%v", total, rows)
	}
	rows, _, err = st.ListIssues(ctx, store.IssueFilter{ProjectID: p.ID, Offset: 2})
	if err != nil || len(rows) != 1 || rows[0].Fingerprint != "a" {
		t.Errorf("last_seen desc offset 2: %v %v", rows, err)
	}
	_, total, err = st.ListIssues(ctx, store.IssueFilter{ProjectID: p.ID, Query: "issue b", Release: "1.0", From: 150})
	if err != nil || total != 1 {
		t.Errorf("filters: total=%d err=%v", total, err)
	}
}
