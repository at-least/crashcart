package store

// Chart queries take a window [from_at, to_at) (from_at bucket-aligned)
// and a bucket width in seconds. They read crashcart_event_stats (the
// hourly rollup, exact for dirty hours), fold with crashcart_bucket and
// gap-fill with crashcart_buckets, so every bucket of the window comes
// back, in order.

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5"
)

// TimelineRow is one (bucket, release) point of a Timeline chart.
type TimelineRow struct {
	Bucket    time.Time `json:"bucket"`
	Release   string    `json:"release"`
	Events    int64     `json:"events"`
	Unhandled int64     `json:"unhandled"`
}

const timelineSQL = `WITH s AS (
    SELECT crashcart_bucket(bucket, $3::bigint) AS bucket, release,
           sum(events) AS events, sum(unhandled) AS unhandled
    FROM crashcart_event_stats($4::bigint, $1::timestamptz, $2::timestamptz)
    GROUP BY 1, 2),
ranked AS (
    SELECT release, row_number() OVER (ORDER BY sum(unhandled) DESC, sum(events) DESC, release) AS rank
    FROM s GROUP BY release),
series AS (
    SELECT CASE WHEN rank <= $5::bigint THEN release ELSE 'other' END AS series, min(rank) AS rank
    FROM ranked GROUP BY 1),
folded AS (
    SELECT s.bucket, CASE WHEN r.rank <= $5::bigint THEN s.release ELSE 'other' END AS series,
           sum(s.events) AS events, sum(s.unhandled) AS unhandled
    FROM s JOIN ranked r USING (release) GROUP BY 1, 2)
SELECT b::timestamptz AS bucket, se.series AS release,
       COALESCE(f.events, 0)::bigint AS events, COALESCE(f.unhandled, 0)::bigint AS unhandled
FROM crashcart_buckets($1::timestamptz, $2::timestamptz, $3::bigint) AS b
CROSS JOIN series se
LEFT JOIN folded f ON f.bucket = b AND f.series = se.series
ORDER BY b, se.rank`

// Timeline is events / unhandled per bucket, split into the top `top`
// releases (by unhandled, then events) plus 'other'; every bucket for
// every series, ordered by bucket, then series rank.
func Timeline(ctx context.Context, db DB, projectID int64, from, to time.Time, width, top int64) ([]TimelineRow, error) {
	rows, err := db.Query(ctx, timelineSQL, from, to, width, projectID, top)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowToStructByName[TimelineRow])
}

// TotalsRow is a project's event totals in a window.
type TotalsRow struct {
	Events    int64 `json:"events"`
	Unhandled int64 `json:"unhandled"`
	Errors    int64 `json:"errors"`
}

func Totals(ctx context.Context, db DB, projectID int64, from, to time.Time) (TotalsRow, error) {
	rows, err := db.Query(ctx, `SELECT COALESCE(sum(events), 0)::bigint AS events,
       COALESCE(sum(unhandled), 0)::bigint AS unhandled,
       COALESCE(sum(errors), 0)::bigint AS errors
FROM crashcart_event_stats($1::bigint, $2::timestamptz, $3::timestamptz)`, projectID, from, to)
	if err != nil {
		return TotalsRow{}, err
	}
	return pgx.CollectOneRow(rows, pgx.RowToStructByName[TotalsRow])
}

// LevelTotalsRow is a project's event count at one level in a window.
type LevelTotalsRow struct {
	Level  EventLevel `json:"level"`
	Events int64      `json:"events"`
}

func LevelTotals(ctx context.Context, db DB, projectID int64, from, to time.Time) ([]LevelTotalsRow, error) {
	rows, err := db.Query(ctx, `SELECT level, sum(events)::bigint AS events
FROM crashcart_event_stats($1::bigint, $2::timestamptz, $3::timestamptz)
GROUP BY level`, projectID, from, to)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowToStructByName[LevelTotalsRow])
}

// ReleaseStatsRow is one release's activity in a window; platforms and
// first_seen are all-time, from the releases table.
type ReleaseStatsRow struct {
	Release   string    `json:"release"`
	Platforms []string  `json:"platforms"`
	FirstSeen time.Time `json:"first_seen"`
	LastSeen  time.Time `json:"last_seen"`
	Events    int64     `json:"events"`
	Unhandled int64     `json:"unhandled"`
	Errors    int64     `json:"errors"`
}

