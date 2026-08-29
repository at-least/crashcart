-- Session writes are pipelined by store.InsertSessions (hand-written).

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
