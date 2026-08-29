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

// AggregateCompressAfter is the age at which aggregate chunks are
// compressed: well past the refresh windows, so the policies never write
// into compressed chunks (a whole-range refresh after import still can).
const AggregateCompressAfter = 30 * 24 * time.Hour

// hypertables carry the RETENTION_DAYS / COMPRESS_AFTER policies and the
// CHUNK_INTERVAL chunk width.
var hypertables = []string{"events", "sessions"}

// aggregates keep AggregateRetentionDays of buckets. In refresh order:
// event_stats_daily rolls up event_stats_hourly.
var aggregates = []string{"event_stats_hourly", "event_stats_daily", "issue_stats_hourly", "release_health_daily"}

// Reconcile sets the compression and retention policies of the hypertables
// and the aggregates, and the hypertables' chunk width, from RETENTION_DAYS
// / COMPRESS_AFTER / CHUNK_INTERVAL. An existing policy is altered in place
// (its job id and run statistics survive); the chunk width applies to new
// chunks only, so the setting can follow the traffic at any time.
// Idempotent; run at startup.
func Reconcile(ctx context.Context, st *store.Store, cfg config.Config, log *slog.Logger) error {
	days := cfg.RetentionDays
	if days < 1 {
		days = 30
	}
	compressAfter := cfg.CompressAfter
	if compressAfter <= 0 {
		compressAfter = 48 * time.Hour
	}
	chunk := cfg.ChunkInterval
	if chunk <= 0 {
		chunk = 7 * 24 * time.Hour
	}
	for _, t := range hypertables {
		if _, err := st.Pool.Exec(ctx, fmt.Sprintf("SELECT set_chunk_time_interval('%s', $1::interval)", t), interval(chunk)); err != nil {
			return fmt.Errorf("set chunk interval %s: %w", t, err)
		}
		if err := setPolicy(ctx, st, t, "retention", time.Duration(days)*24*time.Hour); err != nil {
			return err
		}
		if err := setPolicy(ctx, st, t, "compression", compressAfter); err != nil {
			return err
		}
		log.Info("retention: policies set", "table", t, "retention_days", days, "compress_after", compressAfter, "chunk_interval", chunk)
	}
	for _, a := range aggregates {
		if err := setPolicy(ctx, st, a, "retention", AggregateRetentionDays*24*time.Hour); err != nil {
			return err
		}
		if err := setPolicy(ctx, st, a, "compression", AggregateCompressAfter); err != nil {
			return err
		}
		log.Info("retention: policies set", "aggregate", a, "retention_days", AggregateRetentionDays, "compress_after", AggregateCompressAfter)
	}
	return nil
}

// policyWindowKey is the config key of each policy kind's window.
var policyWindowKey = map[string]string{"retention": "drop_after", "compression": "compress_after"}

// setPolicy alters the window of the existing retention / compression
// policy of a hypertable or aggregate, or adds the policy. The job is
// found by its procedure and the target's name (the jobs view lists an
// aggregate's policies under the aggregate's own name).
func setPolicy(ctx context.Context, st *store.Store, target, kind string, window time.Duration) error {
	tag, err := st.Pool.Exec(ctx, `SELECT alter_job(job_id, config => config || jsonb_build_object($3::text, $4::text))
		FROM timescaledb_information.jobs
		WHERE proc_name = $1 AND hypertable_schema = current_schema() AND hypertable_name = $2`,
		"policy_"+kind, target, policyWindowKey[kind], interval(window))
	if err == nil && tag.RowsAffected() == 0 {
		_, err = st.Pool.Exec(ctx, fmt.Sprintf("SELECT add_%s_policy('%s', $1::interval)", kind, target), interval(window))
	}
	if err != nil {
		return fmt.Errorf("%s policy %s: %w", kind, target, err)
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
	if _, err := st.ExpireUserSessions(ctx); err != nil {
		return fmt.Errorf("expire user sessions: %w", err)
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
