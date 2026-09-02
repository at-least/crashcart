package jobs

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/crashcartapp/crashcart/internal/db/sqlc"
	"github.com/crashcartapp/crashcart/internal/testdb"
)

// TestClaimJobsSkipsLockedRows: SKIP LOCKED — a claim does not wait for
// rows another transaction holds; it takes the free ones at once. (Plain
// FOR UPDATE would block until that transaction ends and then re-check;
// the visible difference is the wait.)
func TestClaimJobsSkipsLockedRows(t *testing.T) {
	st := testdb.New(t)
	testdb.Projects(t, st, 1)
	ctx := context.Background()
	for i := 0; i < 5; i++ {
		args, _ := json.Marshal(map[string]any{"event": i})
		if err := st.EnqueueJob(ctx, sqlc.EnqueueJobParams{Kind: "symbolicate", ProjectID: 1, Args: args, RunAfter: time.Now()}); err != nil {
			t.Fatal(err)
		}
	}
	// Another worker mid-claim: it holds the first three rows locked.
	tx, err := st.Pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	rows, err := tx.Query(ctx, "SELECT id FROM jobs ORDER BY run_after, id LIMIT 3 FOR UPDATE")
	if err != nil {
		t.Fatal(err)
	}
	var held []int64
	for rows.Next() {
		var id int64
		rows.Scan(&id)
		held = append(held, id)
	}
	rows.Close()
	claimCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	start := time.Now()
	got, err := st.ClaimJobs(claimCtx, sqlc.ClaimJobsParams{Max: 10, LockedUntil: time.Now().Add(time.Minute)})
	if err != nil {
		t.Fatalf("claim waited on locked rows (%v): %v", time.Since(start), err)
	}
	if len(got) != 2 {
		t.Fatalf("claimed %d jobs, want the 2 unlocked ones", len(got))
	}
	for _, j := range got {
		for _, h := range held {
			if j.ID == h {
				t.Fatalf("claimed job %d while another transaction held it", h)
			}
		}
	}
	if err := tx.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	// The released rows are claimable now, and only once.
	if got, err := st.ClaimJobs(ctx, sqlc.ClaimJobsParams{Max: 10, LockedUntil: time.Now().Add(time.Minute)}); err != nil || len(got) != 3 {
		t.Fatalf("after the other transaction ended: %d %v", len(got), err)
	}
	if got, _ := st.ClaimJobs(ctx, sqlc.ClaimJobsParams{Max: 10, LockedUntil: time.Now().Add(time.Minute)}); len(got) != 0 {
		t.Fatalf("leased jobs claimed again: %d", len(got))
	}
}

// TestEnqueueOnlyPullsRunAfterForward: a repeat enqueue of a live job may
// make it due earlier, never later.
func TestEnqueueOnlyPullsRunAfterForward(t *testing.T) {
	st := testdb.New(t)
	testdb.Projects(t, st, 1)
	ctx := context.Background()
	args := []byte(`{"event": 7}`)
	enqueue := func(at time.Time) {
		t.Helper()
		if err := st.EnqueueJob(ctx, sqlc.EnqueueJobParams{Kind: "symbolicate", ProjectID: 1, Args: args, RunAfter: at}); err != nil {
			t.Fatal(err)
		}
	}
	runAfter := func() time.Time {
		t.Helper()
		var at time.Time
		var n int
		if err := st.Pool.QueryRow(ctx, "SELECT count(*), min(run_after) FROM jobs").Scan(&n, &at); err != nil || n != 1 {
			t.Fatalf("jobs = %d %v", n, err)
		}
		return at
	}
	now := time.Now().UTC().Truncate(time.Microsecond)
	enqueue(now)
	enqueue(now.Add(time.Hour)) // later: ignored
	if got := runAfter(); !got.Equal(now) {
		t.Fatalf("run_after pushed later: %v (want %v)", got, now)
	}
	earlier := now.Add(-time.Hour)
	enqueue(earlier) // earlier: taken
	if got := runAfter(); !got.Equal(earlier) {
		t.Fatalf("run_after not pulled forward: %v (want %v)", got, earlier)
	}
	// The same holds for the multi-row form.
	enqueue2 := func(at time.Time) {
		t.Helper()
		if err := st.EnqueueJobs(ctx, sqlc.EnqueueJobsParams{Kinds: []string{"symbolicate"}, ProjectIds: []int64{1}, Args: []json.RawMessage{args}, RunAfters: []time.Time{at}}); err != nil {
			t.Fatal(err)
		}
	}
	enqueue2(now)
	if got := runAfter(); !got.Equal(earlier) {
		t.Fatalf("EnqueueJobs pushed later: %v", got)
	}
	enqueue2(earlier.Add(-time.Minute))
	if got := runAfter(); !got.Equal(earlier.Add(-time.Minute)) {
		t.Fatalf("EnqueueJobs did not pull forward: %v", got)
	}
}

