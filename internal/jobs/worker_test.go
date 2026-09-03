package jobs

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/at-least/crashcart/internal/store"
	"github.com/at-least/crashcart/internal/testdb"
)

func TestBackoff(t *testing.T) {
	want := []time.Duration{5 * time.Second, 10 * time.Second, 20 * time.Second, 40 * time.Second, 80 * time.Second, 160 * time.Second, 320 * time.Second, 10 * time.Minute, 10 * time.Minute}
	for i, w := range want {
		if got := Backoff(int32(i)); got != w {
			t.Errorf("Backoff(%d) = %v, want %v", i, got, w)
		}
	}
	if Backoff(100) != 10*time.Minute {
		t.Error("cap")
	}
}

func TestWorkerRetryThenSucceed(t *testing.T) {
	st := testdb.New(t)
	testdb.Projects(t, st, 1)
	ctx := context.Background()
	args, _ := json.Marshal(map[string]any{"event": 42})
	if err := store.EnqueueJob(ctx, st.Pool, "symbolicate", 1, args, time.Now()); err != nil {
		t.Fatal(err)
	}
	if err := store.EnqueueJob(ctx, st.Pool, "alert", 1, []byte("{}"), time.Now()); err != nil {
		t.Fatal(err)
	}

	var calls int
	var secondAttempts int32 = -1
	w := &Worker{Store: st, Poll: 10 * time.Millisecond, Handlers: map[string]Handler{
		"symbolicate": func(ctx context.Context, j store.Job, a json.RawMessage) error {
			calls++
			var got struct {
				Event int64 `json:"event"`
			}
			if err := json.Unmarshal(a, &got); err != nil || got.Event != 42 {
				t.Errorf("args = %s", a)
			}
			if calls == 1 {
				return errors.New("transient failure \x00" + strings.Repeat("x", 600))
			}
			secondAttempts = j.Attempts
			return nil
		},
	}}

	// First pass: the symbolicate job fails and is rescheduled; the alert job has no handler registered and is dropped.
	n, err := w.RunOnce(ctx)
	if err != nil || n != 2 {
		t.Fatalf("RunOnce = %d %v", n, err)
	}
	var attempts int32
	var lastErr string
	var runAfter time.Time
	var kind string
	if err := st.Pool.QueryRow(ctx, "SELECT kind, attempts, last_error, run_after FROM jobs").Scan(&kind, &attempts, &lastErr, &runAfter); err != nil {
		t.Fatalf("expected exactly one job left: %v", err)
	}
	if kind != "symbolicate" || attempts != 1 || len(lastErr) != 500 || runAfter.Before(time.Now().Add(4*time.Second)) {
		t.Fatalf("after failure: kind=%s attempts=%d err_len=%d run_after=%v", kind, attempts, len(lastErr), runAfter)
	}

	// Not due yet: nothing claimed.
	if n, err := w.RunOnce(ctx); err != nil || n != 0 {
		t.Fatalf("premature claim: %d %v", n, err)
	}

	// Make it due and let Run pick it up.
	if _, err := st.Pool.Exec(ctx, "UPDATE jobs SET run_after = now()"); err != nil {
		t.Fatal(err)
	}
	runCtx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() { w.Run(runCtx); close(done) }()
	deadline := time.Now().Add(5 * time.Second)
	for {
		cnt, _ := store.CountJobs(ctx, st.Pool)
		if cnt == 0 || time.Now().After(deadline) {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not stop on cancel")
	}
	if cnt, _ := store.CountJobs(ctx, st.Pool); cnt != 0 {
		t.Fatalf("job not deleted, %d left", cnt)
	}
	if calls != 2 || secondAttempts != 2 { // attempts are counted at claim time
		t.Errorf("calls=%d attempts on retry=%d", calls, secondAttempts)
	}
}

func TestWorkerPanicIsRetry(t *testing.T) {
	st := testdb.New(t)
	testdb.Projects(t, st, 1)
	ctx := context.Background()
	if err := store.EnqueueJob(ctx, st.Pool, "symbolicate", 1, []byte("{}"), time.Now()); err != nil {
		t.Fatal(err)
	}
	w := &Worker{Store: st, Handlers: map[string]Handler{
		"symbolicate": func(context.Context, store.Job, json.RawMessage) error { panic("nope") },
	}}
	if _, err := w.RunOnce(ctx); err != nil {
		t.Fatal(err)
	}
	var attempts int32
	var lastErr string
	if err := st.Pool.QueryRow(ctx, "SELECT attempts, last_error FROM jobs").Scan(&attempts, &lastErr); err != nil {
		t.Fatal(err)
	}
	if attempts != 1 || lastErr != "panic: nope" {
		t.Errorf("attempts=%d last_error=%q", attempts, lastErr)
	}
}

func TestWorkerWakesOnNotify(t *testing.T) {
	st := testdb.New(t)
	testdb.Projects(t, st, 1)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	l := &store.Listener{Pool: st.Pool}
	wake, stop := l.Subscribe(store.ChannelJobs, "")
	defer stop()
	go l.Run(ctx)
	done := make(chan struct{}, 10)
	w := &Worker{Store: st, Poll: time.Minute, Wake: wake, Handlers: map[string]Handler{
		"symbolicate": func(context.Context, store.Job, json.RawMessage) error { done <- struct{}{}; return nil },
	}}
	go w.Run(ctx)
	// The worker is idle on a one-minute poll; a queued job must run within a
	// second because the trigger's NOTIFY wakes it. (The first enqueue may
	// race the LISTEN; retry until it lands.)
	deadline := time.Now().Add(5 * time.Second)
	for {
		if err := store.EnqueueJob(ctx, st.Pool, "symbolicate", 1, []byte("{}"), time.Now()); err != nil {
			t.Fatal(err)
		}
		select {
		case <-done:
			return
		case <-time.After(500 * time.Millisecond):
			if time.Now().After(deadline) {
				t.Fatal("worker did not wake on NOTIFY")
			}
		}
	}
}

func TestWorkerLeaseExpires(t *testing.T) {
	st := testdb.New(t)
	testdb.Projects(t, st, 1)
	ctx := context.Background()
	if err := store.EnqueueJob(ctx, st.Pool, "symbolicate", 1, []byte("{}"), time.Now()); err != nil {
		t.Fatal(err)
	}
	// A worker that died mid-job: the lease is taken but no outcome is recorded.
	if got, err := store.ClaimJobs(ctx, st.Pool, time.Now().Add(50*time.Millisecond), 10); err != nil || len(got) != 1 {
		t.Fatalf("claim: %d %v", len(got), err)
	}
	if got, _ := store.ClaimJobs(ctx, st.Pool, time.Now().Add(time.Minute), 10); len(got) != 0 {
		t.Fatalf("leased job claimed again: %d", len(got))
	}
	time.Sleep(60 * time.Millisecond)
	got, err := store.ClaimJobs(ctx, st.Pool, time.Now().Add(time.Minute), 10)
	if err != nil || len(got) != 1 || got[0].Attempts != 2 {
		t.Fatalf("after lease expiry: %d %v %+v", len(got), err, got)
	}
}

// TestEnqueueWhileLeasedOrBackingOff: the same (kind, project, args)
// enqueued while a job is leased is one row (the retry would otherwise
// collide with it), and an enqueue during a backoff pulls it forward.
func TestEnqueueWhileLeasedOrBackingOff(t *testing.T) {
	st := testdb.New(t)
	testdb.Projects(t, st, 1)
	ctx := context.Background()
	args := []byte(`{"event": 1}`)
	enqueue := func(at time.Time) {
		t.Helper()
		if err := store.EnqueueJob(ctx, st.Pool, "symbolicate", 1, args, at); err != nil {
			t.Fatal(err)
		}
	}
	enqueue(time.Now())
	got, err := store.ClaimJobs(ctx, st.Pool, time.Now().Add(time.Minute), 10)
	if err != nil || len(got) != 1 {
		t.Fatalf("claim: %d %v", len(got), err)
	}
	enqueue(time.Now()) // while leased
	if n, _ := store.CountJobs(ctx, st.Pool); n != 1 {
		t.Fatalf("jobs after enqueue during lease = %d, want 1", n)
	}
	// The retry must not collide with a second row.
	msg := "x"
	if err := store.RetryJob(ctx, st.Pool, got[0].ID, &msg, time.Now().Add(10*time.Minute)); err != nil {
		t.Fatalf("retry: %v", err)
	}
	// Backing off: a new enqueue makes it due now.
	enqueue(time.Now())
	var runAfter time.Time
	if err := st.Pool.QueryRow(ctx, "SELECT run_after FROM jobs").Scan(&runAfter); err != nil || runAfter.After(time.Now().Add(time.Second)) {
		t.Fatalf("run_after after re-enqueue = %v %v (want now)", runAfter, err)
	}
	if n, _ := store.CountJobs(ctx, st.Pool); n != 1 {
		t.Fatalf("jobs = %d", n)
	}
}

// TestHandlerDeadlineIsLease: a handler's context ends when the lease
// does, however late in the batch the job runs.
func TestHandlerDeadlineIsLease(t *testing.T) {
	st := testdb.New(t)
	testdb.Projects(t, st, 1)
	ctx := context.Background()
	if err := store.EnqueueJob(ctx, st.Pool, "alert", 1, []byte("{}"), time.Now()); err != nil {
		t.Fatal(err)
	}
	var deadline time.Time
	w := &Worker{Store: st, Lease: 200 * time.Millisecond, Handlers: map[string]Handler{
		"alert": func(ctx context.Context, j store.Job, _ json.RawMessage) error {
			deadline, _ = ctx.Deadline()
			if j.LockedUntil == nil || !deadline.Equal(*j.LockedUntil) {
				t.Errorf("deadline %v != locked_until %v", deadline, j.LockedUntil)
			}
			return nil
		},
	}}
	if _, err := w.RunOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if deadline.IsZero() {
		t.Fatal("handler not run")
	}
}

// TestReleaseJobAtAttemptCap: a job released at shutdown on its last
// attempt is outside the jobs_pending index, so a duplicate may have been
// enqueued meanwhile; the release must not collide with it (which left
// the job leased until it died) — the older row goes, the newer runs.
func TestReleaseJobAtAttemptCap(t *testing.T) {
	st := testdb.New(t)
	testdb.Projects(t, st, 1)
	ctx := context.Background()
	args := []byte(`{"event": 1}`)
	if err := store.EnqueueJob(ctx, st.Pool, "symbolicate", 1, args, time.Now()); err != nil {
		t.Fatal(err)
	}
	// Claimed for the 8th time: attempts = 8.
	st.Pool.Exec(ctx, "UPDATE jobs SET attempts = 7")
	got, err := store.ClaimJobs(ctx, st.Pool, time.Now().Add(time.Minute), 10)
	if err != nil || len(got) != 1 || got[0].Attempts != 8 {
		t.Fatalf("claim: %+v %v", got, err)
	}
	if err := store.EnqueueJob(ctx, st.Pool, "symbolicate", 1, args, time.Now()); err != nil {
		t.Fatal(err) // a second row: the first is no longer pending
	}
	if err := store.ReleaseJob(ctx, st.Pool, got[0].ID); err != nil {
		t.Fatalf("release with a duplicate pending: %v", err)
	}
	var n int
	var leased bool
	if err := st.Pool.QueryRow(ctx, "SELECT count(*), bool_or(locked_until IS NOT NULL) FROM jobs").Scan(&n, &leased); err != nil || n != 1 || leased {
		t.Fatalf("after release: rows=%d leased=%v err=%v (want one free row)", n, leased, err)
	}
	// Without a duplicate the release un-counts the attempt as before.
	st.Pool.Exec(ctx, "DELETE FROM jobs")
	store.EnqueueJob(ctx, st.Pool, "symbolicate", 1, args, time.Now())
	got, _ = store.ClaimJobs(ctx, st.Pool, time.Now().Add(time.Minute), 10)
	if err := store.ReleaseJob(ctx, st.Pool, got[0].ID); err != nil {
		t.Fatal(err)
	}
	var attempts int
	if err := st.Pool.QueryRow(ctx, "SELECT attempts FROM jobs WHERE locked_until IS NULL").Scan(&attempts); err != nil || attempts != 0 {
		t.Fatalf("plain release: attempts=%d err=%v", attempts, err)
	}
}
