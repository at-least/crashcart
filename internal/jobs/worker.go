// Package jobs runs the Postgres-backed queue: lease a batch with
// SKIP LOCKED (one short transaction), run the handlers with no
// transaction open, delete on success, retry with backoff. A lease that
// expires — the worker died — makes the job claimable again. Workers wake
// on the jobs NOTIFY (store.Listener) and poll as a fallback.
package jobs

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/crashcartapp/crashcart/internal/metrics"
	"log/slog"
	"strings"
	"time"

	"github.com/crashcartapp/crashcart/internal/db/sqlc"
	"github.com/crashcartapp/crashcart/internal/store"
)

// Handler processes one job; returning an error schedules a retry.
type Handler func(ctx context.Context, job sqlc.Job, args json.RawMessage) error

// Worker claims jobs when woken (Wake) and on a timer.
type Worker struct {
	Store    *store.Store
	Log      *slog.Logger
	Handlers map[string]Handler
	Wake     <-chan string // store.Listener subscription on ChannelJobs; nil = poll only
	Poll     time.Duration // idle sleep (default 2 s; 30 s when Wake is set — retries are due at most that late)
	Batch    int32         // jobs per claim (default 25)
	Lease    time.Duration // how long a claimed job stays ours; the handler's ctx deadline (default 10 min)
}

// JobsTotal counts finished handler runs by kind and outcome (ok, retry,
// dead = the last allowed attempt failed).
var JobsTotal = metrics.NewCounter("crashcart_jobs_total", "Job runs by kind and outcome.", "kind", "outcome")

// maxAttempts mirrors the jobs_pending / ClaimJobs bound (attempts < 8).
const maxAttempts = 8

const (
	defaultPoll     = 2 * time.Second
	defaultWakePoll = 30 * time.Second
	defaultBatch    = 25
	defaultLease    = 10 * time.Minute
	baseBackoff     = 5 * time.Second
	maxBackoff      = 10 * time.Minute
	maxErrorChars   = 500
)

// Backoff is the delay before attempt n+1 after n failed attempts:
// 5 s, 10 s, 20 s, … capped at 10 minutes.
func Backoff(attempts int32) time.Duration {
	if attempts < 0 {
		attempts = 0
	}
	if attempts > 10 {
		return maxBackoff
	}
	d := baseBackoff << uint(attempts)
	if d > maxBackoff {
		return maxBackoff
	}
	return d
}

// Run blocks until ctx is done. Each iteration leases one batch, runs
// the handlers, and deletes the succeeded jobs / reschedules the failed
// ones as it goes.
func (w *Worker) Run(ctx context.Context) {
	poll := w.Poll
	if poll <= 0 {
		poll = defaultPoll
		if w.Wake != nil {
			poll = defaultWakePoll
		}
	}
	for {
		if ctx.Err() != nil {
			return
		}
		n, err := w.RunOnce(ctx)
		if err != nil && ctx.Err() == nil {
			w.log().Error("jobs: batch", "err", err)
		}
		if n > 0 && err == nil {
			continue
		}
		select {
		case <-ctx.Done():
			return
		case <-w.Wake:
		case <-time.After(poll):
		}
	}
}

// RunOnce leases and processes one batch; it returns how many jobs it
// claimed (0 = queue idle).
func (w *Worker) RunOnce(ctx context.Context) (int, error) {
	batch := w.Batch
	if batch <= 0 {
		batch = defaultBatch
	}
	lease := w.Lease
	if lease <= 0 {
		lease = defaultLease
	}
	jobs, err := w.Store.ClaimJobs(ctx, sqlc.ClaimJobsParams{Max: batch, LockedUntil: time.Now().Add(lease)})
	if err != nil {
		return 0, err
	}
	var firstErr error
	for _, j := range jobs {
		if ctx.Err() != nil {
			// Shutting down: hand the rest of the batch back untouched.
			if err := w.Store.ReleaseJob(context.WithoutCancel(ctx), j.ID); err != nil && firstErr == nil {
				firstErr = err
			}
			continue
		}
		// One job's outcome failing to record must not leave the rest of
		// the batch leased and untouched until the lease expires.
		if err := w.dispatch(ctx, j); err != nil {
			w.log().Error("jobs: record outcome", "id", j.ID, "kind", j.Kind, "err", err)
			if firstErr == nil {
				firstErr = err
			}
		}
	}
	if firstErr != nil {
		return len(jobs), firstErr
	}
	return len(jobs), ctx.Err()
}

// dispatch runs one job and records the outcome (auto-commit; nothing is
// held open while the handler runs). The returned error is a failure to
// record the outcome, not the handler's.
func (w *Worker) dispatch(ctx context.Context, j sqlc.Job) error {
	bg := context.WithoutCancel(ctx)
	h, ok := w.Handlers[string(j.Kind)]
	if !ok {
		w.log().Warn("jobs: unknown kind, dropping", "id", j.ID, "kind", j.Kind, "project", j.ProjectID)
		return w.Store.DeleteJob(bg, j.ID)
	}
	// The handler's deadline is the lease itself (claimed for the whole
	// batch): past it the job is claimable by another worker, so running
	// on would run it twice.
	deadline := time.Now().Add(defaultLease)
	if j.LockedUntil != nil {
		deadline = *j.LockedUntil
	}
	hctx, cancel := context.WithDeadline(ctx, deadline)
	err := w.run(hctx, h, j)
	cancel()
	if err == nil {
		JobsTotal.Inc(string(j.Kind), "ok")
		return w.Store.DeleteJob(bg, j.ID)
	}
	if errors.Is(err, context.Canceled) && ctx.Err() != nil {
		return w.Store.ReleaseJob(bg, j.ID) // shutting down: not an attempt
	}
	msg := truncate(err.Error(), maxErrorChars)
	delay := Backoff(j.Attempts - 1)
	if j.Attempts >= maxAttempts {
		JobsTotal.Inc(string(j.Kind), "dead")
	} else {
		JobsTotal.Inc(string(j.Kind), "retry")
	}
	w.log().Warn("jobs: failed", "id", j.ID, "kind", j.Kind, "project", j.ProjectID, "attempt", j.Attempts, "retry_in", delay, "err", msg)
	return w.Store.RetryJob(bg, sqlc.RetryJobParams{ID: j.ID, LastError: &msg, RunAfter: time.Now().Add(delay)})
}

// run calls the handler, turning a panic into an error so one bad job
// cannot take the worker down.
func (w *Worker) run(ctx context.Context, h Handler, j sqlc.Job) (err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("panic: %v", r)
		}
	}()
	return h(ctx, j, j.Args)
}

func (w *Worker) log() *slog.Logger {
	if w.Log != nil {
		return w.Log
	}
	return slog.Default()
}

// truncate caps s at n runes and drops NUL bytes (Postgres rejects them in text).
func truncate(s string, n int) string {
	s = strings.ReplaceAll(s, "\x00", "")
	if len(s) <= n {
		return s
	}
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n])
}
