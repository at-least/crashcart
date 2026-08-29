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

	"github.com/at-least/crashcart/internal/config"
	"github.com/at-least/crashcart/internal/pk"
	"github.com/at-least/crashcart/internal/store"
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
// Windows are id units (µs), the integer time dimension of the tables.
func Reconcile(ctx context.Context, st *store.Store, cfg config.Config, log *slog.Logger) error {
	days := cfg.RetentionDays
	if days < 1 {
		days = 30
	}
	dropAfter := int64(days) * pk.Day
	compressAfter := cfg.CompressAfter.Microseconds()
	if compressAfter <= 0 {
		compressAfter = 48 * pk.Hour
	}
	for _, t := range hypertables {
		if _, err := st.Pool.Exec(ctx, fmt.Sprintf("SELECT remove_retention_policy('%s', if_exists => true)", t)); err != nil {
			return fmt.Errorf("remove retention policy %s: %w", t, err)
		}
		if _, err := st.Pool.Exec(ctx, fmt.Sprintf("SELECT add_retention_policy('%s', $1::bigint)", t), dropAfter); err != nil {
			return fmt.Errorf("add retention policy %s: %w", t, err)
		}
		if _, err := st.Pool.Exec(ctx, fmt.Sprintf("SELECT remove_compression_policy('%s', if_exists => true)", t)); err != nil {
			return fmt.Errorf("remove compression policy %s: %w", t, err)
		}
		if _, err := st.Pool.Exec(ctx, fmt.Sprintf("SELECT add_compression_policy('%s', $1::bigint)", t), compressAfter); err != nil {
			return fmt.Errorf("add compression policy %s: %w", t, err)
		}
		log.Info("retention: policies set", "table", t, "retention_days", days, "compress_after", cfg.CompressAfter)
	}
	aggAfter := int64(AggregateRetentionDays) * pk.Day
	for _, a := range aggregates {
		if _, err := st.Pool.Exec(ctx, fmt.Sprintf("SELECT add_retention_policy('%s', $1::bigint, if_not_exists => true)", a), aggAfter); err != nil {
			return fmt.Errorf("add retention policy %s: %w", a, err)
		}
		log.Info("retention: policy set", "aggregate", a, "retention_days", AggregateRetentionDays)
	}
	return nil
}

// Sweep expires issues, jobs, rate-limit windows and symbol files.
func Sweep(ctx context.Context, st *store.Store, cfg config.Config, log *slog.Logger) error {
	days := cfg.RetentionDays
	if days < 1 {
		days = 30
	}
	now := time.Now()
	retention := time.Duration(days) * 24 * time.Hour

	issues, err := st.ExpireIssues(ctx, pk.Lower(now.Add(-retention)))
	if err != nil {
		return fmt.Errorf("expire issues: %w", err)
	}
	jobs, err := st.ExpireJobs(ctx)
	if err != nil {
		return fmt.Errorf("expire jobs: %w", err)
	}
	chunks, err := st.ExpireUploadChunks(ctx, now.Add(-24*time.Hour))
	if err != nil {
		return fmt.Errorf("expire upload chunks: %w", err)
	}
	log.Info("retention: upload chunks expired", "rows", chunks)
	limits, err := st.ExpireRateLimits(ctx, now.Add(-120*time.Second).Unix())
	if err != nil {
		return fmt.Errorf("expire rate limits: %w", err)
	}
	symbols, err := st.ExpireSymbolFiles(ctx, now.Add(-2*retention))
	if err != nil {
		return fmt.Errorf("expire symbol files: %w", err)
	}
	log.Info("retention: sweep", "issues", issues, "jobs", jobs, "rate_limits", limits, "symbol_files", symbols)
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
