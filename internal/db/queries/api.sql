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

-- name: IssueEventRange :one
-- Latest and oldest stored event id of an issue (0 when none are stored).
SELECT COALESCE(max(id), 0)::bigint AS latest, COALESCE(min(id), 0)::bigint AS oldest
FROM events WHERE project_id = $1 AND fingerprint = $2::text;

-- name: ReleaseHealthNN :many
-- Like ReleaseHealth, but crashed/errored are 0 (not NULL) when no session
-- of that status exists in the window (the cagg's FILTERed sums are NULL).
SELECT release,
       COALESCE(sum(total), 0)::bigint   AS total,
       COALESCE(sum(crashed), 0)::bigint AS crashed,
       COALESCE(sum(errored), 0)::bigint AS errored
FROM release_health_daily
WHERE project_id = $1 AND bucket >= $2 AND bucket < $3
GROUP BY release;

-- name: ReleaseHealthDailyNN :many
SELECT bucket,
       COALESCE(sum(total), 0)::bigint   AS total,
       COALESCE(sum(crashed), 0)::bigint AS crashed,
       COALESCE(sum(errored), 0)::bigint AS errored
FROM release_health_daily
WHERE project_id = $1 AND release = $2 AND bucket >= $3 AND bucket < $4
GROUP BY bucket ORDER BY bucket;
