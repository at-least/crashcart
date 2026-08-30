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
-- Bounded to the issue's own [first_seen, last_seen] so only those
-- partitions are read (the issue row is the exact range of its events).
SELECT * FROM events WHERE project_id = $1 AND fingerprint = $2
  AND occurred_at >= sqlc.arg(from_at)::timestamptz AND occurred_at < sqlc.arg(to_at)::timestamptz
ORDER BY occurred_at DESC LIMIT 1;
