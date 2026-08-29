-- Queries used only by internal/api.

-- name: NewIssuesByRelease :many
-- Issues first seen in the window, grouped by the release that introduced them.
SELECT first_release AS release, count(*)::bigint AS n
FROM issues
WHERE project_id = $1 AND first_seen >= $2 AND first_seen < $3 AND first_release IS NOT NULL
GROUP BY first_release;

-- name: CountRegressions :one
-- Issues currently in 'regression' that were seen in the window.
SELECT count(*) FROM issues WHERE project_id = $1 AND status = 'regression' AND last_seen >= $2;

-- name: ReleaseHealthNN :many
-- Like ReleaseHealth, with 0 (not NULL) totals when the window is empty.
SELECT release,
       COALESCE(sum(total), 0)::bigint   AS total,
       COALESCE(sum(crashed), 0)::bigint AS crashed,
       COALESCE(sum(errored), 0)::bigint AS errored
FROM release_health_hourly
WHERE project_id = $1 AND bucket >= $2 AND bucket < $3
GROUP BY release;

-- name: ReleaseHealthDailyNN :many
SELECT crashcart_bucket(bucket, 86400)::timestamptz AS bucket,
       COALESCE(sum(total), 0)::bigint   AS total,
       COALESCE(sum(crashed), 0)::bigint AS crashed,
       COALESCE(sum(errored), 0)::bigint AS errored
FROM release_health_hourly
WHERE project_id = $1 AND release = $2 AND bucket >= $3 AND bucket < $4
GROUP BY 1 ORDER BY 1;
