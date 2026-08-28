-- events.id encodes the event time (internal/pk); every query below bounds
-- the scan with an id range so it walks the PK — the table's only index.

-- name: InsertEvents :copyfrom
INSERT INTO events (
    id, event_id, level, message, platform, environment, release,
    device_id, device_model, os_version, screen, error_type, error_location,
    handled, sdk_name, user_id, fingerprint, tags, breadcrumbs, payload
) VALUES (
    $1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17, $18, $19, $20
);

-- List view never returns payload/breadcrumbs (large). Every filter is
-- optional: a NULL / empty-array / empty-object argument disables it.
-- name: ListEvents :many
SELECT id, to_timestamp(id / 1000000.0)::timestamptz AS occurred_at,
       level, message, platform, environment, release,
       device_id, device_model, os_version, screen, error_type, error_location,
       handled, sdk_name, user_id, fingerprint, tags
FROM events
WHERE id >= @since_id AND id < @until_id
  AND (cardinality(@levels::text[]) = 0 OR level = ANY(@levels::text[]))
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
ORDER BY id DESC
LIMIT @page_limit OFFSET @page_offset;

-- name: GetEvent :one
SELECT *, to_timestamp(id / 1000000.0)::timestamptz AS occurred_at FROM events WHERE id = $1;

-- Fingerprints of events in a window matching event-level filters — one
-- range scan; the caller then loads issues by key.
-- name: FingerprintsInRange :many
SELECT DISTINCT fingerprint FROM events
WHERE id >= @since_id AND id < @until_id AND fingerprint IS NOT NULL
  AND (sqlc.narg('release')::text IS NULL OR release = sqlc.narg('release'))
  AND (sqlc.narg('user_id')::text IS NULL OR user_id = sqlc.narg('user_id'))
  AND (sqlc.narg('device_id')::text IS NULL OR device_id = sqlc.narg('device_id'))
  AND (sqlc.narg('device_model')::text IS NULL OR device_model = sqlc.narg('device_model'))
  AND (sqlc.narg('os_version')::text IS NULL OR os_version = sqlc.narg('os_version'));

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
WHERE id >= @since_id AND (level = 'fatal' OR handled = false);

-- Retention: bounded delete so a huge backlog never holds one long transaction.
-- name: DeleteEventsBefore :execrows
DELETE FROM events WHERE id IN (
    SELECT old.id FROM events old WHERE old.id < @cutoff_id ORDER BY old.id LIMIT @batch
);

-- name: DeleteUserDevicesBefore :execrows
DELETE FROM user_devices WHERE last_seen < $1;
