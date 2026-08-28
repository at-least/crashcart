// Package jobs runs the Postgres-backed queue: claim a batch with
// SKIP LOCKED, dispatch by kind, delete on success, retry with backoff.
package jobs

import (
	"context"
	"encoding/json"
	"log/slog"
	"time"

	"github.com/newlix/crashcart/internal/db/sqlc"
	"github.com/newlix/crashcart/internal/store"
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

// Run blocks until ctx is done.
func (w *Worker) Run(ctx context.Context) {
	// TODO(jobs)
	<-ctx.Done()
}
