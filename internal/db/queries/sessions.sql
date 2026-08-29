-- Session writes are pipelined by store.InsertSessions (hand-written).

-- name: ReleaseHealth :many
-- Per release over a window: sessions and crash-free rate inputs.
SELECT release,
       COALESCE(sum(total), 0)::bigint   AS total,
       COALESCE(sum(crashed), 0)::bigint AS crashed,
       COALESCE(sum(errored), 0)::bigint AS errored
FROM release_health_hourly
WHERE project_id = $1 AND bucket >= $2 AND bucket < $3
GROUP BY release;

-- name: ReleaseHealthDaily :many
-- Per UTC day (from_at day-aligned).
SELECT crashcart_bucket(bucket, 86400)::timestamptz AS bucket, COALESCE(sum(total), 0)::bigint AS total, COALESCE(sum(crashed), 0)::bigint AS crashed, COALESCE(sum(errored), 0)::bigint AS errored
FROM release_health_hourly
WHERE project_id = $1 AND release = $2 AND bucket >= $3 AND bucket < $4
GROUP BY 1 ORDER BY 1;
