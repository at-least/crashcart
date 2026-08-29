-- name: InsertSession :exec
-- Updates of one session (same sid, same start) overwrite the status,
-- except that a terminal status is never downgraded to 'ok'.
INSERT INTO sessions (started_at, project_id, sid, release, environment, status, count)
VALUES ($1, $2, $3, $4, $5, $6, $7)
ON CONFLICT (project_id, sid, started_at) DO UPDATE SET
    status = CASE WHEN sessions.status = 'ok' OR EXCLUDED.status <> 'ok' THEN EXCLUDED.status ELSE sessions.status END;

-- name: ReleaseHealth :many
-- Per release over a window: sessions and crash-free rate inputs.
SELECT release,
       COALESCE(sum(total), 0)::bigint   AS total,
       COALESCE(sum(crashed), 0)::bigint AS crashed,
       COALESCE(sum(errored), 0)::bigint AS errored
FROM release_health_daily
WHERE project_id = $1 AND bucket >= $2 AND bucket < $3
GROUP BY release;

-- name: ReleaseHealthDaily :many
SELECT bucket, COALESCE(sum(total), 0)::bigint AS total, COALESCE(sum(crashed), 0)::bigint AS crashed, COALESCE(sum(errored), 0)::bigint AS errored
FROM release_health_daily
WHERE project_id = $1 AND release = $2 AND bucket >= $3 AND bucket < $4
GROUP BY bucket ORDER BY bucket;