// ReleaseStats is every release with activity in the window, most
// recently active first.
func ReleaseStats(ctx context.Context, db DB, projectID int64, from, to time.Time) ([]ReleaseStatsRow, error) {
	rows, err := db.Query(ctx, `SELECT s.release,
       COALESCE(r.platforms, '{}'::text[])::text[] AS platforms,
       COALESCE(r.first_seen, min(s.bucket))::timestamptz AS first_seen, max(s.bucket)::timestamptz AS last_seen,
       sum(s.events)::bigint AS events, sum(s.unhandled)::bigint AS unhandled, sum(s.errors)::bigint AS errors
FROM crashcart_event_stats($1::bigint, $2::timestamptz, $3::timestamptz) AS s
LEFT JOIN releases r ON r.project_id = s.project_id AND r.release = s.release
WHERE s.release <> ''
GROUP BY s.release, r.platforms, r.first_seen ORDER BY max(s.bucket) DESC, s.release`, projectID, from, to)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowToStructByName[ReleaseStatsRow])
}

// ReleaseTimelineRow is one bucket of ReleaseTimeline.
type ReleaseTimelineRow struct {
	Bucket    time.Time `json:"bucket"`
	Events    int64     `json:"events"`
	Unhandled int64     `json:"unhandled"`
}

func ReleaseTimeline(ctx context.Context, db DB, projectID int64, release string, from, to time.Time, width int64) ([]ReleaseTimelineRow, error) {
	rows, err := db.Query(ctx, `WITH h AS (
    SELECT crashcart_bucket(bucket, $3::bigint) AS bucket, sum(events) AS events, sum(unhandled) AS unhandled
    FROM crashcart_event_stats($4::bigint, $1::timestamptz, $2::timestamptz)
    WHERE release = $5::text
    GROUP BY 1)
SELECT b::timestamptz AS bucket, COALESCE(h.events, 0)::bigint AS events, COALESCE(h.unhandled, 0)::bigint AS unhandled
FROM crashcart_buckets($1::timestamptz, $2::timestamptz, $3::bigint) AS b
LEFT JOIN h ON h.bucket = b
ORDER BY b`, from, to, width, projectID, release)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowToStructByName[ReleaseTimelineRow])
}

// LatestReleaseHealthRow is the most recently active release in the
// window with its session totals over the same window.
type LatestReleaseHealthRow struct {
	Release string `json:"release"`
	Total   int64  `json:"total"`
	Crashed int64  `json:"crashed"`
}

// LatestReleaseHealth is the most recently active release in the window
// (by events; ties by name) with its session totals over the same window
// (0 without sessions). hourFrom is the event window start aligned to
// width, dayFrom the day-aligned start for the session totals.
func LatestReleaseHealth(ctx context.Context, db DB, projectID int64, hourFrom, dayFrom, to time.Time) (LatestReleaseHealthRow, error) {
	// Three of the four params are time.Time and @To is reused across both
	// the health-hourly window and the event-stats window — exactly the
	// shape where a positional arg list silently swaps two same-typed
	// values, so this binds by name instead.
	rows, err := db.Query(ctx, `SELECT e.release,
       COALESCE((SELECT sum(h.total) FROM release_health_hourly h
                  WHERE h.project_id = e.project_id AND h.release = e.release
                    AND h.bucket >= @DayFrom::timestamptz AND h.bucket < @To::timestamptz), 0)::bigint AS total,
       COALESCE((SELECT sum(h.crashed) FROM release_health_hourly h
                  WHERE h.project_id = e.project_id AND h.release = e.release
                    AND h.bucket >= @DayFrom::timestamptz AND h.bucket < @To::timestamptz), 0)::bigint AS crashed
FROM crashcart_event_stats(@ProjectID::bigint, @HourFrom::timestamptz, @To::timestamptz) AS e
WHERE e.release <> ''
GROUP BY e.project_id, e.release
ORDER BY max(e.bucket) DESC, e.release DESC
LIMIT 1`, pgx.StrictNamedArgs{
		"ProjectID": projectID,
		"HourFrom":  hourFrom,
		"DayFrom":   dayFrom,
		"To":        to,
	})
	if err != nil {
		return LatestReleaseHealthRow{}, err
	}
	return pgx.CollectOneRow(rows, pgx.RowToStructByName[LatestReleaseHealthRow])
}

// UnhandledSpikeInputsRow is one project's unhandled counts for the spike
// check: the exact last hour vs. the 24 full hourly buckets before it.
type UnhandledSpikeInputsRow struct {
	ProjectID int64 `json:"project_id"`
	Recent    int64 `json:"recent"`
	Baseline  int64 `json:"baseline"`
}

