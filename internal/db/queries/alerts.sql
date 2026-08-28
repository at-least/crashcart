-- name: ListAlertTypes :many
SELECT * FROM alert_types ORDER BY type;

-- name: GetAlertType :one
SELECT * FROM alert_types WHERE type = $1;

-- name: SetAlertEnabled :execrows
UPDATE alert_types SET enabled = $2 WHERE type = $1;

-- name: MarkAlertTriggered :exec
UPDATE alert_types SET last_triggered = $2, cooldown_until = $3 WHERE type = $1;
