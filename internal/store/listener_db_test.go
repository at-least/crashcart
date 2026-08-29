package store_test

import (
	"context"
	"testing"
	"time"

	"github.com/crashcartapp/crashcart/internal/db/sqlc"
	"github.com/crashcartapp/crashcart/internal/store"
	"github.com/crashcartapp/crashcart/internal/testdb"
)

func TestListenerNotifications(t *testing.T) {
	st := testdb.New(t)
	testdb.Projects(t, st, 1, 7)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	l := &store.Listener{Pool: st.Pool}
	jobs, stopJobs := l.Subscribe(store.ChannelJobs, "")
	defer stopJobs()
	mine, stopMine := l.Subscribe(store.ChannelIssues, "7")
	defer stopMine()
	other, stopOther := l.Subscribe(store.ChannelIssues, "8")
	defer stopOther()
	go l.Run(ctx)
	// LISTEN is asynchronous: retry the first enqueue until the wake-up arrives.
	deadline := time.Now().Add(5 * time.Second)
	for {
		if err := st.EnqueueJob(ctx, sqlc.EnqueueJobParams{Kind: "x", ProjectID: 1, Args: []byte("{}"), RunAfter: time.Now()}); err != nil {
			t.Fatal(err)
		}
		select {
		case <-jobs:
		case <-time.After(200 * time.Millisecond):
			if time.Now().After(deadline) {
				t.Fatal("no jobs notification")
			}
			continue
		}
		break
	}
	now := time.Now()
	if _, err := st.UpsertIssue(ctx, sqlc.UpsertIssueParams{ProjectID: 7, Fingerprint: "f", Title: "T", Level: "error", EventCount: 1, FirstSeen: now, LastSeen: now}); err != nil {
		t.Fatal(err)
	}
	select {
	case p := <-mine:
		if p != "7" {
			t.Fatalf("payload = %q", p)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("no issue notification for project 7")
	}
	select {
	case p := <-other:
		t.Fatalf("project 8 must not be woken (got %q)", p)
	case <-time.After(100 * time.Millisecond):
	}
	// A second event on the same issue is an update, not a new issue: no notification.
	if _, err := st.UpsertIssue(ctx, sqlc.UpsertIssueParams{ProjectID: 7, Fingerprint: "f", Title: "T", Level: "error", EventCount: 1, FirstSeen: now, LastSeen: now}); err != nil {
		t.Fatal(err)
	}
	select {
	case <-mine:
		t.Fatal("an existing issue must not notify")
	case <-time.After(200 * time.Millisecond):
	}
	// Resolve, then see it again on another release: regression notifies.
	if _, err := st.SetIssueStatus(ctx, sqlc.SetIssueStatusParams{ProjectID: 7, Fingerprint: "f", Status: "resolved"}); err != nil {
		t.Fatal(err)
	}
	rel := "2.0"
	if _, err := st.UpsertIssue(ctx, sqlc.UpsertIssueParams{ProjectID: 7, Fingerprint: "f", Title: "T", Level: "error", EventCount: 1, FirstSeen: now, LastSeen: now.Add(time.Second), FirstRelease: &rel}); err != nil {
		t.Fatal(err)
	}
	select {
	case <-mine:
	case <-time.After(3 * time.Second):
		t.Fatal("no regression notification")
	}
}
