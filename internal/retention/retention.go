// Package retention reconciles TimescaleDB policies from the configuration
// and sweeps the non-hypertable tables.
package retention

import (
	"context"
	"log/slog"

	"github.com/newlix/crashcart/internal/config"
	"github.com/newlix/crashcart/internal/store"
)

// Reconcile (re)creates the compression and retention policies so they
// match RETENTION_DAYS / COMPRESS_AFTER. Idempotent; run at startup.
func Reconcile(ctx context.Context, st *store.Store, cfg config.Config, log *slog.Logger) error {
	return nil // TODO(retention)
}

// Sweep expires issues, jobs, rate-limit windows and symbol files.
func Sweep(ctx context.Context, st *store.Store, cfg config.Config, log *slog.Logger) error {
	return nil // TODO(retention)
}
