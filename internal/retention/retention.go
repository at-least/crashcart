// Package retention keeps the time-series tables partitioned and bounded,
// rolls the statistics up and sweeps the non-partitioned tables.
// Everything here is idempotent and runs on one replica at a time
// (store.RunAsLeader).
package retention

import (
	"context"
	"fmt"
	"log/slog"
	"regexp"
	"slices"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/at-least/crashcart/internal/config"
	"github.com/at-least/crashcart/internal/store"
)

// AggregateRetentionDays is how long the rollup tables keep history,
// independent of RETENTION_DAYS.
const AggregateRetentionDays = 400

// PartitionWidth is the width of one events / sessions / attachments partition (weeks,
// Monday-aligned). Retention drops whole partitions, so rows — payloads
// included — live up to one width longer than RETENTION_DAYS.
const PartitionWidth = 7 * 24 * time.Hour

// partitionsAhead is how far into the future partitions exist, so a
// replica that missed a few sweeps still writes into real partitions.
const partitionsAhead = 2 * PartitionWidth

// partitioned lists the tables and their partition key.
var partitioned = []struct{ table, column string }{{"events", "occurred_at"}, {"attachments", "occurred_at"}, {"sessions", "started_at"}, {"monitor_checkins", "started_at"}}

// rolled lists the rollup tables (bounded by AggregateRetentionDays).
var rolled = []string{"event_stats_hourly_rolled", "issue_stats_hourly_rolled", "release_health_hourly_rolled", "client_report_counts"}

// Reconcile runs at startup: partitions for the retention window plus
// partitionsAhead exist. Idempotent.
func Reconcile(ctx context.Context, st *store.Store, cfg config.Config, log *slog.Logger) error {
	if err := EnsurePartitions(ctx, st, cfg, time.Now()); err != nil {
		return err
	}
	log.Info("retention: reconciled", "retention", cfg.Retention(), "partition_width", PartitionWidth)
	return nil
}

// ── partitions ─────────────────────────────────────────────────────────

var partitionName = regexp.MustCompile(`^(events|attachments|sessions|monitor_checkins)_p(\d{8})$`)

// weekStart is the Monday 00:00 UTC on or before t.
func weekStart(t time.Time) time.Time {
	t = t.UTC().Truncate(24 * time.Hour)
	return t.AddDate(0, 0, -(int(t.Weekday())+6)%7)
}

// EnsurePartitions creates the weekly partitions from the week holding
// the start of the retention window (nothing older can be written: ingest
// clamps a time before the window to now) to partitionsAhead past now:
// one catalog listing per table, then only the missing weeks. A week
// earlier than that would be dropped again by the same sweep. A week whose rows already sit in the DEFAULT partition —
// written while no partition covered it — gets them moved into the new
// partition in the same transaction.
func EnsurePartitions(ctx context.Context, st *store.Store, cfg config.Config, now time.Time) error {
	from := weekStart(now.Add(-cfg.Retention()))
	to := weekStart(now.Add(partitionsAhead))
	for _, t := range partitioned {
		have, err := Partitions(ctx, st, t.table)
		if err != nil {
			return err
		}
		for start := from; !start.After(to); start = start.Add(PartitionWidth) {
			if slices.ContainsFunc(have, func(p Partition) bool { return p.Start.Equal(start) }) {
				continue
			}
			if err := ensurePartition(ctx, st, t.table, t.column, start); err != nil {
				return err
			}
		}
	}
	return nil
}

