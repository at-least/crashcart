package jobs

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/crashcartapp/crashcart/internal/db/sqlc"
	"github.com/crashcartapp/crashcart/internal/testdb"
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
	ctx := context.Background()
	args, _ := json.Marshal(map[string]any{"event": 42})
	if err := st.EnqueueJob(ctx, sqlc.EnqueueJobParams{Kind: "flaky", ProjectID: 1, Args: args, RunAfter: time.Now()}); err != nil {
		t.Fatal(err)
	}
	if err := st.EnqueueJob(ctx, sqlc.EnqueueJobParams{Kind: "nobody-handles-this", ProjectID: 1, Args: []byte("{}"), RunAfter: time.Now()}); err != nil {
		t.Fatal(err)
	}

	var calls int
	var secondAttempts int32 = -1
	w := &Worker{Store: st, Poll: 10 * time.Millisecond, Handlers: map[string]Handler{
		"flaky": func(ctx context.Context, j sqlc.Job, a json.RawMessage) error {
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

	// First pass: the flaky job fails and is rescheduled; the unknown kind is dropped.
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
	if kind != "flaky" || attempts != 1 || len(lastErr) != 500 || runAfter.Before(time.Now().Add(4*time.Second)) {
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
		cnt, _ := st.CountJobs(ctx)
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
	if cnt, _ := st.CountJobs(ctx); cnt != 0 {
		t.Fatalf("job not deleted, %d left", cnt)
	}
	if calls != 2 || secondAttempts != 1 {
		t.Errorf("calls=%d attempts on retry=%d", calls, secondAttempts)
	}
}

func TestWorkerPanicIsRetry(t *testing.T) {
	st := testdb.New(t)
	ctx := context.Background()
	if err := st.EnqueueJob(ctx, sqlc.EnqueueJobParams{Kind: "boom", ProjectID: 1, Args: []byte("{}"), RunAfter: time.Now()}); err != nil {
		t.Fatal(err)
	}
	w := &Worker{Store: st, Handlers: map[string]Handler{
		"boom": func(context.Context, sqlc.Job, json.RawMessage) error { panic("nope") },
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
