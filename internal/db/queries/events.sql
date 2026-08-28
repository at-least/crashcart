-- name: InsertEvents :copyfrom
INSERT INTO events (
    occurred_at, event_id, level, message, platform, environment, release,
    device_id, device_model, os_version, screen, error_type, error_location,
    handled, sdk_name, user_id, fingerprint, tags, breadcrumbs, payload
) VALUES (
    $1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17, $18, $19, $20
);

-- List view never returns payload/breadcrumbs (large). Every filter is
-- optional: a NULL / empty-array / empty-object argument disables it.
-- name: ListEvents :many
SELECT id, occurred_at, level, message, platform, environment, release,
       device_id, device_model, os_version, screen, error_type, error_location,
       handled, sdk_name, user_id, fingerprint, tags
FROM events
WHERE (cardinality(@levels::text[]) = 0 OR level = ANY(@levels::text[]))
  AND (sqlc.narg('since')::timestamptz IS NULL OR occurred_at >= sqlc.narg('since'))
  AND (sqlc.narg('until')::timestamptz IS NULL OR occurred_at < sqlc.narg('until'))
  AND (sqlc.narg('device_id')::text IS NULL OR device_id = sqlc.narg('device_id'))
  AND (sqlc.narg('user_id')::text IS NULL
       OR user_id = sqlc.narg('user_id')
       OR device_id = ANY(@user_devices::text[]))
  AND (sqlc.narg('platform')::text IS NULL OR platform = sqlc.narg('platform'))
  AND (sqlc.narg('release')::text IS NULL OR release = sqlc.narg('release'))
  AND (sqlc.narg('error_type')::text IS NULL OR error_type = sqlc.narg('error_type'))
  AND (sqlc.narg('fingerprint')::text IS NULL OR fingerprint = sqlc.narg('fingerprint'))
  AND (NOT @crashes_only::boolean OR handled = false OR level = 'fatal')
  AND (sqlc.narg('device_model')::text IS NULL OR device_model = sqlc.narg('device_model'))
  AND (sqlc.narg('os_version')::text IS NULL OR os_version = sqlc.narg('os_version'))
  AND (sqlc.narg('error_location')::text IS NULL OR error_location ILIKE sqlc.narg('error_location'))
  AND (sqlc.narg('message')::text IS NULL OR message ILIKE sqlc.narg('message'))
  AND tags @> @tags::jsonb
ORDER BY occurred_at DESC, id DESC
LIMIT @page_limit OFFSET @page_offset;

-- name: GetEvent :one
SELECT * FROM events WHERE id = $1;

-- name: ListUserDevices :many
SELECT device_id FROM user_devices WHERE user_id = $1 ORDER BY last_seen DESC LIMIT 100;

-- name: UpsertUserDevice :batchexec
INSERT INTO user_devices (user_id, device_id, last_seen)
VALUES ($1, $2, $3)
ON CONFLICT (user_id, device_id) DO UPDATE
  SET last_seen = EXCLUDED.last_seen
  WHERE user_devices.last_seen < EXCLUDED.last_seen - interval '1 day';

-- Crash count in a precise window (alerting).
-- name: CountCrashesSince :one
SELECT count(*) FROM events
WHERE occurred_at >= $1 AND (level = 'fatal' OR handled = false);

-- Retention: bounded delete so a huge backlog never holds one long transaction.
-- name: DeleteEventsBefore :execrows
DELETE FROM events WHERE id IN (
    SELECT old.id FROM events old WHERE old.occurred_at < $1 ORDER BY old.occurred_at LIMIT $2
);

-- name: DeleteUserDevicesBefore :execrows
DELETE FROM user_devices WHERE last_seen < $1;
