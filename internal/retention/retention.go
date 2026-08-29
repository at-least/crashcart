// Package retention reconciles TimescaleDB policies from the configuration
// and sweeps the non-hypertable tables.
package retention

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/crashcartapp/crashcart/internal/config"
	"github.com/crashcartapp/crashcart/internal/store"
)

// AggregateRetentionDays is how long the continuous aggregates keep
// history, independent of RETENTION_DAYS.
const AggregateRetentionDays = 400

// hypertables carry the RETENTION_DAYS / COMPRESS_AFTER policies.
var hypertables = []string{"events", "sessions"}

// aggregates keep AggregateRetentionDays of buckets.
var aggregates = []string{"event_stats_hourly", "issue_stats_hourly", "release_health_daily"}

// Reconcile (re)creates the compression and retention policies so they
// match RETENTION_DAYS / COMPRESS_AFTER. Idempotent; run at startup.
func Reconcile(ctx context.Context, st *store.Store, cfg config.Config, log *slog.Logger) error {
	days := cfg.RetentionDays
	if days < 1 {
		days = 30
	}
	dropAfter := interval(time.Duration(days) * 24 * time.Hour)
	compressAfter := cfg.CompressAfter
	if compressAfter <= 0 {
		compressAfter = 48 * time.Hour
	}
	for _, t := range hypertables {
		if _, err := st.Pool.Exec(ctx, fmt.Sprintf("SELECT remove_retention_policy('%s', if_exists => true)", t)); err != nil {
			return fmt.Errorf("remove retention policy %s: %w", t, err)
		}
		if _, err := st.Pool.Exec(ctx, fmt.Sprintf("SELECT add_retention_policy('%s', $1::interval)", t), dropAfter); err != nil {
			return fmt.Errorf("add retention policy %s: %w", t, err)
		}
		if _, err := st.Pool.Exec(ctx, fmt.Sprintf("SELECT remove_compression_policy('%s', if_exists => true)", t)); err != nil {
			return fmt.Errorf("remove compression policy %s: %w", t, err)
		}
		if _, err := st.Pool.Exec(ctx, fmt.Sprintf("SELECT add_compression_policy('%s', $1::interval)", t), interval(compressAfter)); err != nil {
			return fmt.Errorf("add compression policy %s: %w", t, err)
		}
		log.Info("retention: policies set", "table", t, "retention_days", days, "compress_after", compressAfter)
	}
	aggAfter := interval(AggregateRetentionDays * 24 * time.Hour)
	for _, a := range aggregates {
		if _, err := st.Pool.Exec(ctx, fmt.Sprintf("SELECT add_retention_policy('%s', $1::interval, if_not_exists => true)", a), aggAfter); err != nil {
			return fmt.Errorf("add retention policy %s: %w", a, err)
		}
		log.Info("retention: policy set", "aggregate", a, "retention_days", AggregateRetentionDays)
	}
	return nil
}

// Sweep expires issues, jobs, upload chunks and symbol files.
func Sweep(ctx context.Context, st *store.Store, cfg config.Config, log *slog.Logger) error {
	days := cfg.RetentionDays
	if days < 1 {
		days = 30
	}
	now := time.Now()
	retention := time.Duration(days) * 24 * time.Hour

	issues, err := st.ExpireIssues(ctx, now.Add(-retention))
	if err != nil {
		return fmt.Errorf("expire issues: %w", err)
	}
	jobs, err := st.ExpireJobs(ctx)
	if err != nil {
		return fmt.Errorf("expire jobs: %w", err)
	}
	if _, err := st.ExpireProjectUsage(ctx, now.Add(-retention)); err != nil {
		return fmt.Errorf("expire project usage: %w", err)
	}
	chunks, err := st.ExpireUploadChunks(ctx, now.Add(-24*time.Hour))
	if err != nil {
		return fmt.Errorf("expire upload chunks: %w", err)
	}
	log.Info("retention: upload chunks expired", "rows", chunks)
	symbols, err := st.ExpireSymbolFiles(ctx, now.Add(-2*retention))
	if err != nil {
		return fmt.Errorf("expire symbol files: %w", err)
	}
	log.Info("retention: sweep", "issues", issues, "jobs", jobs, "symbol_files", symbols)
	return nil
}

// RefreshAggregates materializes the continuous aggregates over their whole
// range. The policies only refresh the recent window and real-time
// aggregation covers only what lies past the watermark, so history written
// in bulk — `crashcart import`, `crashcart seed` — is invisible in the stats
// until this runs. Must not be called inside a transaction.
func RefreshAggregates(ctx context.Context, st *store.Store) error {
	for _, a := range aggregates {
		if err := refreshAggregate(ctx, st, a); err != nil {
			return fmt.Errorf("refresh %s: %w", a, err)
		}
	}
	return nil
}

// refreshAggregate retries while the aggregate's own policy job holds the
// refresh lock (SQLSTATE 55P03, "concurrent refresh").
func refreshAggregate(ctx context.Context, st *store.Store, name string) error {
	var err error
	for attempt := 0; attempt < 20; attempt++ {
		_, err = st.Pool.Exec(ctx, fmt.Sprintf("CALL refresh_continuous_aggregate('%s', NULL, NULL)", name))
		var pg *pgconn.PgError
		if err == nil || !errors.As(err, &pg) || pg.Code != "55P03" {
			return err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(500 * time.Millisecond):
		}
	}
	return err
}

// interval renders d as a Postgres interval literal (days / hours when
// whole, so the policy config reads naturally).
func interval(d time.Duration) string {
	switch {
	case d%(24*time.Hour) == 0:
		return fmt.Sprintf("%d days", int64(d/(24*time.Hour)))
	case d%time.Hour == 0:
		return fmt.Sprintf("%d hours", int64(d/time.Hour))
	}
	return fmt.Sprintf("%d seconds", int64(d.Seconds()))
}
