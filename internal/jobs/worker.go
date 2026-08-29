// Package jobs runs the Postgres-backed queue: claim a batch with
// SKIP LOCKED, dispatch by kind, delete on success, retry with backoff.
package jobs

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/crashcartapp/crashcart/internal/db/sqlc"
	"github.com/crashcartapp/crashcart/internal/store"
)

// Handler processes one job; returning an error schedules a retry.
type Handler func(ctx context.Context, job sqlc.Job, args json.RawMessage) error

// Worker polls the jobs table.
type Worker struct {
	Store    *store.Store
	Log      *slog.Logger
	Handlers map[string]Handler
	Poll     time.Duration // idle sleep (default 2 s)
	Batch    int32         // jobs per claim (default 25)
}

const (
	defaultPoll   = 2 * time.Second
	defaultBatch  = 25
	baseBackoff   = 5 * time.Second
	maxBackoff    = 10 * time.Minute
	maxErrorChars = 500
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

// Run blocks until ctx is done. Each iteration claims one batch inside a
// transaction (the rows stay locked while their handlers run), deletes
// the succeeded jobs, reschedules the failed ones and commits.
func (w *Worker) Run(ctx context.Context) {
	poll := w.Poll
	if poll <= 0 {
		poll = defaultPoll
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
		case <-time.After(poll):
		}
	}
}

// RunOnce claims and processes one batch; it returns how many jobs it
// claimed (0 = queue idle).
func (w *Worker) RunOnce(ctx context.Context) (int, error) {
	batch := w.Batch
	if batch <= 0 {
		batch = defaultBatch
	}
	var n int
	err := w.Store.Tx(ctx, func(ctx context.Context, tx pgx.Tx, q *sqlc.Queries) error {
		jobs, err := q.ClaimJobs(ctx, batch)
		if err != nil {
			return err
		}
		n = len(jobs)
		for _, j := range jobs {
			if err := ctx.Err(); err != nil {
				return err
			}
			if err := w.dispatch(ctx, q, j); err != nil {
				return err
			}
		}
		return nil
	})
	return n, err
}

// dispatch runs one job and records the outcome in the transaction.
func (w *Worker) dispatch(ctx context.Context, q *sqlc.Queries, j sqlc.Job) error {
	h, ok := w.Handlers[j.Kind]
	if !ok {
		w.log().Warn("jobs: unknown kind, dropping", "id", j.ID, "kind", j.Kind, "project", j.ProjectID)
		return q.DeleteJob(ctx, j.ID)
	}
	err := w.run(ctx, h, j)
	if err == nil {
		return q.DeleteJob(ctx, j.ID)
	}
	if errors.Is(err, context.Canceled) && ctx.Err() != nil {
		return err // shutting down: leave the row untouched (rolls back)
	}
	msg := truncate(err.Error(), maxErrorChars)
	delay := Backoff(j.Attempts)
	w.log().Warn("jobs: failed", "id", j.ID, "kind", j.Kind, "project", j.ProjectID, "attempt", j.Attempts+1, "retry_in", delay, "err", msg)
	return q.RetryJob(ctx, sqlc.RetryJobParams{ID: j.ID, LastError: &msg, RunAfter: time.Now().Add(delay)})
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
