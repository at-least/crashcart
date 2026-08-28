-- name: UpsertHourlyStats :batchexec
INSERT INTO hourly_stats (hour, level, crash_count, fatal_count, error_count)
VALUES ($1, $2, $3, $4, $5)
ON CONFLICT (hour, level) DO UPDATE SET
    crash_count = hourly_stats.crash_count + EXCLUDED.crash_count,
    fatal_count = hourly_stats.fatal_count + EXCLUDED.fatal_count,
    error_count = hourly_stats.error_count + EXCLUDED.error_count;

-- Totals for a window. `until` NULL = open-ended.
-- name: StatsTotals :one
SELECT COALESCE(SUM(fatal_count), 0)::bigint AS fatal,
       COALESCE(SUM(crash_count), 0)::bigint AS crash,
       COALESCE(SUM(error_count), 0)::bigint AS error
FROM hourly_stats
WHERE hour >= @since AND (sqlc.narg('until')::timestamptz IS NULL OR hour < sqlc.narg('until'));

-- Per-level event counts: a fatal row counts fatal events, an error row
-- counts handled + unhandled errors.
-- name: StatsByLevel :many
SELECT level,
       (CASE WHEN level = 'fatal' THEN SUM(fatal_count) ELSE SUM(error_count + crash_count) END)::bigint AS count
FROM hourly_stats
WHERE hour >= @since AND (sqlc.narg('until')::timestamptz IS NULL OR hour < sqlc.narg('until'))
GROUP BY level
ORDER BY level;

-- name: CrashesByHour :many
SELECT hour, SUM(crash_count)::bigint AS count
FROM hourly_stats
WHERE hour >= @since AND (sqlc.narg('until')::timestamptz IS NULL OR hour < sqlc.narg('until'))
GROUP BY hour
ORDER BY hour;

-- name: CrashesByDay :many
SELECT date_trunc('day', hour)::timestamptz AS day, SUM(crash_count)::bigint AS count
FROM hourly_stats
WHERE hour >= @since AND (sqlc.narg('until')::timestamptz IS NULL OR hour < sqlc.narg('until'))
GROUP BY 1
ORDER BY 1;

-- name: VolumeByHour :many
SELECT hour, level,
       (CASE WHEN level = 'fatal' THEN SUM(fatal_count) ELSE SUM(error_count + crash_count) END)::bigint AS count
FROM hourly_stats
WHERE hour >= @since AND (sqlc.narg('until')::timestamptz IS NULL OR hour < sqlc.narg('until'))
GROUP BY hour, level
ORDER BY hour, level;

-- name: VolumeByDay :many
SELECT date_trunc('day', hour)::timestamptz AS day, level,
       (CASE WHEN level = 'fatal' THEN SUM(fatal_count) ELSE SUM(error_count + crash_count) END)::bigint AS count
FROM hourly_stats
WHERE hour >= @since AND (sqlc.narg('until')::timestamptz IS NULL OR hour < sqlc.narg('until'))
GROUP BY 1, level
ORDER BY 1, level;

-- Weekly baseline for crash-spike detection: average daily crashes over
-- [since, until) — the caller excludes the most recent days.
-- name: CrashBaselineDailyAvg :one
SELECT COALESCE(AVG(daily.crashes), 0)::float8 AS avg
FROM (
    SELECT date_trunc('day', hour) AS day, SUM(crash_count) AS crashes
    FROM hourly_stats
    WHERE hour >= $1 AND hour < $2
    GROUP BY 1
) daily;

-- name: DeleteHourlyStatsBefore :execrows
DELETE FROM hourly_stats WHERE hour < $1;

-- ── releases ────────────────────────────────────────────────

-- name: UpsertRelease :batchexec
INSERT INTO releases (version, platform, first_seen, last_seen, crash_count, error_count, total_events)
VALUES ($1, $2, $3, $4, $5, $6, $7)
ON CONFLICT (version) DO UPDATE SET
    first_seen   = LEAST(releases.first_seen, EXCLUDED.first_seen),
    last_seen    = GREATEST(releases.last_seen, EXCLUDED.last_seen),
    crash_count  = releases.crash_count + EXCLUDED.crash_count,
    error_count  = releases.error_count + EXCLUDED.error_count,
    total_events = releases.total_events + EXCLUDED.total_events;

-- name: ListReleases :many
SELECT * FROM releases ORDER BY last_seen DESC LIMIT 50;

-- Versions active in a window (dashboard release picker).
-- name: ListReleaseVersions :many
SELECT version FROM releases
WHERE last_seen >= @since AND (sqlc.narg('until')::timestamptz IS NULL OR first_seen < sqlc.narg('until'))
ORDER BY last_seen DESC
LIMIT 20;

-- ── release health ──────────────────────────────────────────

-- name: UpsertReleaseHealth :batchexec
INSERT INTO release_health (release, day, total_sessions, crashed_sessions, errored_sessions)
VALUES ($1, $2, $3, $4, $5)
ON CONFLICT (release, day) DO UPDATE SET
    total_sessions   = release_health.total_sessions + EXCLUDED.total_sessions,
    crashed_sessions = release_health.crashed_sessions + EXCLUDED.crashed_sessions,
    errored_sessions = release_health.errored_sessions + EXCLUDED.errored_sessions;

-- name: ReleaseHealthSummary :many
SELECT release,
       SUM(total_sessions)::bigint   AS total_sessions,
       SUM(crashed_sessions)::bigint AS crashed_sessions,
       SUM(errored_sessions)::bigint AS errored_sessions
FROM release_health
WHERE day >= @since_day AND (sqlc.narg('until_day')::date IS NULL OR day < sqlc.narg('until_day'))
GROUP BY release
ORDER BY release DESC;

-- name: DeleteReleaseHealthBefore :execrows
DELETE FROM release_health WHERE day < $1;
