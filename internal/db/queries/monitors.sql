-- name: UpsertMonitor :one
-- The SDK's monitor_config upsert: schedule/thresholds are overwritten,
-- state (last_status, consecutive_*, next_expected_at, last_checkin_at)
-- is untouched — a re-upload of the same schedule must not reset a
-- monitor's alerting history.
INSERT INTO monitors (project_id, slug, schedule_type, schedule_value, schedule_unit, timezone,
                      checkin_margin_min, max_runtime_min, failure_threshold, recovery_threshold)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)
ON CONFLICT (project_id, slug) DO UPDATE SET
    schedule_type = EXCLUDED.schedule_type, schedule_value = EXCLUDED.schedule_value, schedule_unit = EXCLUDED.schedule_unit,
    timezone = EXCLUDED.timezone, checkin_margin_min = EXCLUDED.checkin_margin_min, max_runtime_min = EXCLUDED.max_runtime_min,
    failure_threshold = EXCLUDED.failure_threshold, recovery_threshold = EXCLUDED.recovery_threshold
RETURNING *;

-- name: GetMonitor :one
SELECT * FROM monitors WHERE project_id = $1 AND slug = $2;

-- name: ListMonitors :many
-- Project-scoped, alphabetical: the Monitors page and its API.
SELECT * FROM monitors WHERE project_id = $1 ORDER BY slug;

-- name: DeleteMonitor :execrows
DELETE FROM monitors WHERE project_id = $1 AND slug = $2;

-- name: RecordMonitorResult :exec
-- Advances a monitor's state after a terminal check-in (ingest) or a
-- missed/timeout detection (alerts.CheckMonitors) — never after an
-- in_progress one. next_expected_at and the consecutive counters are
-- computed in Go (the schedule math needs the parsed cron/interval
-- schedule); this just writes the result.
UPDATE monitors SET
    last_status = sqlc.arg(last_status)::checkin_status,
    consecutive_failures = sqlc.arg(consecutive_failures)::int,
    consecutive_successes = sqlc.arg(consecutive_successes)::int,
    alerting = sqlc.arg(alerting)::bool,
    next_expected_at = sqlc.arg(next_expected_at)::timestamptz,
    last_checkin_at = sqlc.arg(last_checkin_at)::timestamptz
WHERE project_id = $1 AND slug = $2;

-- name: DueMonitors :many
-- Monitors whose next expected check-in has passed (alerts.CheckMonitors,
-- every minute): each becomes one synthetic `missed` check-in.
SELECT * FROM monitors WHERE next_expected_at IS NOT NULL AND next_expected_at < sqlc.arg(now)::timestamptz;

-- name: TimedOutCheckIns :many
-- in_progress check-ins that outlived their monitor's max_runtime_min
-- (alerts.CheckMonitors): each is flipped to `timeout`.
SELECT c.project_id, c.monitor_slug, c.check_in_id, c.started_at
FROM monitor_checkins c JOIN monitors m ON m.project_id = c.project_id AND m.slug = c.monitor_slug
WHERE c.status = 'in_progress' AND c.started_at + make_interval(mins => m.max_runtime_min) < $1;

-- name: FindOpenCheckIn :one
-- The row a check-in item resolves to before writing: by its own
-- check_in_id, or — the all-zero shorthand — the monitor's latest
-- in_progress row. No match means a fresh row (a new check_in_id, or a
-- zero id with nothing open to update).
SELECT * FROM monitor_checkins
WHERE project_id = $1 AND monitor_slug = $2
  AND ((sqlc.arg(zero)::bool AND status = 'in_progress') OR (NOT sqlc.arg(zero)::bool AND check_in_id = sqlc.arg(check_in_id)::uuid))
ORDER BY started_at DESC LIMIT 1;

-- name: InsertCheckIn :exec
INSERT INTO monitor_checkins (started_at, project_id, monitor_slug, check_in_id, status, duration_s, release, environment)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8);

-- name: UpdateCheckIn :exec
-- A later check-in of the same run (the same check_in_id, or the
-- all-zero shorthand resolved by FindOpenCheckIn to an existing row):
-- status advances, duration/release/environment are kept if this one
-- did not send them. started_at (the partition key) is never touched.
UPDATE monitor_checkins SET status = $5, duration_s = COALESCE(sqlc.narg(duration_s)::real, duration_s),
    release = COALESCE(sqlc.narg(release)::text, release), environment = COALESCE(sqlc.narg(environment)::text, environment)
WHERE project_id = $1 AND monitor_slug = $2 AND check_in_id = $3 AND started_at = $4;

-- name: ListCheckIns :many
-- One monitor's recent check-ins, newest first: the Monitors detail page.
SELECT * FROM monitor_checkins WHERE project_id = $1 AND monitor_slug = $2 ORDER BY started_at DESC, check_in_id DESC LIMIT $3;
