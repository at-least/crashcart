-- Queries used only by the viewer (internal/web).

-- name: IssuesIntroducedPerRelease :many
-- How many issues were first seen on each release.
SELECT first_release AS release, count(*)::bigint AS n
FROM issues WHERE project_id = $1 AND first_release IS NOT NULL
GROUP BY first_release;

-- name: ListIssuesIntroducedIn :many
SELECT * FROM issues WHERE project_id = $1 AND first_release = $2
ORDER BY event_count DESC LIMIT $3;

-- name: ListIssuesPresentIn :many
-- Issues still open whose latest event came from this release.
SELECT * FROM issues WHERE project_id = $1 AND last_release = $2 AND status NOT IN ('resolved', 'ignored')
ORDER BY event_count DESC LIMIT $3;

-- name: LatestIssueEvent :one
SELECT * FROM events WHERE project_id = $1 AND fingerprint = $2 ORDER BY id DESC LIMIT 1;

-- name: OldestIssueEvent :one
SELECT * FROM events WHERE project_id = $1 AND fingerprint = $2 ORDER BY id ASC LIMIT 1;

-- name: LatestRelease :one
-- The release with the most recent activity in the window.
SELECT release FROM event_stats_hourly
WHERE project_id = $1 AND bucket >= $2 AND release <> ''
GROUP BY release ORDER BY max(bucket) DESC LIMIT 1;

-- name: DistinctReleases :many
SELECT release FROM event_stats_hourly
WHERE project_id = $1 AND bucket >= $2 AND release <> ''
GROUP BY release ORDER BY max(bucket) DESC LIMIT 50;