// TestDeadJobs: after maxAttempts a job is never claimed again, is listed
// by DeadJobs with its error, and ExpireJobs drops it only once it is a
// week old — a live job of any age stays.
func TestDeadJobs(t *testing.T) {
	st := testdb.New(t)
	testdb.Projects(t, st, 1)
	ctx := context.Background()
	msg := "gave up"
	insert := func(args string, attempts int, age time.Duration) int64 {
		t.Helper()
		var id int64
		if err := st.Pool.QueryRow(ctx, "INSERT INTO jobs (kind, project_id, args, run_after, attempts, last_error, created_at) VALUES ('symbolicate', 1, $1, now(), $2, $3, now() - $4::interval) RETURNING id",
			args, attempts, msg, fmt.Sprintf("%d seconds", int(age.Seconds()))).Scan(&id); err != nil {
			t.Fatal(err)
		}
		return id
	}
	deadOld := insert(`{"e": 1}`, maxAttempts, 8*24*time.Hour)
	deadNew := insert(`{"e": 2}`, maxAttempts, 0)
	liveOld := insert(`{"e": 3}`, maxAttempts-1, 30*24*time.Hour)
	// The live job is a month old: ExpireJobs leaves it, and the dead job
	// that is younger than a week; only the week-old dead one goes.
	n, err := st.ExpireJobs(ctx)
	if err != nil || n != 1 {
		t.Fatalf("ExpireJobs = %d %v (want the one dead for a week)", n, err)
	}
	var ids []int64
	rows, _ := st.Pool.Query(ctx, "SELECT id FROM jobs ORDER BY id")
	for rows.Next() {
		var id int64
		rows.Scan(&id)
		ids = append(ids, id)
	}
	rows.Close()
	if len(ids) != 2 || ids[0] != deadNew || ids[1] != liveOld || ids[0] == deadOld {
		t.Fatalf("jobs left = %v, want [%d %d]", ids, deadNew, liveOld)
	}
	// Claimed for its last attempt (attempts = 8: ExpireJobs would count it
	// as dead already — a live job older than a week on its 8th attempt is
	// the one case the sweep and the worker overlap), then exhausted:
	// RetryJob leaves attempts at the cap, so it is dead now.
	got, err := st.ClaimJobs(ctx, sqlc.ClaimJobsParams{Max: 10, LockedUntil: time.Now().Add(time.Minute)})
	if err != nil || len(got) != 1 || got[0].ID != liveOld || got[0].Attempts != maxAttempts {
		t.Fatalf("claim = %+v %v (want only the live job, on its last attempt)", got, err)
	}
	if err := st.RetryJob(ctx, sqlc.RetryJobParams{ID: liveOld, LastError: &msg, RunAfter: time.Now()}); err != nil {
		t.Fatal(err)
	}
	if got, _ := st.ClaimJobs(ctx, sqlc.ClaimJobsParams{Max: 10, LockedUntil: time.Now().Add(time.Minute)}); len(got) != 0 {
		t.Fatalf("dead job claimed: %+v", got)
	}
	dead, err := st.DeadJobs(ctx, 1)
	if err != nil || len(dead) != 2 {
		t.Fatalf("DeadJobs = %d %v", len(dead), err)
	}
	for _, d := range dead {
		if d.LastError == nil || *d.LastError != msg {
			t.Errorf("dead job %d without its error: %+v", d.ID, d)
		}
	}
	// A dead job's args are outside jobs_pending: a new enqueue is a fresh row.
	if err := st.EnqueueJob(ctx, sqlc.EnqueueJobParams{Kind: "symbolicate", ProjectID: 1, Args: []byte(`{"e": 2}`), RunAfter: time.Now()}); err != nil {
		t.Fatal(err)
	}
	if got, _ := st.ClaimJobs(ctx, sqlc.ClaimJobsParams{Max: 10, LockedUntil: time.Now().Add(time.Minute)}); len(got) != 1 || got[0].Attempts != 1 {
		t.Fatalf("re-enqueue after death: %+v", got)
	}
}

// TestWorkerPollsWithoutNotify: with a wake channel that never fires the
// poll timer still picks up due jobs (the fallback for a lost NOTIFY).
func TestWorkerPollsWithoutNotify(t *testing.T) {
	st := testdb.New(t)
	testdb.Projects(t, st, 1)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{}, 1)
	wake := make(chan string) // nobody sends
	w := &Worker{Store: st, Poll: 50 * time.Millisecond, Wake: wake, Handlers: map[string]Handler{
		"symbolicate": func(context.Context, sqlc.Job, json.RawMessage) error { done <- struct{}{}; return nil },
	}}
	go w.Run(ctx)
	time.Sleep(20 * time.Millisecond) // past the first (empty) claim
	if err := st.EnqueueJob(ctx, sqlc.EnqueueJobParams{Kind: "symbolicate", ProjectID: 1, Args: []byte("{}"), RunAfter: time.Now()}); err != nil {
		t.Fatal(err)
	}
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("poll fallback did not run the job")
	}
}
