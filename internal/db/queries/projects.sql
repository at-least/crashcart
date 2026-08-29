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
-- A new DSN key: the old one stops authenticating within the ingest cache TTL.
UPDATE projects SET public_key = $2 WHERE id = $1 RETURNING *;

-- name: DeleteProject :exec
DELETE FROM projects WHERE id = $1;
