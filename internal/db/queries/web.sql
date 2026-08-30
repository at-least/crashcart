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

-- The portal: one query per statistic across every project, not four per
-- project.

-- name: PortalUnhandled :many
-- Unhandled per project in a window (one row per project).
SELECT project_id, sum(unhandled)::bigint AS unhandled
FROM event_stats_hourly WHERE bucket >= sqlc.arg(from_at)::timestamptz AND bucket < sqlc.arg(to_at)::timestamptz
GROUP BY 1;

-- name: PortalPlatforms :many
-- Raw platforms per project in a window, most events first.
SELECT project_id, platform, sum(events)::bigint AS events
FROM event_stats_hourly WHERE bucket >= sqlc.arg(from_at)::timestamptz AND bucket < sqlc.arg(to_at)::timestamptz
GROUP BY 1, 2 ORDER BY 1, 3 DESC, 2;

-- name: PortalLatestReleases :many
-- The most recently active release per project (ties by name, like
-- LatestReleaseHealth).
SELECT DISTINCT ON (project_id) project_id, release
FROM (SELECT project_id, release, max(bucket) AS last FROM event_stats_hourly
      WHERE bucket >= sqlc.arg(from_at)::timestamptz AND bucket < sqlc.arg(to_at)::timestamptz AND release <> ''
      GROUP BY 1, 2) t
ORDER BY project_id, last DESC, release DESC;

-- name: PortalOpenIssues :many
SELECT project_id, count(*)::bigint AS n FROM issues
WHERE status IN ('unresolved', 'regression') GROUP BY 1;

-- name: PortalReleaseHealth :many
-- Session totals of one release per project (the latest active one).
SELECT k.project_id::bigint AS project_id, COALESCE(sum(h.total), 0)::bigint AS total, COALESCE(sum(h.crashed), 0)::bigint AS crashed
FROM (SELECT unnest(sqlc.arg(project_ids)::bigint[]) AS project_id, unnest(sqlc.arg(releases)::text[]) AS release) AS k
JOIN release_health_hourly h ON h.project_id = k.project_id AND h.release = k.release
  AND h.bucket >= sqlc.arg(from_at)::timestamptz AND h.bucket < sqlc.arg(to_at)::timestamptz
GROUP BY k.project_id;

-- name: LatestIssueEvent :one
-- Bounded to the issue's own [first_seen, last_seen] so only those
-- partitions are read (the issue row is the exact range of its events).
SELECT * FROM events WHERE project_id = $1 AND fingerprint = $2
  AND occurred_at >= sqlc.arg(from_at)::timestamptz AND occurred_at < sqlc.arg(to_at)::timestamptz
ORDER BY occurred_at DESC LIMIT 1;
