-- Attachment writes are pipelined by store.InsertAttachments (hand-written).

-- name: ListAttachments :many
-- An event's attachments without their bytes (the event page, the API).
-- The time keeps the lookup to one partition.
SELECT occurred_at, project_id, event_id, n, filename, content_type, attachment_type, size
FROM attachments WHERE project_id = $1 AND event_id = $2 AND occurred_at = $3 ORDER BY n;

-- name: GetAttachment :one
SELECT * FROM attachments WHERE project_id = $1 AND event_id = $2 AND occurred_at = $3 AND n = $4;
