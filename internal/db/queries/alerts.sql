-- name: ListAlertRules :many
SELECT * FROM alert_rules WHERE project_id = $1 ORDER BY type;

-- name: GetAlertRule :one
SELECT * FROM alert_rules WHERE project_id = $1 AND type = $2;

-- name: UpsertAlertRule :one
INSERT INTO alert_rules (project_id, type, enabled, cooldown_minutes) VALUES ($1, $2, $3, $4)
ON CONFLICT (project_id, type) DO UPDATE SET enabled = EXCLUDED.enabled, cooldown_minutes = EXCLUDED.cooldown_minutes
RETURNING *;

-- name: EnsureAlertRules :exec
-- The three default rules (enabled, default cooldown); existing rows untouched.
INSERT INTO alert_rules (project_id, type, enabled, cooldown_minutes)
SELECT sqlc.arg(project_id)::bigint, t, true, sqlc.arg(cooldown_minutes)::int
FROM unnest(ARRAY['new_issue', 'regression', 'crash_spike']::alert_type[]) AS t
ON CONFLICT (project_id, type) DO NOTHING;

-- name: ClaimAlertRule :one
-- Claims the cooldown atomically (no row: disabled, cooling down, or no
-- rule) and returns the previous last_triggered, for UnclaimAlertRule
-- when nothing could be delivered. The self-join reads the row before
-- the update.
UPDATE alert_rules a SET last_triggered = now()
FROM alert_rules old
WHERE old.project_id = a.project_id AND old.type = a.type
  AND a.project_id = $1 AND a.type = $2 AND a.enabled
  AND (a.last_triggered IS NULL OR a.last_triggered < now() - make_interval(mins => a.cooldown_minutes))
RETURNING old.last_triggered AS previous;

-- name: UnclaimAlertRule :exec
-- Gives a claim back (nothing was delivered): the cooldown must not eat
-- the next alert too.
UPDATE alert_rules SET last_triggered = sqlc.narg(previous) WHERE project_id = $1 AND type = $2;

-- name: ListAlertChannels :many
SELECT * FROM alert_channels WHERE project_id = $1 ORDER BY id;

-- name: CreateAlertChannel :one
INSERT INTO alert_channels (project_id, kind, config) VALUES ($1, $2, $3) RETURNING *;

-- name: DeleteAlertChannel :execrows
DELETE FROM alert_channels WHERE project_id = $1 AND id = $2;
