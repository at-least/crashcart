-- Insert-side values describe the first event of the fingerprint in this
-- batch; on conflict counters accumulate, last_* follow the newest event,
-- and a resolved issue that reappears in a different release regresses.
-- name: UpsertIssue :batchexec
INSERT INTO issues (fingerprint, title, level, error_type, screen, platform, event_count,
                    first_seen, last_seen, first_release, last_release)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11)
ON CONFLICT (fingerprint) DO UPDATE SET
    event_count  = issues.event_count + EXCLUDED.event_count,
    first_seen   = LEAST(issues.first_seen, EXCLUDED.first_seen),
    last_seen    = GREATEST(issues.last_seen, EXCLUDED.last_seen),
    last_release = CASE WHEN EXCLUDED.last_seen >= issues.last_seen
                        THEN COALESCE(EXCLUDED.last_release, issues.last_release)
                        ELSE issues.last_release END,
    status       = CASE WHEN issues.status = 'resolved'
                         AND EXCLUDED.last_release IS DISTINCT FROM issues.last_release
                        THEN 'regression' ELSE issues.status END,
    updated_at   = now();

-- Optional filters. Event-scoped filtering (release/user/device/…) is done
-- by the caller: FingerprintsInRange → @fingerprints (empty = no filter).
-- name: ListIssues :many
SELECT * FROM issues i
WHERE i.last_seen >= @since
  AND (sqlc.narg('until')::timestamptz IS NULL OR i.first_seen < sqlc.narg('until'))
  AND (sqlc.narg('error_type')::text IS NULL OR i.error_type = sqlc.narg('error_type'))
  AND (sqlc.narg('status')::text IS NULL OR i.status = sqlc.narg('status'))
  AND (NOT @by_fingerprint::boolean OR i.fingerprint = ANY(@fingerprints::text[]))
ORDER BY i.last_seen DESC
LIMIT @page_limit;

-- name: GetIssue :one
SELECT * FROM issues WHERE fingerprint = $1;

-- name: UpdateIssueStatus :execrows
UPDATE issues SET status = $2, updated_at = now() WHERE fingerprint = $1;

-- Alerting: issues first seen since a point in time.
-- name: NewIssuesSince :many
SELECT fingerprint, title, error_type FROM issues
WHERE first_seen >= $1 AND level IN ('error', 'fatal')
ORDER BY first_seen DESC LIMIT 5;

-- name: RegressionsSince :many
SELECT fingerprint, title, error_type, last_release FROM issues
WHERE status = 'regression' AND last_seen >= $1
ORDER BY last_seen DESC LIMIT 5;

-- name: DeleteIssuesBefore :execrows
DELETE FROM issues WHERE last_seen < $1;
