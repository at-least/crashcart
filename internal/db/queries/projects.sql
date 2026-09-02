-- name: ListProjects :many
SELECT * FROM projects ORDER BY name;

-- name: GetProject :one
SELECT * FROM projects WHERE slug = $1;

-- name: GetProjectByID :one
SELECT * FROM projects WHERE id = $1;

-- name: GetProjectByKey :one
SELECT * FROM projects WHERE public_key = $1;

-- name: CreateProject :one
INSERT INTO projects (slug, name, platform, public_key)
VALUES ($1, $2, $3, $4) RETURNING *;

-- name: UpdateProject :one
UPDATE projects SET name = $2, platform = $3, sample_keep_first = $4, sample_rate = $5, daily_quota = $6
WHERE id = $1 RETURNING *;

-- name: RotateProjectKey :one
-- The raw column update; store.RotateProjectKey wraps this in a
-- transaction that first retires the outgoing key into project_keys.
UPDATE projects SET public_key = $2 WHERE id = $1 RETURNING *;

-- name: DeleteProject :exec
DELETE FROM projects WHERE id = $1;

-- name: RetireProjectKey :exec
-- The current key of a project, pushed into project_keys before Rotate
-- overwrites it — it keeps authenticating (GetProjectByRetiredKey) until
-- someone deletes the row explicitly.
INSERT INTO project_keys (project_id, public_key) SELECT p.id, p.public_key FROM projects p WHERE p.id = $1;

-- name: ListProjectKeys :many
-- A project's retired-but-still-valid keys, newest retirement first.
SELECT * FROM project_keys WHERE project_id = $1 ORDER BY retired_at DESC;

-- name: DeleteProjectKey :execrows
DELETE FROM project_keys WHERE project_id = $1 AND id = $2;

-- name: GetProjectByRetiredKey :one
-- The ingest fallback when a key isn't the current one: still valid until
-- its project_keys row is deleted. key_id is what TouchProjectKey needs.
SELECT k.id AS key_id, sqlc.embed(p) FROM project_keys k JOIN projects p ON p.id = k.project_id WHERE k.public_key = $1;

-- name: TouchProjectKey :exec
-- Records use of a retired key at most once a minute (one write per key
-- per minute, not per request) — the fact that answers "is it safe to
-- delete this now".
UPDATE project_keys SET last_used_at = now()
WHERE id = $1 AND (last_used_at IS NULL OR last_used_at < now() - INTERVAL '1 minute');
