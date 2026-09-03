// Session writes are pipelined by store.InsertSessions (hand-written).
package store

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5"
)

// Session is a raw session row (export only — writes are pipelined by
// InsertSessions, reads normally go through the release_health_hourly
// rollup, not this table directly).
type Session struct {
	StartedAt   time.Time     `json:"started_at"`
	ProjectID   int64         `json:"project_id"`
	Sid         string        `json:"sid"`
	Release     string        `json:"release"`
	Environment *string       `json:"environment"`
	Status      SessionStatus `json:"status"`
	Count       int32         `json:"count"`
}

// ReleaseHealthRow is one release's session totals over a window: crash-free
// rate inputs.
type ReleaseHealthRow struct {
	Release string
	Total   int64
	Crashed int64
	Errored int64
}

// ReleaseHealth: per release over a window.
func ReleaseHealth(ctx context.Context, db DB, projectID int64, from, to time.Time) ([]ReleaseHealthRow, error) {
	rows, err := db.Query(ctx, `SELECT release,
		       COALESCE(sum(total), 0)::bigint   AS total,
		       COALESCE(sum(crashed), 0)::bigint AS crashed,
		       COALESCE(sum(errored), 0)::bigint AS errored
		FROM release_health_hourly
		WHERE project_id = $1 AND bucket >= $2 AND bucket < $3
		GROUP BY release`, projectID, from, to)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowToStructByName[ReleaseHealthRow])
}

// ReleaseHealthDailyRow is one UTC day's session totals for one release.
type ReleaseHealthDailyRow struct {
	Bucket  time.Time
	Total   int64
	Crashed int64
	Errored int64
}

// ReleaseHealthDaily: per UTC day (from day-aligned).
func ReleaseHealthDaily(ctx context.Context, db DB, projectID int64, release string, from, to time.Time) ([]ReleaseHealthDailyRow, error) {
	rows, err := db.Query(ctx, `SELECT crashcart_bucket(bucket, 86400)::timestamptz AS bucket,
		       COALESCE(sum(total), 0)::bigint AS total, COALESCE(sum(crashed), 0)::bigint AS crashed, COALESCE(sum(errored), 0)::bigint AS errored
		FROM release_health_hourly
		WHERE project_id = $1 AND release = $2 AND bucket >= $3 AND bucket < $4
		GROUP BY 1 ORDER BY 1`, projectID, release, from, to)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowToStructByName[ReleaseHealthDailyRow])
}
