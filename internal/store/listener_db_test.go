package store_test

import (
	"context"
	"testing"
	"time"

	"github.com/crashcartapp/crashcart/internal/db/sqlc"
	"github.com/crashcartapp/crashcart/internal/sentry"
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
		if err := st.EnqueueJob(ctx, sqlc.EnqueueJobParams{Kind: "alert", ProjectID: 1, Args: []byte("{}"), RunAfter: time.Now()}); err != nil {
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
	if _, err := st.UpsertIssue(ctx, sqlc.UpsertIssueParams{ProjectID: 7, Fingerprint: sentry.DerivedID([]byte("f")), Title: "T", Level: "error", EventCount: 1, FirstSeen: now, LastSeen: now}); err != nil {
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
	if _, err := st.UpsertIssue(ctx, sqlc.UpsertIssueParams{ProjectID: 7, Fingerprint: sentry.DerivedID([]byte("f")), Title: "T", Level: "error", EventCount: 1, FirstSeen: now, LastSeen: now}); err != nil {
		t.Fatal(err)
	}
	select {
	case <-mine:
		t.Fatal("an existing issue must not notify")
	case <-time.After(200 * time.Millisecond):
	}
	// Resolve, then see it again on another release: regression notifies.
	if _, err := st.SetIssueStatus(ctx, sqlc.SetIssueStatusParams{ProjectID: 7, Fingerprint: sentry.DerivedID([]byte("f")), Status: "resolved"}); err != nil {
		t.Fatal(err)
	}
	rel := "2.0"
	if _, err := st.UpsertIssue(ctx, sqlc.UpsertIssueParams{ProjectID: 7, Fingerprint: sentry.DerivedID([]byte("f")), Title: "T", Level: "error", EventCount: 1, FirstSeen: now, LastSeen: now.Add(time.Second), FirstRelease: &rel, Releases: []string{rel}, Regress: true}); err != nil {
		t.Fatal(err)
	}
	select {
	case <-mine:
	case <-time.After(3 * time.Second):
		t.Fatal("no regression notification")
	}
}

// TestListenerKeepalive: a notification still arrives after the listener
// has been idle longer than its keepalive (the wait is sliced and the
// connection pinged in between).
func TestListenerKeepalive(t *testing.T) {
	st := testdb.New(t)
	testdb.Projects(t, st, 1)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	old := store.ListenKeepalive
	store.ListenKeepalive = 50 * time.Millisecond
	t.Cleanup(func() { store.ListenKeepalive = old })
	l := &store.Listener{Pool: st.Pool}
	jobs, stop := l.Subscribe(store.ChannelJobs, "")
	defer stop()
	go l.Run(ctx)
	time.Sleep(300 * time.Millisecond) // several keepalive rounds
	deadline := time.Now().Add(5 * time.Second)
	for {
		if err := st.EnqueueJob(ctx, sqlc.EnqueueJobParams{Kind: "alert", ProjectID: 1, Args: []byte(`{"k":1}`), RunAfter: time.Now()}); err != nil {
			t.Fatal(err)
		}
		select {
		case <-jobs:
			return
		case <-time.After(200 * time.Millisecond):
			if time.Now().After(deadline) {
				t.Fatal("no notification after keepalive rounds")
			}
		}
	}
}

// TestListenerReconnects: when the LISTEN connection dies (the server
// drops it here), the listener counts the loss, reconnects and delivers
// again — the wake-ups a replica relies on survive a network blip.
func TestListenerReconnects(t *testing.T) {
	st := testdb.New(t)
	testdb.Projects(t, st, 1)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	l := &store.Listener{Pool: st.Pool}
	jobs, stop := l.Subscribe(store.ChannelJobs, "")
	defer stop()
	go l.Run(ctx)
	wait := func(what string) {
		t.Helper()
		deadline := time.Now().Add(10 * time.Second)
		for {
			if err := st.EnqueueJob(ctx, sqlc.EnqueueJobParams{Kind: "alert", ProjectID: 1, Args: []byte("{}"), RunAfter: time.Now()}); err != nil {
				t.Fatal(err)
			}
			select {
			case <-jobs:
				return
			case <-time.After(200 * time.Millisecond):
				if time.Now().After(deadline) {
					t.Fatalf("no jobs notification %s", what)
				}
			}
		}
	}
	wait("before the disconnect")
	before := store.ListenerReconnects.Value()
	// Kill the listening backend: its last statement was the LISTEN.
	var killed int
	if err := st.Pool.QueryRow(ctx, `SELECT count(*) FROM (SELECT pg_terminate_backend(pid) FROM pg_stat_activity
		WHERE datname = current_database() AND pid <> pg_backend_pid() AND query LIKE 'LISTEN %') k`).Scan(&killed); err != nil || killed == 0 {
		t.Fatalf("terminate listener backend: killed=%d err=%v", killed, err)
	}
	wait("after the reconnect")
	if got := store.ListenerReconnects.Value(); got != before+1 {
		t.Errorf("reconnects = %d, want %d", got, before+1)
	}
	// Drain what the retries queued so the count above stays exact.
	for {
		select {
		case <-jobs:
			continue
		default:
		}
		break
	}
}
