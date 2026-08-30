-- name: UpsertIssue :one
-- Called once per (project, fingerprint) per envelope with the folded
-- count; `releases` are the distinct releases of the folded events ('' for
-- none); `level` is the latest event's. Regression (only with regress =
-- true — ingest; symbolication moving an old event between issues is not
-- new evidence) is Sentry's "resolved in next release": a resolved issue
-- seen again on a release outside the set it had been seen on when it was
-- resolved (old builds in the field are inside that set; a fixed release
-- is not). Returns the row after the update plus whether it was
-- created in this call and whether this call flipped it to regression
-- (prev is the statement's snapshot of the row before the upsert).
WITH prev AS (SELECT status FROM issues WHERE project_id = $1 AND fingerprint = $2)
INSERT INTO issues (project_id, fingerprint, title, level, error_type, transaction, platform,
                    event_count, stored_count, first_seen, last_seen, first_release, last_release, releases)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $12, COALESCE(sqlc.arg(releases)::text[], '{}'))
ON CONFLICT (project_id, fingerprint) DO UPDATE SET
    event_count  = issues.event_count + EXCLUDED.event_count,
    stored_count = issues.stored_count + EXCLUDED.stored_count,
    last_seen    = GREATEST(issues.last_seen, EXCLUDED.last_seen),
    first_seen   = LEAST(issues.first_seen, EXCLUDED.first_seen),
    last_release = CASE WHEN EXCLUDED.last_seen >= issues.last_seen THEN COALESCE(EXCLUDED.last_release, issues.last_release) ELSE issues.last_release END,
    level        = CASE WHEN EXCLUDED.last_seen >= issues.last_seen THEN EXCLUDED.level ELSE issues.level END, -- the latest event's, as in Sentry
    releases     = CASE WHEN issues.releases @> EXCLUDED.releases THEN issues.releases
                        ELSE (SELECT array_agg(DISTINCT r ORDER BY r) FROM unnest(issues.releases || EXCLUDED.releases) AS r) END,
    status       = CASE WHEN sqlc.arg(regress)::bool AND issues.status = 'resolved'
                         AND NOT (COALESCE(issues.resolved_releases, '{}') @> EXCLUDED.releases)
                        THEN 'regression' ELSE issues.status END,
    updated_at   = now()
RETURNING *, (xmax = 0) AS created,
          COALESCE(issues.status = 'regression' AND (SELECT status FROM prev) = 'resolved', false)::bool AS regressed;

-- name: GetIssue :one
SELECT * FROM issues WHERE project_id = $1 AND fingerprint = $2;

-- name: SetIssueStatus :one
-- Resolving records the releases seen so far (regression detection).
UPDATE issues SET status = sqlc.arg(status)::issue_status, status_by = sqlc.narg(status_by),
    resolved_releases = CASE WHEN sqlc.arg(status)::issue_status = 'resolved' THEN releases ELSE resolved_releases END,
    updated_at = now()
WHERE project_id = $1 AND fingerprint = $2 RETURNING *;

-- name: SetIssuesStatus :execrows
UPDATE issues SET status = sqlc.arg(status)::issue_status, status_by = sqlc.narg(status_by),
    resolved_releases = CASE WHEN sqlc.arg(status)::issue_status = 'resolved' THEN releases ELSE resolved_releases END,
    updated_at = now()
WHERE project_id = $1 AND fingerprint = ANY($2::uuid[]);

-- name: AdjustIssueStoredCount :exec
UPDATE issues SET stored_count = GREATEST(0, stored_count + $3), event_count = GREATEST(0, event_count + $3), updated_at = now()
WHERE project_id = $1 AND fingerprint = $2;

-- name: DeleteEmptyIssue :exec
DELETE FROM issues WHERE project_id = $1 AND fingerprint = $2 AND event_count <= 0 AND status = 'unresolved';

-- name: CountIssuesByStatus :many
SELECT status, count(*) AS n FROM issues WHERE project_id = $1 GROUP BY status;

-- name: CountNewIssues :one
SELECT count(*) FROM issues WHERE project_id = $1 AND first_seen >= $2;

-- name: CountNewIssuesIn :one
-- New issues in [from, to).
SELECT count(*) FROM issues WHERE project_id = $1 AND first_seen >= sqlc.arg(from_at) AND first_seen < sqlc.arg(to_at);

-- name: ListRegressions :many
SELECT * FROM issues WHERE project_id = $1 AND status = 'regression' ORDER BY last_seen DESC LIMIT $2;

-- name: ListNewIssues :many
SELECT * FROM issues WHERE project_id = $1 AND first_seen >= $2 ORDER BY first_seen DESC LIMIT $3;

-- name: ListIssuesByRelease :many
SELECT * FROM issues WHERE project_id = $1 AND (first_release = $2 OR last_release = $2)
ORDER BY event_count DESC LIMIT $3;

-- name: IssueSparklines :many
-- Per fingerprint, the event counts of every bucket in the window as one
-- array (gap-filled, in bucket order); see the chart-query note in stats.sql.
WITH h AS (
    SELECT fingerprint, crashcart_bucket(bucket, sqlc.arg(width)::bigint) AS bucket, sum(events) AS events
    FROM issue_stats_hourly
    WHERE project_id = sqlc.arg(project_id)::bigint AND fingerprint = ANY(sqlc.arg(fingerprints)::uuid[])
      AND bucket >= sqlc.arg(from_at)::timestamptz AND bucket < sqlc.arg(to_at)::timestamptz
    GROUP BY 1, 2)
SELECT f.fingerprint::uuid AS fingerprint, array_agg(COALESCE(h.events, 0)::bigint ORDER BY b)::bigint[] AS counts
FROM unnest(sqlc.arg(fingerprints)::uuid[]) AS f(fingerprint)
CROSS JOIN crashcart_buckets(sqlc.arg(from_at)::timestamptz, sqlc.arg(to_at)::timestamptz, sqlc.arg(width)::bigint) AS b
LEFT JOIN h ON h.fingerprint = f.fingerprint AND h.bucket = b
GROUP BY f.fingerprint;

-- name: IssueTimeline :many
WITH h AS (
    SELECT crashcart_bucket(bucket, sqlc.arg(width)::bigint) AS bucket, sum(events) AS events
    FROM issue_stats_hourly
    WHERE project_id = sqlc.arg(project_id)::bigint AND fingerprint = sqlc.arg(fingerprint)::uuid
      AND bucket >= sqlc.arg(from_at)::timestamptz AND bucket < sqlc.arg(to_at)::timestamptz
    GROUP BY 1)
SELECT b::timestamptz AS bucket, COALESCE(h.events, 0)::bigint AS events
FROM crashcart_buckets(sqlc.arg(from_at)::timestamptz, sqlc.arg(to_at)::timestamptz, sqlc.arg(width)::bigint) AS b
LEFT JOIN h ON h.bucket = b
ORDER BY b;

-- name: AddIssueStored :exec
UPDATE issues SET stored_count = stored_count + $3 WHERE project_id = $1 AND fingerprint = $2;
