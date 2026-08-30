-- name: GetEvent :one
-- By Sentry event_id alone (the viewer and the API: URLs carry only the
-- id). Without a time this touches every partition; the newest row wins when
-- a resend carried another timestamp.
SELECT * FROM events WHERE project_id = $1 AND event_id = $2 ORDER BY occurred_at DESC LIMIT 1;

-- name: GetEventAt :one
-- By primary key: the time lets the planner open one partition. Jobs carry it.
SELECT * FROM events WHERE project_id = $1 AND event_id = $2 AND occurred_at = $3;

-- name: SetEventSymbols :exec
UPDATE events SET symbols = $3, symbolicated = true, fingerprint = $4, culprit = $5
WHERE project_id = $1 AND event_id = $2 AND occurred_at = $6;

-- name: IssueUsers :many
-- Distinct users per issue in a window (index: project_id, fingerprint, occurred_at).
SELECT fingerprint, count(DISTINCT user_id)::bigint AS users
FROM events WHERE project_id = $1 AND fingerprint = ANY($2::uuid[]) AND occurred_at >= $3 AND occurred_at < $4 AND user_id IS NOT NULL
GROUP BY fingerprint;

-- name: IssueEventRange :one
-- Newest and oldest stored event of an issue (NULL when none are stored),
-- within the issue's own [first_seen, last_seen] so only those partitions
-- are read.
SELECT (SELECT e.event_id FROM events e WHERE e.project_id = sqlc.arg(project_id)::bigint AND e.fingerprint = sqlc.arg(fingerprint)::uuid
          AND e.occurred_at >= sqlc.arg(from_at)::timestamptz AND e.occurred_at < sqlc.arg(to_at)::timestamptz ORDER BY e.occurred_at DESC LIMIT 1)::uuid AS latest,
       (SELECT e.event_id FROM events e WHERE e.project_id = sqlc.arg(project_id)::bigint AND e.fingerprint = sqlc.arg(fingerprint)::uuid
          AND e.occurred_at >= sqlc.arg(from_at)::timestamptz AND e.occurred_at < sqlc.arg(to_at)::timestamptz ORDER BY e.occurred_at ASC LIMIT 1)::uuid AS oldest;

-- name: ExistingEventIDs :many
-- Which of these event_ids are already stored (resent envelopes). A resend
-- carries the SDK's own timestamp, so the window is the envelope's own
-- time range: only the partitions it spans are read.
SELECT event_id FROM events
WHERE project_id = $1 AND event_id = ANY($2::uuid[]) AND occurred_at >= sqlc.arg(from_at)::timestamptz AND occurred_at < sqlc.arg(to_at)::timestamptz;

-- name: ExistingEventIDsAnyTime :many
-- The same without a window, for the few events whose timestamp was
-- replaced by the server's (a clock far off): a resend of those carries
-- a different time, so the stored copy can be in any partition.
SELECT event_id FROM events WHERE project_id = $1 AND event_id = ANY($2::uuid[]);
