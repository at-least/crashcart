-- name: InsertSession :exec
INSERT INTO sessions (id, project_id, release, environment, status, count)
VALUES ($1, $2, $3, $4, $5, $6) ON CONFLICT (id) DO NOTHING;

-- name: ReleaseHealth :many
-- Per release over a window: sessions and crash-free rate inputs.
SELECT release,
       sum(total)::bigint   AS total,
       sum(crashed)::bigint AS crashed,
       sum(errored)::bigint AS errored
FROM release_health_daily
WHERE project_id = $1 AND bucket >= $2 AND bucket < $3
GROUP BY release;

-- name: ReleaseHealthDaily :many
SELECT bucket, sum(total)::bigint AS total, sum(crashed)::bigint AS crashed, sum(errored)::bigint AS errored
FROM release_health_daily
WHERE project_id = $1 AND release = $2 AND bucket >= $3 AND bucket < $4
GROUP BY bucket ORDER BY bucket;
