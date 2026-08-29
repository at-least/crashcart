// Package retention keeps the time-series tables partitioned and bounded,
// rolls the statistics up, sets the object store's lifecycle rules and
// sweeps the non-partitioned tables. Everything here is idempotent and
// runs on one replica at a time (store.RunAsLeader).
package retention

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"regexp"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/crashcartapp/crashcart/internal/blob"
	"github.com/crashcartapp/crashcart/internal/config"
	"github.com/crashcartapp/crashcart/internal/db/sqlc"
	"github.com/crashcartapp/crashcart/internal/store"
)

// AggregateRetentionDays is how long the rollup tables keep history,
// independent of RETENTION_DAYS.
const AggregateRetentionDays = 400

// PartitionWidth is the width of one events / sessions partition (weeks,
// Monday-aligned). Retention drops whole partitions, so rows live up to
// one width longer than RETENTION_DAYS; the object store's lifecycle rule
// for payloads is RETENTION_DAYS plus this, so a payload never expires
// before its row.
const PartitionWidth = 7 * 24 * time.Hour

// partitionsAhead is how far into the future partitions exist, so a
// replica that missed a few sweeps still writes into real partitions.
const partitionsAhead = 2 * PartitionWidth

// partitioned lists the tables and their partition key.
var partitioned = []struct{ table, column string }{{"events", "occurred_at"}, {"sessions", "started_at"}}

// rolled lists the rollup tables (bounded by AggregateRetentionDays).
var rolled = []string{"event_stats_hourly_rolled", "issue_stats_hourly_rolled", "release_health_hourly_rolled"}

// Reconcile runs at startup: partitions for the retention window plus
// partitionsAhead exist, and the object store's lifecycle rules match
// RETENTION_DAYS. Idempotent.
func Reconcile(ctx context.Context, st *store.Store, cfg config.Config, log *slog.Logger) error {
	if err := EnsurePartitions(ctx, st, cfg, time.Now()); err != nil {
		return err
	}
	if err := reconcileLifecycle(ctx, st, cfg, log); err != nil {
		// Not fatal: a bucket policy the credentials may not set is
		// something the operator can do by hand — the rules are logged.
		log.Error("retention: object-store lifecycle rules not set (set them on the bucket by hand)", "err", err, "rules", lifecycleRules(cfg))
	}
	log.Info("retention: reconciled", "retention_days", days(cfg), "partition_width", PartitionWidth)
	return nil
}

// lifecycleRules are the bucket's expiration rules for RETENTION_DAYS:
// payloads outlive their partition, symbol files match ExpireSymbolFiles,
// chunks never assembled go after a day.
func lifecycleRules(cfg config.Config) []blob.LifecycleRule {
	d := days(cfg)
	return []blob.LifecycleRule{
		{ID: "crashcart-events", Prefix: blob.PrefixEvents, Days: d + int(PartitionWidth/(24*time.Hour))},
		{ID: "crashcart-symbols", Prefix: blob.PrefixSymbols, Days: 2 * d},
		{ID: "crashcart-chunks", Prefix: blob.PrefixChunks, Days: 1},
	}
}

// bucketAdmin is what an object store must offer for Reconcile to manage
// the bucket (blob.S3 does; the in-memory test store does not).
type bucketAdmin interface {
	EnsureBucket(ctx context.Context) error
	SetLifecycle(ctx context.Context, rules []blob.LifecycleRule) error
}

func reconcileLifecycle(ctx context.Context, st *store.Store, cfg config.Config, log *slog.Logger) error {
	admin, ok := st.Blobs.(bucketAdmin)
	if !ok {
		return nil
	}
	if err := admin.EnsureBucket(ctx); err != nil {
		return err
	}
	return admin.SetLifecycle(ctx, lifecycleRules(cfg))
}

func days(cfg config.Config) int {
	if cfg.RetentionDays < 1 {
		return 30
	}
	return cfg.RetentionDays
}

// ── partitions ─────────────────────────────────────────────────────────

var partitionName = regexp.MustCompile(`^(events|sessions)_p(\d{8})$`)

