-- name: UpsertUserReport :exec
-- One report per event, as the protocol requires: a resend overwrites
-- (received_at keeps the report's original arrival time).
INSERT INTO user_reports (project_id, event_id, name, email, comments)
VALUES ($1, $2, $3, $4, $5)
ON CONFLICT (project_id, event_id) DO UPDATE SET
    name = EXCLUDED.name, email = EXCLUDED.email, comments = EXCLUDED.comments;

-- name: GetUserReport :one
-- Whether an event has a report, for the event page / API — no join to
-- events (the report outlives a sampled-out or not-yet-arrived event).
SELECT * FROM user_reports WHERE project_id = $1 AND event_id = $2;

-- name: ListUserReports :many
-- Newest first, project-scoped: the Feedback page and its API endpoint.
-- Not tied to events, so a report whose event was sampled out still shows.
SELECT * FROM user_reports WHERE project_id = $1 ORDER BY received_at DESC, event_id DESC LIMIT $2 OFFSET $3;

-- name: CountUserReports :one
SELECT count(*) FROM user_reports WHERE project_id = $1;

-- name: SweepUserReports :execrows
-- Its own expiry (no join to events): a report survives independently of
-- whether its event was ever stored, so it is cut by its own clock.
DELETE FROM user_reports WHERE received_at < $1;