// UnhandledSpikeInputs: per project, unhandled in the exact last hour
// (from the raw table, so the top of the hour does not matter) vs. the 24
// full hourly buckets before that hour. Projects with no unhandled in
// either are omitted.
func UnhandledSpikeInputs(ctx context.Context, db DB, recentFrom, baselineFrom, baselineTo time.Time) ([]UnhandledSpikeInputsRow, error) {
	// recentFrom/baselineFrom/baselineTo are three time.Time in a row —
	// bound by name so a future edit can't silently transpose the spike
	// window with the baseline window.
	rows, err := db.Query(ctx, `WITH recent AS (
    SELECT p.id AS project_id,
           (SELECT count(*) FROM events e WHERE e.project_id = p.id AND e.occurred_at >= @RecentFrom::timestamptz
              AND e.handled = false) AS n
    FROM projects p),
baseline AS (
    SELECT project_id, sum(unhandled) AS n FROM event_stats_hourly
    WHERE bucket >= @BaselineFrom::timestamptz AND bucket < @BaselineTo::timestamptz
    GROUP BY project_id)
SELECT COALESCE(recent.project_id, baseline.project_id)::bigint AS project_id,
       COALESCE(recent.n, 0)::bigint AS recent, COALESCE(baseline.n, 0)::bigint AS baseline
FROM recent FULL OUTER JOIN baseline USING (project_id)
WHERE COALESCE(recent.n, 0) > 0 OR COALESCE(baseline.n, 0) > 0`, pgx.StrictNamedArgs{
		"RecentFrom":   recentFrom,
		"BaselineFrom": baselineFrom,
		"BaselineTo":   baselineTo,
	})
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowToStructByName[UnhandledSpikeInputsRow])
}

// PlatformTotalsRow is one raw SDK platform's event count in a window.
type PlatformTotalsRow struct {
	Platform string `json:"platform"`
	Events   int64  `json:"events"`
}

// PlatformTotals is the raw SDK platforms seen in a window (for the
// "expected vs received" check).
func PlatformTotals(ctx context.Context, db DB, projectID int64, from, to time.Time) ([]PlatformTotalsRow, error) {
	rows, err := db.Query(ctx, `SELECT platform, sum(events)::bigint AS events
FROM crashcart_event_stats($1::bigint, $2::timestamptz, $3::timestamptz)
GROUP BY platform ORDER BY events DESC`, projectID, from, to)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowToStructByName[PlatformTotalsRow])
}

// MarkEventStatsDirty is called in the transaction that writes or updates
// events: the hours touched (distinct, UTC hour starts) are recomputed by
// the rollup job and read live until then. gen moves so a mark that lands
// while the job runs is not cleared by it. Rows insert in bucket order
// (the upsert locks each row until commit; two writers in opposite orders
// would deadlock).
func MarkEventStatsDirty(ctx context.Context, db DB, projectID int64, buckets []time.Time) error {
	_, err := db.Exec(ctx, markEventHours, projectID, buckets)
	return err
}

func MarkSessionStatsDirty(ctx context.Context, db DB, projectID int64, buckets []time.Time) error {
	_, err := db.Exec(ctx, markSessionHours, projectID, buckets)
	return err
}

// CountDirtyStats is hours awaiting rollup (events + sessions); tests and
// the health check.
func CountDirtyStats(ctx context.Context, db DB) (int32, error) {
	var n int32
	err := db.QueryRow(ctx, "SELECT (SELECT count(*) FROM event_stats_dirty) + (SELECT count(*) FROM session_stats_dirty)").Scan(&n)
	return n, err
}

// AddProjectUsage counts n received events against the project's UTC day
// and returns the day's total (the caller compares it with daily_quota
// and rolls back).
func AddProjectUsage(ctx context.Context, db DB, projectID int64, day time.Time, n int64) (int64, error) {
	var events int64
	err := db.QueryRow(ctx, `INSERT INTO project_usage (project_id, day, events) VALUES ($1, $2, $3)
ON CONFLICT (project_id, day) DO UPDATE SET events = project_usage.events + EXCLUDED.events
RETURNING events`, projectID, day, n).Scan(&events)
	return events, err
}

func ProjectUsage(ctx context.Context, db DB, projectID int64, day time.Time) (int64, error) {
	var n int64
	err := db.QueryRow(ctx, "SELECT COALESCE((SELECT events FROM project_usage WHERE project_id = $1 AND day = $2), 0)::bigint", projectID, day).Scan(&n)
	return n, err
}

func ExpireProjectUsage(ctx context.Context, db DB, before time.Time) (int64, error) {
	tag, err := db.Exec(ctx, "DELETE FROM project_usage WHERE day < $1", before)
	if err != nil {
		return 0, err
	}
	return tag.RowsAffected(), nil
}
