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

// hypertables carry the RETENTION_DAYS / COMPRESS_AFTER policies; the
// value is the time column (for the plain-Postgres sweep).
var hypertables = map[string]string{"events": "occurred_at", "sessions": "started_at"}

// aggregates keep AggregateRetentionDays of buckets.
var aggregates = []string{"event_stats_hourly", "issue_stats_hourly", "release_health_daily"}

// Reconcile (re)creates the compression and retention policies so they
// match RETENTION_DAYS / COMPRESS_AFTER. Idempotent; run at startup.
func Reconcile(ctx context.Context, st *store.Store, cfg config.Config, log *slog.Logger) error {
	if st.Plain {
		log.Info("retention: plain Postgres — no policies; the sweep deletes by time range")
		return nil
	}
	days := cfg.RetentionDays
	if days < 1 {
		days = 30
	}
	dropAfter := interval(time.Duration(days) * 24 * time.Hour)
	compressAfter := cfg.CompressAfter
	if compressAfter <= 0 {
		compressAfter = 48 * time.Hour
	}
	for t := range hypertables {
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

	if st.Plain {
		cutoff := now.Add(-retention)
		for t, col := range hypertables {
			n, err := deleteBefore(ctx, st, t, col, cutoff)
			if err != nil {
				return fmt.Errorf("expire %s: %w", t, err)
			}
			log.Info("retention: expired", "table", t, "rows", n)
		}
		aggCutoff := now.Add(-AggregateRetentionDays * 24 * time.Hour)
		for _, a := range aggregates {
			if _, err := st.Pool.Exec(ctx, "DELETE FROM "+a+"_rolled WHERE bucket < $1", aggCutoff); err != nil {
				return fmt.Errorf("expire %s: %w", a, err)
			}
		}
	}
	issues, err := st.ExpireIssues(ctx, now.Add(-retention))
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
	if st.Plain {
		return RollupAll(ctx, st, time.Now())
	}
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

// deleteBefore removes rows with col < cutoff in batches of 5000 (a
// bounded transaction each, so a big backlog does not lock the table).
func deleteBefore(ctx context.Context, st *store.Store, table, col string, cutoff time.Time) (int64, error) {
	var total int64
	for {
		tag, err := st.Pool.Exec(ctx, fmt.Sprintf(`DELETE FROM %s WHERE ctid IN (SELECT ctid FROM %s WHERE %s < $1 LIMIT 5000)`, table, table, col), cutoff)
		if err != nil {
			return total, err
		}
		total += tag.RowsAffected()
		if tag.RowsAffected() < 5000 {
			return total, nil
		}
	}
}

// ── plain-Postgres rollup (the stand-in for continuous aggregates) ──────

// RollupLookback is how many complete hours RollupRecent re-rolls each run
// (late events, a missed run).
const RollupLookback = 3

// Rollup recomputes the *_rolled stats for [from, to), widened to whole
// hour / day buckets (UTC) and clamped so the current hour is never rolled.
// The watermark (where rolled data ends; the views compute everything after
// it live) moves forward to the end of the range. Idempotent: delete +
// insert per range, in one transaction.
func Rollup(ctx context.Context, st *store.Store, from, to time.Time, now time.Time) error {
	hourStart := now.UTC().Truncate(time.Hour)
	if to.After(hourStart) {
		to = hourStart
	}
	if !to.After(from) {
		return nil
	}
	const day = 24 * time.Hour
	h0 := from.UTC().Truncate(time.Hour)
	h1 := to.Add(-time.Nanosecond).UTC().Truncate(time.Hour).Add(time.Hour)
	d0 := from.UTC().Truncate(day)
	d1 := to.Add(-time.Nanosecond).UTC().Truncate(day).Add(day)
	tx, err := st.Pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(context.WithoutCancel(ctx))
	stmts := []struct {
		sql  string
		args []any
	}{
		{"DELETE FROM event_stats_hourly_rolled WHERE bucket >= $1 AND bucket < $2", []any{h0, h1}},
		{`INSERT INTO event_stats_hourly_rolled (bucket, project_id, release, platform, level, events, crashes, errors)
		  SELECT date_trunc('hour', occurred_at, 'UTC'), project_id, COALESCE(release, ''), COALESCE(platform, ''), level,
		         count(*), count(*) FILTER (WHERE crashcart_is_crash(level, handled)),
		         count(*) FILTER (WHERE level = 'error' AND handled IS NOT false)
		  FROM events WHERE occurred_at >= $1 AND occurred_at < $2 GROUP BY 1, 2, 3, 4, 5`, []any{h0, h1}},
		{"DELETE FROM issue_stats_hourly_rolled WHERE bucket >= $1 AND bucket < $2", []any{h0, h1}},
		{`INSERT INTO issue_stats_hourly_rolled (bucket, project_id, fingerprint, events)
		  SELECT date_trunc('hour', occurred_at, 'UTC'), project_id, fingerprint, count(*)
		  FROM events WHERE occurred_at >= $1 AND occurred_at < $2 AND fingerprint IS NOT NULL GROUP BY 1, 2, 3`, []any{h0, h1}},
		{"DELETE FROM release_health_daily_rolled WHERE bucket >= $1 AND bucket < $2", []any{d0, d1}},
		{`INSERT INTO release_health_daily_rolled (bucket, project_id, release, total, crashed, errored)
		  SELECT date_trunc('day', started_at, 'UTC'), project_id, release, sum(count),
		         COALESCE(sum(count) FILTER (WHERE status = 'crashed'), 0),
		         COALESCE(sum(count) FILTER (WHERE status IN ('errored', 'abnormal')), 0)
		  FROM sessions WHERE started_at >= $1 AND started_at < $2 GROUP BY 1, 2, 3`, []any{d0, h1}},
		{`INSERT INTO stats_rollup (id, watermark) VALUES (true, $1)
		  ON CONFLICT (id) DO UPDATE SET watermark = GREATEST(stats_rollup.watermark, EXCLUDED.watermark)`, []any{h1}},
	}
	for _, s := range stmts {
		if _, err := tx.Exec(ctx, s.sql, s.args...); err != nil {
			return err
		}
	}
	return tx.Commit(ctx)
}

// RollupRecent re-rolls the last RollupLookback complete hours (the scheduler).
func RollupRecent(ctx context.Context, st *store.Store, now time.Time) error {
	to := now.UTC().Truncate(time.Hour)
	return Rollup(ctx, st, to.Add(-RollupLookback*time.Hour), to, now)
}

// RollupAll rebuilds the stats from the oldest stored event / session —
// after an import or seed, or to repair.
func RollupAll(ctx context.Context, st *store.Store, now time.Time) error {
	var from *time.Time
	if err := st.Pool.QueryRow(ctx, "SELECT least((SELECT min(occurred_at) FROM events), (SELECT min(started_at) FROM sessions))").Scan(&from); err != nil {
		return err
	}
	if from == nil {
		return nil
	}
	return Rollup(ctx, st, *from, now, now)
}
