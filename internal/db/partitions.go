package db

import (
	"context"
	"fmt"
	"regexp"
	"sort"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/newlix/crashcart/internal/pk"
)

// events is range-partitioned by id (= time) into one partition per UTC
// day, named events_pYYYYMMDD. The partition set is managed here, not by an
// extension: EnsurePartitions runs at startup and from the retention job so
// the next days always exist; DropPartitionsBefore is retention's fast path.

var partitionRe = regexp.MustCompile(`^events_p(\d{8})$`)

func partitionName(day time.Time) string { return "events_p" + day.UTC().Format("20060102") }

func dayOf(t time.Time) time.Time {
	t = t.UTC()
	return time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, time.UTC)
}

// EnsurePartitions creates the daily partitions covering [from, to]
// (inclusive days) that don't exist yet. Safe to call concurrently.
func EnsurePartitions(ctx context.Context, pool *pgxpool.Pool, from, to time.Time) ([]string, error) {
	tx, err := pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtext('crashcart_partitions'))`); err != nil {
		return nil, err
	}
	existing, err := listPartitions(ctx, tx)
	if err != nil {
		return nil, err
	}
	var created []string
	for day := dayOf(from); !day.After(dayOf(to)); day = day.AddDate(0, 0, 1) {
		name := partitionName(day)
		if existing[name] {
			continue
		}
		stmt := fmt.Sprintf("CREATE TABLE %s PARTITION OF events FOR VALUES FROM (%d) TO (%d)",
			name, pk.Lower(day), pk.Lower(day.AddDate(0, 0, 1)))
		if _, err := tx.Exec(ctx, stmt); err != nil {
			return created, fmt.Errorf("create %s: %w", name, err)
		}
		created = append(created, name)
	}
	return created, tx.Commit(ctx)
}

// DropPartitionsBefore drops every daily partition whose whole day is
// before cutoff. Rows in the default partition are not touched.
func DropPartitionsBefore(ctx context.Context, pool *pgxpool.Pool, cutoff time.Time) ([]string, error) {
	existing, err := listPartitions(ctx, pool)
	if err != nil {
		return nil, err
	}
	names := make([]string, 0, len(existing))
	for n := range existing {
		names = append(names, n)
	}
	sort.Strings(names)
	limit := dayOf(cutoff)
	var dropped []string
	for _, name := range names {
		m := partitionRe.FindStringSubmatch(name)
		if m == nil {
			continue
		}
		day, err := time.Parse("20060102", m[1])
		if err != nil || day.AddDate(0, 0, 1).After(limit) { // partition ends after the cutoff day → keep
			continue
		}
		if _, err := pool.Exec(ctx, "DROP TABLE IF EXISTS "+name); err != nil {
			return dropped, fmt.Errorf("drop %s: %w", name, err)
		}
		dropped = append(dropped, name)
	}
	return dropped, nil
}

type querier interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
}

// listPartitions returns the partitions of events in the current schema.
func listPartitions(ctx context.Context, q querier) (map[string]bool, error) {
	rows, err := q.Query(ctx, `SELECT c.relname FROM pg_inherits i
		JOIN pg_class c ON c.oid = i.inhrelid
		JOIN pg_class p ON p.oid = i.inhparent
		WHERE p.relname = 'events'
		  AND p.relnamespace = (SELECT oid FROM pg_namespace WHERE nspname = current_schema())`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]bool{}
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, err
		}
		out[name] = true
	}
	return out, rows.Err()
}

// PartitionHorizon is how many days ahead partitions are kept ready.
const PartitionHorizon = 3

// EnsureUpcomingPartitions covers yesterday through now + PartitionHorizon.
func EnsureUpcomingPartitions(ctx context.Context, pool *pgxpool.Pool, now time.Time) ([]string, error) {
	return EnsurePartitions(ctx, pool, now.AddDate(0, 0, -1), now.AddDate(0, 0, PartitionHorizon))
}