func ensurePartition(ctx context.Context, st *store.Store, table, column string, start time.Time) error {
	name := fmt.Sprintf("%s_p%s", table, start.Format("20060102"))
	end := start.Add(PartitionWidth)
	return pgx.BeginFunc(ctx, st.Pool, func(tx pgx.Tx) error {
		// Replicas start (and sweep) together: one creates the partition
		// at a time, the others find it once they hold the lock.
		if _, err := tx.Exec(ctx, "SELECT pg_advisory_xact_lock($1)", store.LeaderPartitions); err != nil {
			return err
		}
		// A catalog scan, not to_regclass: inside a transaction the
		// name lookup can answer from a catalog snapshot taken before
		// the other replica's CREATE TABLE committed.
		var exists bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace
			WHERE c.relname = $1 AND n.nspname = current_schema())`, name).Scan(&exists); err != nil {
			return err
		}
		if exists {
			return nil
		}
		def := table + "_default"
		// Nothing lands in the default partition while its rows for this
		// week move out and the partition is attached.
		if _, err := tx.Exec(ctx, "LOCK TABLE "+def+" IN ACCESS EXCLUSIVE MODE"); err != nil {
			return err
		}
		var stray int64
		if err := tx.QueryRow(ctx, fmt.Sprintf("SELECT count(*) FROM %s WHERE %s >= $1 AND %s < $2", def, column, column), start, end).Scan(&stray); err != nil {
			return err
		}
		bounds := fmt.Sprintf("FOR VALUES FROM ('%s') TO ('%s')", start.Format(time.RFC3339), end.Format(time.RFC3339))
		if stray == 0 {
			_, err := tx.Exec(ctx, fmt.Sprintf("CREATE TABLE %s PARTITION OF %s %s", name, table, bounds))
			return err
		}
		// Create standalone, move the rows, attach (attach checks the
		// default partition, which is empty for this range by then).
		if _, err := tx.Exec(ctx, fmt.Sprintf("CREATE TABLE %s (LIKE %s INCLUDING DEFAULTS INCLUDING CONSTRAINTS INCLUDING STORAGE)", name, table)); err != nil {
			return fmt.Errorf("create: %w", err)
		}
		if _, err := tx.Exec(ctx, fmt.Sprintf("INSERT INTO %s SELECT * FROM %s WHERE %s >= $1 AND %s < $2", name, def, column, column), start, end); err != nil {
			return fmt.Errorf("move: %w", err)
		}
		if _, err := tx.Exec(ctx, fmt.Sprintf("DELETE FROM %s WHERE %s >= $1 AND %s < $2", def, column, column), start, end); err != nil {
			return fmt.Errorf("delete: %w", err)
		}
		if _, err := tx.Exec(ctx, fmt.Sprintf("ALTER TABLE %s ATTACH PARTITION %s %s", table, name, bounds)); err != nil {
			return fmt.Errorf("attach: %w", err)
		}
		return nil
	})
}

// Partition is one weekly partition of events / attachments / sessions.
type Partition struct {
	Name  string
	Start time.Time
}

// Partitions lists the weekly partitions of table, oldest first.
func Partitions(ctx context.Context, st *store.Store, table string) ([]Partition, error) {
	rows, err := st.Pool.Query(ctx, `SELECT c.relname FROM pg_inherits i
		JOIN pg_class c ON c.oid = i.inhrelid JOIN pg_class p ON p.oid = i.inhparent
		JOIN pg_namespace n ON n.oid = p.relnamespace
		WHERE p.relname = $1 AND n.nspname = current_schema() ORDER BY c.relname`, table)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Partition
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, err
		}
		m := partitionName.FindStringSubmatch(name)
		if m == nil {
			continue
		}
		start, err := time.Parse("20060102", m[2])
		if err != nil {
			continue
		}
		out = append(out, Partition{name, start})
	}
	return out, rows.Err()
}

// dropExpiredPartitions drops the partitions that end before the
// retention cutoff and deletes the default partition's rows older than it.
func dropExpiredPartitions(ctx context.Context, st *store.Store, cfg config.Config, now time.Time, log *slog.Logger) error {
	cutoff := now.Add(-cfg.Retention())
	for _, t := range partitioned {
		parts, err := Partitions(ctx, st, t.table)
		if err != nil {
			return err
		}
		for _, p := range parts {
			if p.Start.Add(PartitionWidth).After(cutoff) {
				continue
			}
			if _, err := st.Pool.Exec(ctx, "DROP TABLE IF EXISTS "+p.Name); err != nil {
				return fmt.Errorf("drop %s: %w", p.Name, err)
			}
			log.Info("retention: partition dropped", "table", p.Name)
		}
		if _, err := st.Pool.Exec(ctx, fmt.Sprintf("DELETE FROM %s_default WHERE %s < $1", t.table, t.column), cutoff); err != nil {
			return fmt.Errorf("expire %s_default: %w", t.table, err)
		}
	}
	return nil
}

// ── sweep ──────────────────────────────────────────────────────────────

// Sweep runs hourly: partitions (create ahead, drop expired), then the
// row-level expiries — issues, jobs, usage counters, user sessions, upload
// chunks, symbol files and rollup history past AggregateRetentionDays.
func Sweep(ctx context.Context, st *store.Store, cfg config.Config, log *slog.Logger) error {
	now := time.Now()
	retention := cfg.Retention()
	if err := EnsurePartitions(ctx, st, cfg, now); err != nil {
		return fmt.Errorf("partitions: %w", err)
	}
	if err := dropExpiredPartitions(ctx, st, cfg, now, log); err != nil {
		return err
	}
	issues, err := ExpireIssues(ctx, st, cfg, now)
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
	if _, err := st.ExpireUploadChunks(ctx, now.Add(-24*time.Hour)); err != nil {
		return fmt.Errorf("expire upload chunks: %w", err)
	}
	symbols, err := st.ExpireSymbolFiles(ctx, now.Add(-2*retention))
	if err != nil {
		return fmt.Errorf("expire symbol files: %w", err)
	}
	// user_reports has no partition of its own to ride out with events (see
	// schema.sql): its own cutoff, on the same retention window.
	if _, err := st.SweepUserReports(ctx, now.Add(-retention)); err != nil {
		return fmt.Errorf("expire user reports: %w", err)
	}
	for _, t := range rolled {
		if _, err := st.Pool.Exec(ctx, fmt.Sprintf("DELETE FROM %s WHERE bucket < $1", t), now.Add(-AggregateRetentionDays*24*time.Hour)); err != nil {
			return fmt.Errorf("expire %s: %w", t, err)
		}
	}
	log.Info("retention: sweep", "issues", issues, "jobs", jobs, "symbol_files", symbols)
	return nil
}

// ── issues ─────────────────────────────────────────────────────────────

// StaleIssueFactor: an issue nobody resolved or ignored is dropped once
// its last event is StaleIssueFactor × RETENTION_DAYS old — its events are
// long gone, its sparkline empty, and the table it sits in is scanned by
// every issue list.
const StaleIssueFactor = 4

const expireIssuesSQL = `WITH gone AS (
	DELETE FROM issues WHERE last_seen < $1 AND status IN ('resolved', 'ignored')
	RETURNING project_id, fingerprint),
stale AS (
	DELETE FROM issues WHERE last_seen < $2
	RETURNING project_id, fingerprint),
all_gone AS (SELECT * FROM gone UNION ALL SELECT * FROM stale),
rolled AS (
	DELETE FROM issue_stats_hourly_rolled r USING all_gone g
	WHERE r.project_id = g.project_id AND r.fingerprint = g.fingerprint)
SELECT (SELECT count(*) FROM gone), (SELECT count(*) FROM stale)`

// ExpireIssues deletes resolved / ignored issues whose events are gone —
// the cutoff is the partition boundary, so an issue never outlives its
// events the other way round: a deleted issue whose events still listed
// would be re-created (first_seen = now, a new_issue alert) by the next
// event — and issues of any status last seen StaleIssueFactor retentions
// ago. Their per-issue rollup rows go with them. Returns how many.
func ExpireIssues(ctx context.Context, st *store.Store, cfg config.Config, now time.Time) (int64, error) {
	retention := cfg.Retention()
	resolvedBefore := weekStart(now.Add(-retention)) // the oldest surviving partition starts here
	staleBefore := now.Add(-StaleIssueFactor * retention)
	var resolved, stale int64
	if err := st.Pool.QueryRow(ctx, expireIssuesSQL, resolvedBefore, staleBefore).Scan(&resolved, &stale); err != nil {
		return 0, err
	}
	return resolved + stale, nil
}

// ── rollup ─────────────────────────────────────────────────────────────

// RollupBatch bounds the dirty hours one Rollup pass recomputes.
const RollupBatch = 500

// Rollup recomputes up to RollupBatch dirty hours of the events and
// sessions statistics from the raw rows and clears their keys. The
// current hour is left dirty (it is marked again by every envelope and
// the views compute it live anyway). Returns how many keys it processed;
// 0 means the rollups are current.
//
// Three steps, so no ingest transaction ever waits on this one: read the
// keys with their gen (no lock), recompute in one transaction (locks only
// rollup rows), then delete each key only if its gen is still what was
// read — a mark that landed meanwhile keeps the key for the next pass.
//
// An hour older than RETENTION_DAYS is never recomputed: its raw rows are
// gone (or an event that arrived with a clock far in the past is the only
// one, and is about to be swept), and the rolled row — which outlives the
// raw rows by design — is the only record of it. Such a key is just
// cleared.
func Rollup(ctx context.Context, st *store.Store, cfg config.Config) (int, error) {
	cutoff := time.Now().Add(-cfg.Retention()).Truncate(time.Hour)
	n, err := rollup(ctx, st, "event_stats_dirty", cutoff, rollupEvents)
	if err != nil {
		return n, err
	}
	m, err := rollup(ctx, st, "session_stats_dirty", cutoff, rollupSessions)
	return n + m, err
}

// RollupAll runs Rollup until nothing but the current hour is dirty
// (after an import or seed).
func RollupAll(ctx context.Context, st *store.Store, cfg config.Config) error {
	for {
		n, err := Rollup(ctx, st, cfg)
		if err != nil || n == 0 {
			return err
		}
	}
}

type dirtyKey struct {
	project int64
	bucket  time.Time
	gen     int64
}

func rollup(ctx context.Context, st *store.Store, dirtyTable string, cutoff time.Time, recompute func(ctx context.Context, tx pgx.Tx, pids []int64, buckets []time.Time, lo, hi time.Time) error) (int, error) {
	// Newest first: with a backlog (an import), the hours the viewer is
	// looking at become clean first.
	rows, err := st.Pool.Query(ctx, fmt.Sprintf(`SELECT project_id, bucket, gen FROM %s
		WHERE bucket < date_trunc('hour', now()) ORDER BY bucket DESC LIMIT $1`, dirtyTable), RollupBatch)
	if err != nil {
		return 0, err
	}
	keys, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (dirtyKey, error) {
		var k dirtyKey
		err := r.Scan(&k.project, &k.bucket, &k.gen)
		return k, err
	})
	if err != nil || len(keys) == 0 {
		return 0, err
	}
	var pids []int64
	var buckets []time.Time
	allP := make([]int64, len(keys))
	allB := make([]time.Time, len(keys))
	allG := make([]int64, len(keys))
	for i, k := range keys {
		allP[i], allB[i], allG[i] = k.project, k.bucket, k.gen
		if !k.bucket.Before(cutoff) {
			pids = append(pids, k.project)
			buckets = append(buckets, k.bucket)
		}
	}
	if len(pids) > 0 {
		// The batch's time range as constants, so the planner prunes the
		// raw table to those partitions whatever join it picks.
		lo, hi := buckets[0], buckets[0]
		for _, b := range buckets {
			if b.Before(lo) {
				lo = b
			}
			if b.After(hi) {
				hi = b
			}
		}
		if err := pgx.BeginFunc(ctx, st.Pool, func(tx pgx.Tx) error { return recompute(ctx, tx, pids, buckets, lo, hi.Add(time.Hour)) }); err != nil {
			return 0, fmt.Errorf("rollup %s: %w", dirtyTable, err)
		}
	}
	if _, err := st.Pool.Exec(ctx, fmt.Sprintf(`DELETE FROM %s d USING unnest($1::bigint[], $2::timestamptz[], $3::bigint[]) AS k(project_id, bucket, gen)
		WHERE d.project_id = k.project_id AND d.bucket = k.bucket AND d.gen = k.gen`, dirtyTable), allP, allB, allG); err != nil {
		return 0, err
	}
	return len(keys), nil
}

// exec runs the statements in order, each with its own arguments.
func exec(ctx context.Context, tx pgx.Tx, stmts ...stmt) error {
	for _, s := range stmts {
		if _, err := tx.Exec(ctx, s.sql, s.args...); err != nil {
			return err
		}
	}
	return nil
}

type stmt struct {
	sql  string
	args []any
}

func rollupEvents(ctx context.Context, tx pgx.Tx, pids []int64, buckets []time.Time, lo, hi time.Time) error {
	keys, window := []any{pids, buckets}, []any{pids, buckets, lo, hi}
	return exec(ctx, tx,
		stmt{`DELETE FROM event_stats_hourly_rolled r USING unnest($1::bigint[], $2::timestamptz[]) AS k(project_id, bucket)
		 WHERE r.project_id = k.project_id AND r.bucket = k.bucket`, keys},
		stmt{`INSERT INTO event_stats_hourly_rolled (bucket, project_id, release, platform, level, events, unhandled, errors)
		 SELECT k.bucket, k.project_id, COALESCE(e.release, ''), COALESCE(e.platform, ''), e.level,
		        count(*), count(*) FILTER (WHERE e.handled = false),
		        count(*) FILTER (WHERE e.level = 'error' AND e.handled IS NOT false)
		 FROM unnest($1::bigint[], $2::timestamptz[]) AS k(project_id, bucket)
		 JOIN events e ON e.project_id = k.project_id AND e.occurred_at >= k.bucket AND e.occurred_at < k.bucket + INTERVAL '1 hour'
		   AND e.occurred_at >= $3 AND e.occurred_at < $4
		 GROUP BY 1, 2, 3, 4, 5`, window},
		stmt{`DELETE FROM issue_stats_hourly_rolled r USING unnest($1::bigint[], $2::timestamptz[]) AS k(project_id, bucket)
		 WHERE r.project_id = k.project_id AND r.bucket = k.bucket`, keys},
		stmt{`INSERT INTO issue_stats_hourly_rolled (bucket, project_id, fingerprint, events)
		 SELECT k.bucket, k.project_id, e.fingerprint, count(*)
		 FROM unnest($1::bigint[], $2::timestamptz[]) AS k(project_id, bucket)
		 JOIN events e ON e.project_id = k.project_id AND e.occurred_at >= k.bucket AND e.occurred_at < k.bucket + INTERVAL '1 hour'
		   AND e.occurred_at >= $3 AND e.occurred_at < $4
		 WHERE e.fingerprint IS NOT NULL
		 GROUP BY 1, 2, 3`, window},
	)
}

func rollupSessions(ctx context.Context, tx pgx.Tx, pids []int64, buckets []time.Time, lo, hi time.Time) error {
	return exec(ctx, tx,
		stmt{`DELETE FROM release_health_hourly_rolled r USING unnest($1::bigint[], $2::timestamptz[]) AS k(project_id, bucket)
		 WHERE r.project_id = k.project_id AND r.bucket = k.bucket`, []any{pids, buckets}},
		stmt{`INSERT INTO release_health_hourly_rolled (bucket, project_id, release, total, crashed, errored)
		 SELECT k.bucket, k.project_id, s.release, sum(s.count),
		        COALESCE(sum(s.count) FILTER (WHERE s.status = 'crashed'), 0),
		        COALESCE(sum(s.count) FILTER (WHERE s.status IN ('errored', 'abnormal')), 0)
		 FROM unnest($1::bigint[], $2::timestamptz[]) AS k(project_id, bucket)
		 JOIN sessions s ON s.project_id = k.project_id AND s.started_at >= k.bucket AND s.started_at < k.bucket + INTERVAL '1 hour'
		   AND s.started_at >= $3 AND s.started_at < $4
		 GROUP BY 1, 2, 3`, []any{pids, buckets, lo, hi}},
	)
}