// weekStart is the Monday 00:00 UTC on or before t.
func weekStart(t time.Time) time.Time {
	t = t.UTC().Truncate(24 * time.Hour)
	return t.AddDate(0, 0, -(int(t.Weekday())+6)%7)
}

// EnsurePartitions creates the weekly partitions from the start of the
// retention window (one width earlier, so a late event still has one) to
// partitionsAhead past now. A week whose rows already sit in the DEFAULT
// partition — written while no partition covered it — gets them moved
// into the new partition in the same transaction.
func EnsurePartitions(ctx context.Context, st *store.Store, cfg config.Config, now time.Time) error {
	from := weekStart(now.Add(-time.Duration(days(cfg))*24*time.Hour - PartitionWidth))
	to := weekStart(now.Add(partitionsAhead))
	for start := from; !start.After(to); start = start.Add(PartitionWidth) {
		for _, t := range partitioned {
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
	var exists bool
	if err := st.Pool.QueryRow(ctx, "SELECT to_regclass($1) IS NOT NULL", name).Scan(&exists); err != nil {
		return err
	}
	if exists {
		return nil
	}
	return pgx.BeginFunc(ctx, st.Pool, func(tx pgx.Tx) error {
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
		for _, q := range []string{
			fmt.Sprintf("CREATE TABLE %s (LIKE %s INCLUDING DEFAULTS INCLUDING CONSTRAINTS)", name, table),
			fmt.Sprintf("INSERT INTO %s SELECT * FROM %s WHERE %s >= $1 AND %s < $2", name, def, column, column),
			fmt.Sprintf("DELETE FROM %s WHERE %s >= $1 AND %s < $2", def, column, column),
			fmt.Sprintf("ALTER TABLE %s ATTACH PARTITION %s %s", table, name, bounds),
		} {
			var err error
			if strings.Contains(q, "$1") {
				_, err = tx.Exec(ctx, q, start, end)
			} else {
				_, err = tx.Exec(ctx, q)
			}
			if err != nil {
				return fmt.Errorf("%s: %w", strings.Fields(q)[0], err)
			}
		}
		return nil
	})
}

// Partitions lists the weekly partitions of table (name → start), oldest first.
func Partitions(ctx context.Context, st *store.Store, table string) ([]struct {
	Name  string
	Start time.Time
}, error) {
	rows, err := st.Pool.Query(ctx, `SELECT c.relname FROM pg_inherits i
		JOIN pg_class c ON c.oid = i.inhrelid JOIN pg_class p ON p.oid = i.inhparent
		JOIN pg_namespace n ON n.oid = p.relnamespace
		WHERE p.relname = $1 AND n.nspname = current_schema() ORDER BY c.relname`, table)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []struct {
		Name  string
		Start time.Time
	}
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
		out = append(out, struct {
			Name  string
			Start time.Time
		}{name, start})
	}
	return out, rows.Err()
}

// dropExpiredPartitions drops the partitions that end before the
// retention cutoff and deletes the default partition's rows older than it.
func dropExpiredPartitions(ctx context.Context, st *store.Store, cfg config.Config, now time.Time, log *slog.Logger) error {
	cutoff := now.Add(-time.Duration(days(cfg)) * 24 * time.Hour)
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
// row-level expiries — issues, jobs, usage counters, user sessions, symbol
// files (their objects expire by lifecycle rule on the same schedule) and
// rollup history past AggregateRetentionDays.
func Sweep(ctx context.Context, st *store.Store, cfg config.Config, log *slog.Logger) error {
	now := time.Now()
	retention := time.Duration(days(cfg)) * 24 * time.Hour
	if err := EnsurePartitions(ctx, st, cfg, now); err != nil {
		return fmt.Errorf("partitions: %w", err)
	}
	if err := dropExpiredPartitions(ctx, st, cfg, now, log); err != nil {
		return err
	}
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
	symbols, err := st.ExpireSymbolFiles(ctx, now.Add(-2*retention))
	if err != nil {
		return fmt.Errorf("expire symbol files: %w", err)
	}
	if _, err := st.ExpireSpool(ctx, now.Add(-retention)); err != nil {
		return fmt.Errorf("expire payload spool: %w", err)
	}
	for _, t := range rolled {
		if _, err := st.Pool.Exec(ctx, fmt.Sprintf("DELETE FROM %s WHERE bucket < $1", t), now.Add(-AggregateRetentionDays*24*time.Hour)); err != nil {
			return fmt.Errorf("expire %s: %w", t, err)
		}
	}
	log.Info("retention: sweep", "issues", issues, "jobs", jobs, "symbol_files", symbols)
	return nil
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
func Rollup(ctx context.Context, st *store.Store) (int, error) {
	n, err := rollup(ctx, st, "event_stats_dirty", rollupEvents)
	if err != nil {
		return n, err
	}
	m, err := rollup(ctx, st, "session_stats_dirty", rollupSessions)
	return n + m, err
}

// RollupAll runs Rollup until nothing but the current hour is dirty
// (after an import or seed).
func RollupAll(ctx context.Context, st *store.Store) error {
	for {
		n, err := Rollup(ctx, st)
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

func rollup(ctx context.Context, st *store.Store, dirtyTable string, recompute func(ctx context.Context, tx pgx.Tx, pids []int64, buckets []time.Time) error) (int, error) {
	rows, err := st.Pool.Query(ctx, fmt.Sprintf(`SELECT project_id, bucket, gen FROM %s
		WHERE bucket < date_trunc('hour', now()) ORDER BY bucket LIMIT $1`, dirtyTable), RollupBatch)
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
	pids := make([]int64, len(keys))
	buckets := make([]time.Time, len(keys))
	gens := make([]int64, len(keys))
	for i, k := range keys {
		pids[i], buckets[i], gens[i] = k.project, k.bucket, k.gen
	}
	if err := pgx.BeginFunc(ctx, st.Pool, func(tx pgx.Tx) error { return recompute(ctx, tx, pids, buckets) }); err != nil {
		return 0, fmt.Errorf("rollup %s: %w", dirtyTable, err)
	}
	if _, err := st.Pool.Exec(ctx, fmt.Sprintf(`DELETE FROM %s d USING unnest($1::bigint[], $2::timestamptz[], $3::bigint[]) AS k(project_id, bucket, gen)
		WHERE d.project_id = k.project_id AND d.bucket = k.bucket AND d.gen = k.gen`, dirtyTable), pids, buckets, gens); err != nil {
		return 0, err
	}
	return len(keys), nil
}

func rollupEvents(ctx context.Context, tx pgx.Tx, pids []int64, buckets []time.Time) error {
	for _, q := range []string{
		`DELETE FROM event_stats_hourly_rolled r USING unnest($1::bigint[], $2::timestamptz[]) AS k(project_id, bucket)
		 WHERE r.project_id = k.project_id AND r.bucket = k.bucket`,
		`INSERT INTO event_stats_hourly_rolled (bucket, project_id, release, platform, level, events, crashes, errors)
		 SELECT k.bucket, k.project_id, COALESCE(e.release, ''), COALESCE(e.platform, ''), e.level,
		        count(*), count(*) FILTER (WHERE crashcart_is_crash(e.level, e.handled)),
		        count(*) FILTER (WHERE e.level = 'error' AND e.handled IS NOT false)
		 FROM unnest($1::bigint[], $2::timestamptz[]) AS k(project_id, bucket)
		 JOIN events e ON e.project_id = k.project_id AND e.occurred_at >= k.bucket AND e.occurred_at < k.bucket + INTERVAL '1 hour'
		 GROUP BY 1, 2, 3, 4, 5`,
		`DELETE FROM issue_stats_hourly_rolled r USING unnest($1::bigint[], $2::timestamptz[]) AS k(project_id, bucket)
		 WHERE r.project_id = k.project_id AND r.bucket = k.bucket`,
		`INSERT INTO issue_stats_hourly_rolled (bucket, project_id, fingerprint, events)
		 SELECT k.bucket, k.project_id, e.fingerprint, count(*)
		 FROM unnest($1::bigint[], $2::timestamptz[]) AS k(project_id, bucket)
		 JOIN events e ON e.project_id = k.project_id AND e.occurred_at >= k.bucket AND e.occurred_at < k.bucket + INTERVAL '1 hour'
		 WHERE e.fingerprint IS NOT NULL
		 GROUP BY 1, 2, 3`,
	} {
		if _, err := tx.Exec(ctx, q, pids, buckets); err != nil {
			return err
		}
	}
	return nil
}

func rollupSessions(ctx context.Context, tx pgx.Tx, pids []int64, buckets []time.Time) error {
	for _, q := range []string{
		`DELETE FROM release_health_hourly_rolled r USING unnest($1::bigint[], $2::timestamptz[]) AS k(project_id, bucket)
		 WHERE r.project_id = k.project_id AND r.bucket = k.bucket`,
		`INSERT INTO release_health_hourly_rolled (bucket, project_id, release, total, crashed, errored)
		 SELECT k.bucket, k.project_id, s.release, sum(s.count),
		        COALESCE(sum(s.count) FILTER (WHERE s.status = 'crashed'), 0),
		        COALESCE(sum(s.count) FILTER (WHERE s.status IN ('errored', 'abnormal')), 0)
		 FROM unnest($1::bigint[], $2::timestamptz[]) AS k(project_id, bucket)
		 JOIN sessions s ON s.project_id = k.project_id AND s.started_at >= k.bucket AND s.started_at < k.bucket + INTERVAL '1 hour'
		 GROUP BY 1, 2, 3`,
	} {
		if _, err := tx.Exec(ctx, q, pids, buckets); err != nil {
			return err
		}
	}
	return nil
}

// DirtyHours is how many hours await rollup (health / tests).
func DirtyHours(ctx context.Context, st *store.Store) (int64, error) {
	n, err := st.CountDirtyStats(ctx)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, nil
	}
	return int64(n), err
}

// ── payload packs ──────────────────────────────────────────────────────

// PackInterval is how often each process looks for closed packs.
const PackInterval = 5 * time.Second

// PackPayloads uploads the closed packs: for each, lock its row (SKIP
// LOCKED, so processes share the work), lay the spool rows out by offset,
// PUT the object, delete the rows and the pack, commit. A failed PUT rolls
// the pack back for the next run; a crash after the PUT only means the
// same bytes are uploaded again. Returns how many payloads were packed.
func PackPayloads(ctx context.Context, st *store.Store) (int, error) {
	keys, err := st.ClosedPacks(ctx)
	if err != nil {
		return 0, err
	}
	total := 0
	for _, key := range keys {
		n, err := packOne(ctx, st, key)
		if err != nil {
			return total, err
		}
		total += n
	}
	return total, nil
}

func packOne(ctx context.Context, st *store.Store, key string) (int, error) {
	n := 0
	err := st.Tx(ctx, func(ctx context.Context, tx pgx.Tx, q *sqlc.Queries) error {
		if _, err := q.LockClosedPack(ctx, key); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return nil // another process has it, or already uploaded it
			}
			return err
		}
		rows, err := q.PackRows(ctx, key)
		if err != nil {
			return err
		}
		if len(rows) > 0 {
			members := make([]blob.PackMember, len(rows))
			for i, r := range rows {
				members[i] = blob.PackMember{Off: r.Offset, Data: r.Data}
			}
			if err := st.Blobs.PutRaw(ctx, key, blob.AssemblePack(members)); err != nil {
				return err
			}
			if err := q.DeletePack(ctx, key); err != nil {
				return err
			}
		}
		if err := q.DeletePackRow(ctx, key); err != nil {
			return err
		}
		n = len(rows)
		return nil
	})
	return n, err
}

// PackAll closes the open packs and uploads everything (the CLI after an
// import or seed: the data should be in the bucket when the command
// returns).
func PackAll(ctx context.Context, st *store.Store) error {
	if _, err := st.CloseOpenPacks(ctx); err != nil {
		return err
	}
	_, err := PackPayloads(ctx, st)
	return err
}
