-- name: GetEvent :one
SELECT * FROM events WHERE project_id = $1 AND id = $2;

-- name: GetEventByEventID :one
SELECT * FROM events WHERE project_id = $1 AND event_id = $2 ORDER BY id DESC LIMIT 1;

-- name: SetEventSymbols :exec
UPDATE events SET symbols = $3, symbolicated = true, fingerprint = $4, error_location = $5
WHERE project_id = $1 AND id = $2;

-- name: IssueUsers :many
-- Distinct users per issue in a window (index: project_id, fingerprint, id).
SELECT fingerprint, count(DISTINCT user_id)::bigint AS users
FROM events WHERE project_id = $1 AND fingerprint = ANY($2::text[]) AND id >= $3 AND id < $4 AND user_id IS NOT NULL
GROUP BY fingerprint;

-- name: IssueNeighbors :one
-- Latest and oldest stored event of an issue.
SELECT max(id)::bigint AS latest, min(id)::bigint AS oldest FROM events WHERE project_id = $1 AND fingerprint = $2;

-- name: ExistingEventIDs :many
SELECT id FROM events WHERE id = ANY($1::bigint[]);
