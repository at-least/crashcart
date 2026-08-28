-- name: Timeline :many
-- Hourly buckets over a window, split by release (the top-N is done in Go).
SELECT bucket, release, platform,
       sum(events)::bigint AS events, sum(crashes)::bigint AS crashes, sum(errors)::bigint AS errors
FROM event_stats_hourly
WHERE project_id = $1 AND bucket >= $2 AND bucket < $3
GROUP BY bucket, release, platform ORDER BY bucket;

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
-- Every release with activity in the window (plus all-time first/last seen).
SELECT release, platform,
       min(bucket)::bigint AS first_seen, max(bucket)::bigint AS last_seen,
       sum(events)::bigint AS events, sum(crashes)::bigint AS crashes, sum(errors)::bigint AS errors
FROM event_stats_hourly
WHERE project_id = $1 AND bucket >= $2 AND bucket < $3 AND release <> ''
GROUP BY release, platform ORDER BY max(bucket) DESC;

-- name: ReleaseTimeline :many
SELECT bucket, sum(events)::bigint AS events, sum(crashes)::bigint AS crashes
FROM event_stats_hourly
WHERE project_id = $1 AND release = $2 AND bucket >= $3 AND bucket < $4
GROUP BY bucket ORDER BY bucket;

-- name: CrashSpikeInputs :one
-- Crashes in the last hour vs. the hourly mean of the 24 h before it.
SELECT COALESCE(sum(crashes) FILTER (WHERE bucket >= $3), 0)::bigint AS recent,
       COALESCE(sum(crashes) FILTER (WHERE bucket < $3), 0)::bigint AS baseline
FROM event_stats_hourly
WHERE project_id = $1 AND bucket >= $2;
