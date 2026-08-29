-- Chart queries take a window [from_at, to_at) (from_at bucket-aligned)
-- and a bucket width in seconds; they fold the hourly aggregates with
-- crashcart_bucket and gap-fill with crashcart_buckets, so every bucket
-- of the window comes back, in order.

-- name: Timeline :many
-- Events / crashes per bucket, split into the top `top` releases (by
-- crashes, then events) plus 'other'; every bucket for every series,
-- ordered by bucket, then series rank.
WITH s AS (
    SELECT crashcart_bucket(bucket, sqlc.arg(width)::bigint) AS bucket, release,
           sum(events) AS events, sum(crashes) AS crashes
    FROM event_stats_hourly
    WHERE project_id = sqlc.arg(project_id)::bigint
      AND bucket >= sqlc.arg(from_at)::timestamptz AND bucket < sqlc.arg(to_at)::timestamptz
    GROUP BY 1, 2),
ranked AS (
    SELECT release, row_number() OVER (ORDER BY sum(crashes) DESC, sum(events) DESC, release) AS rank
    FROM s GROUP BY release),
series AS (
    SELECT CASE WHEN rank <= sqlc.arg(top)::bigint THEN release ELSE 'other' END AS series, min(rank) AS rank
    FROM ranked GROUP BY 1),
folded AS (
    SELECT s.bucket, CASE WHEN r.rank <= sqlc.arg(top)::bigint THEN s.release ELSE 'other' END AS series,
           sum(s.events) AS events, sum(s.crashes) AS crashes
    FROM s JOIN ranked r USING (release) GROUP BY 1, 2)
SELECT b::timestamptz AS bucket, se.series AS release,
       COALESCE(f.events, 0)::bigint AS events, COALESCE(f.crashes, 0)::bigint AS crashes
FROM crashcart_buckets(sqlc.arg(from_at)::timestamptz, sqlc.arg(to_at)::timestamptz, sqlc.arg(width)::bigint) AS b
CROSS JOIN series se
LEFT JOIN folded f ON f.bucket = b AND f.series = se.series
ORDER BY b, se.rank;

-- name: Totals :one
SELECT COALESCE(sum(events), 0)::bigint AS events,
       COALESCE(sum(crashes), 0)::bigint AS crashes,
       COALESCE(sum(errors), 0)::bigint AS errors
FROM event_stats_hourly
WHERE project_id = $1 AND bucket >= $2 AND bucket < $3;

-- name: LevelTotals :many
SELECT level, sum(events)::bigint AS events
FROM event_stats_hourly
WHERE project_id = $1 AND bucket >= $2 AND bucket < $3
GROUP BY level;

-- name: ReleaseStats :many
-- Every release with activity in the window, most recently active first.
SELECT release,
       array_remove(array_agg(DISTINCT platform), '')::text[] AS platforms,
       min(bucket)::timestamptz AS first_seen, max(bucket)::timestamptz AS last_seen,
       sum(events)::bigint AS events, sum(crashes)::bigint AS crashes, sum(errors)::bigint AS errors
FROM event_stats_hourly
WHERE project_id = $1 AND bucket >= $2 AND bucket < $3 AND release <> ''
GROUP BY release ORDER BY max(bucket) DESC, release;

-- name: ReleaseTimeline :many
WITH h AS (
    SELECT crashcart_bucket(bucket, sqlc.arg(width)::bigint) AS bucket, sum(events) AS events, sum(crashes) AS crashes
    FROM event_stats_hourly
    WHERE project_id = sqlc.arg(project_id)::bigint AND release = sqlc.arg(release)::text
      AND bucket >= sqlc.arg(from_at)::timestamptz AND bucket < sqlc.arg(to_at)::timestamptz
    GROUP BY 1)
SELECT b::timestamptz AS bucket, COALESCE(h.events, 0)::bigint AS events, COALESCE(h.crashes, 0)::bigint AS crashes
FROM crashcart_buckets(sqlc.arg(from_at)::timestamptz, sqlc.arg(to_at)::timestamptz, sqlc.arg(width)::bigint) AS b
LEFT JOIN h ON h.bucket = b
ORDER BY b;

-- name: LatestReleaseHealth :one
-- The most recently active release in the window (by events; ties by
-- name) with its session totals over the same window (0 without sessions).
SELECT e.release,
       COALESCE((SELECT sum(h.total) FROM release_health_daily h
                  WHERE h.project_id = e.project_id AND h.release = e.release
                    AND h.bucket >= sqlc.arg(day_from)::timestamptz AND h.bucket < sqlc.arg(to_at)::timestamptz), 0)::bigint AS total,
       COALESCE((SELECT sum(h.crashed) FROM release_health_daily h
                  WHERE h.project_id = e.project_id AND h.release = e.release
                    AND h.bucket >= sqlc.arg(day_from)::timestamptz AND h.bucket < sqlc.arg(to_at)::timestamptz), 0)::bigint AS crashed
FROM event_stats_hourly e
WHERE e.project_id = sqlc.arg(project_id)::bigint AND e.release <> ''
  AND e.bucket >= sqlc.arg(hour_from)::timestamptz AND e.bucket < sqlc.arg(to_at)::timestamptz
GROUP BY e.project_id, e.release
ORDER BY max(e.bucket) DESC, e.release DESC
LIMIT 1;

-- name: CrashSpikeInputs :one
-- Crashes in the exact last hour (from the raw table, so the top of the
-- hour does not matter) vs. the 24 full hourly buckets before that hour.
SELECT (SELECT count(*) FROM events e
         WHERE e.project_id = sqlc.arg(project_id)::bigint AND e.occurred_at >= sqlc.arg(recent_from)::timestamptz
           AND crashcart_is_crash(e.level, e.handled))::bigint AS recent,
       COALESCE((SELECT sum(h.crashes) FROM event_stats_hourly h
                  WHERE h.project_id = sqlc.arg(project_id)::bigint
                    AND h.bucket >= sqlc.arg(baseline_from)::timestamptz AND h.bucket < sqlc.arg(baseline_to)::timestamptz), 0)::bigint AS baseline;

-- name: PlatformTotals :many
-- Raw SDK platforms seen in a window (for the "expected vs received" check).
SELECT platform, sum(events)::bigint AS events
FROM event_stats_hourly
WHERE project_id = $1 AND bucket >= $2 AND bucket < $3
GROUP BY platform ORDER BY events DESC;

-- name: AddProjectUsage :one
-- Counts n received events against the project's UTC day and returns the
-- day's total (the caller compares it with daily_quota and rolls back).
INSERT INTO project_usage (project_id, day, events) VALUES ($1, $2, $3)
ON CONFLICT (project_id, day) DO UPDATE SET events = project_usage.events + EXCLUDED.events
RETURNING events;

-- name: ProjectUsage :one
SELECT COALESCE((SELECT events FROM project_usage WHERE project_id = $1 AND day = $2), 0)::bigint;

-- name: ExpireProjectUsage :execrows
DELETE FROM project_usage WHERE day < $1;
