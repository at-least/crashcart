-- Chart queries take a window [from_at, to_at) (from_at bucket-aligned)
-- and a bucket width in seconds. They read crashcart_event_stats (the
-- hourly rollup, exact for dirty hours), fold with crashcart_bucket and
-- gap-fill with crashcart_buckets, so every bucket of the window comes
-- back, in order.

-- name: Timeline :many
-- Events / crashes per bucket, split into the top `top` releases (by
-- crashes, then events) plus 'other'; every bucket for every series,
-- ordered by bucket, then series rank.
WITH s AS (
    SELECT crashcart_bucket(bucket, sqlc.arg(width)::bigint) AS bucket, release,
           sum(events) AS events, sum(crashes) AS crashes
    FROM crashcart_event_stats(sqlc.arg(project_id)::bigint, sqlc.arg(from_at)::timestamptz, sqlc.arg(to_at)::timestamptz)
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
FROM crashcart_event_stats(sqlc.arg(project_id)::bigint, sqlc.arg(from_at)::timestamptz, sqlc.arg(to_at)::timestamptz);

-- name: LevelTotals :many
SELECT level, sum(events)::bigint AS events
FROM crashcart_event_stats(sqlc.arg(project_id)::bigint, sqlc.arg(from_at)::timestamptz, sqlc.arg(to_at)::timestamptz)
GROUP BY level;

-- name: ReleaseStats :many
-- Every release with activity in the window, most recently active first;
-- platforms and first_seen are all-time, from the releases table.
SELECT s.release,
       COALESCE(r.platforms, '{}'::text[])::text[] AS platforms,
       COALESCE(r.first_seen, min(s.bucket))::timestamptz AS first_seen, max(s.bucket)::timestamptz AS last_seen,
       sum(s.events)::bigint AS events, sum(s.crashes)::bigint AS crashes, sum(s.errors)::bigint AS errors
FROM crashcart_event_stats(sqlc.arg(project_id)::bigint, sqlc.arg(from_at)::timestamptz, sqlc.arg(to_at)::timestamptz) AS s
LEFT JOIN releases r ON r.project_id = s.project_id AND r.release = s.release
WHERE s.release <> ''
GROUP BY s.release, r.platforms, r.first_seen ORDER BY max(s.bucket) DESC, s.release;

-- name: ReleaseTimeline :many
WITH h AS (
    SELECT crashcart_bucket(bucket, sqlc.arg(width)::bigint) AS bucket, sum(events) AS events, sum(crashes) AS crashes
    FROM crashcart_event_stats(sqlc.arg(project_id)::bigint, sqlc.arg(from_at)::timestamptz, sqlc.arg(to_at)::timestamptz)
    WHERE release = sqlc.arg(release)::text
    GROUP BY 1)
SELECT b::timestamptz AS bucket, COALESCE(h.events, 0)::bigint AS events, COALESCE(h.crashes, 0)::bigint AS crashes
FROM crashcart_buckets(sqlc.arg(from_at)::timestamptz, sqlc.arg(to_at)::timestamptz, sqlc.arg(width)::bigint) AS b
LEFT JOIN h ON h.bucket = b
ORDER BY b;

-- name: LatestReleaseHealth :one
-- The most recently active release in the window (by events; ties by
-- name) with its session totals over the same window (0 without sessions).
-- hour_from is the event window start aligned to `width`, day_from the
-- day-aligned start for the session totals.
SELECT e.release,
       COALESCE((SELECT sum(h.total) FROM release_health_hourly h
                  WHERE h.project_id = e.project_id AND h.release = e.release
                    AND h.bucket >= sqlc.arg(day_from)::timestamptz AND h.bucket < sqlc.arg(to_at)::timestamptz), 0)::bigint AS total,
       COALESCE((SELECT sum(h.crashed) FROM release_health_hourly h
                  WHERE h.project_id = e.project_id AND h.release = e.release
                    AND h.bucket >= sqlc.arg(day_from)::timestamptz AND h.bucket < sqlc.arg(to_at)::timestamptz), 0)::bigint AS crashed
FROM crashcart_event_stats(sqlc.arg(project_id)::bigint, sqlc.arg(hour_from)::timestamptz, sqlc.arg(to_at)::timestamptz) AS e
WHERE e.release <> ''
GROUP BY e.project_id, e.release
ORDER BY max(e.bucket) DESC, e.release DESC
LIMIT 1;

-- name: CrashSpikeInputs :many
-- Per project: crashes in the exact last hour (from the raw table, so the
-- top of the hour does not matter) vs. the 24 full hourly buckets before
-- that hour. Projects with no crashes in either are omitted.
WITH recent AS (
    -- Per project (every events index leads with project_id: this is the
    -- events_project_crash index per project, not a scan of the partition).
    SELECT p.id AS project_id,
           (SELECT count(*) FROM events e WHERE e.project_id = p.id AND e.occurred_at >= sqlc.arg(recent_from)::timestamptz
              AND crashcart_is_crash(e.level, e.handled)) AS n
    FROM projects p),
baseline AS (
    SELECT project_id, sum(crashes) AS n FROM event_stats_hourly
    WHERE bucket >= sqlc.arg(baseline_from)::timestamptz AND bucket < sqlc.arg(baseline_to)::timestamptz
    GROUP BY project_id)
SELECT COALESCE(recent.project_id, baseline.project_id)::bigint AS project_id,
       COALESCE(recent.n, 0)::bigint AS recent, COALESCE(baseline.n, 0)::bigint AS baseline
FROM recent FULL OUTER JOIN baseline USING (project_id)
WHERE COALESCE(recent.n, 0) > 0 OR COALESCE(baseline.n, 0) > 0;

-- name: PlatformTotals :many
-- Raw SDK platforms seen in a window (for the "expected vs received" check).
SELECT platform, sum(events)::bigint AS events
FROM crashcart_event_stats(sqlc.arg(project_id)::bigint, sqlc.arg(from_at)::timestamptz, sqlc.arg(to_at)::timestamptz)
GROUP BY platform ORDER BY events DESC;

-- name: MarkEventStatsDirty :exec
-- Called in the transaction that writes or updates events: the hours
-- touched (distinct, UTC hour starts) are recomputed by the rollup job and
-- read live until then. gen moves so a mark that lands while the job runs
-- is not cleared by it.
INSERT INTO event_stats_dirty (project_id, bucket)
SELECT sqlc.arg(project_id)::bigint, unnest(sqlc.arg(buckets)::timestamptz[])
ON CONFLICT (project_id, bucket) DO UPDATE SET gen = event_stats_dirty.gen + 1;

-- name: MarkSessionStatsDirty :exec
INSERT INTO session_stats_dirty (project_id, bucket)
SELECT sqlc.arg(project_id)::bigint, unnest(sqlc.arg(buckets)::timestamptz[])
ON CONFLICT (project_id, bucket) DO UPDATE SET gen = session_stats_dirty.gen + 1;

-- name: CountDirtyStats :one
-- Hours awaiting rollup (events + sessions); tests and the health check.
SELECT (SELECT count(*) FROM event_stats_dirty) + (SELECT count(*) FROM session_stats_dirty);

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
