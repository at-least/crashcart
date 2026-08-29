-- name: GetEvent :one
-- By Sentry event_id (the newest row when a resend carried another timestamp).
SELECT * FROM events WHERE project_id = $1 AND event_id = $2 ORDER BY occurred_at DESC LIMIT 1;

-- name: SetEventSymbols :exec
UPDATE events SET symbols = $3, symbolicated = true, fingerprint = $4, error_location = $5
WHERE project_id = $1 AND event_id = $2;

-- name: IssueUsers :many
-- Distinct users per issue in a window (index: project_id, fingerprint, occurred_at).
SELECT fingerprint, count(DISTINCT user_id)::bigint AS users
FROM events WHERE project_id = $1 AND fingerprint = ANY($2::uuid[]) AND occurred_at >= $3 AND occurred_at < $4 AND user_id IS NOT NULL
GROUP BY fingerprint;

-- name: IssueEventRange :one
-- Newest and oldest stored event of an issue (NULL when none are stored).
SELECT (SELECT e.event_id FROM events e WHERE e.project_id = sqlc.arg(project_id)::bigint AND e.fingerprint = sqlc.arg(fingerprint)::uuid ORDER BY e.occurred_at DESC LIMIT 1)::uuid AS latest,
       (SELECT e.event_id FROM events e WHERE e.project_id = sqlc.arg(project_id)::bigint AND e.fingerprint = sqlc.arg(fingerprint)::uuid ORDER BY e.occurred_at ASC LIMIT 1)::uuid AS oldest;

-- name: ExistingEventIDs :many
-- Which of these event_ids are already stored (resent envelopes).
SELECT event_id FROM events WHERE project_id = $1 AND event_id = ANY($2::uuid[]);
