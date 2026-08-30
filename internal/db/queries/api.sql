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
