-- name: ListAlertRules :many
SELECT * FROM alert_rules WHERE project_id = $1 ORDER BY type;

-- name: GetAlertRule :one
SELECT * FROM alert_rules WHERE project_id = $1 AND type = $2;

-- name: UpsertAlertRule :one
INSERT INTO alert_rules (project_id, type, enabled, cooldown_minutes) VALUES ($1, $2, $3, $4)
ON CONFLICT (project_id, type) DO UPDATE SET enabled = EXCLUDED.enabled, cooldown_minutes = EXCLUDED.cooldown_minutes
RETURNING *;

-- name: TouchAlertRule :execrows
-- Claims the cooldown atomically: succeeds only if it is not still cooling down.
UPDATE alert_rules SET last_triggered = now()
WHERE project_id = $1 AND type = $2 AND enabled
  AND (last_triggered IS NULL OR last_triggered < now() - make_interval(mins => cooldown_minutes));

-- name: ListAlertChannels :many
SELECT * FROM alert_channels WHERE project_id = $1 ORDER BY id;

-- name: CreateAlertChannel :one
INSERT INTO alert_channels (project_id, kind, config) VALUES ($1, $2, $3) RETURNING *;

-- name: DeleteAlertChannel :execrows
DELETE FROM alert_channels WHERE project_id = $1 AND id = $2;
