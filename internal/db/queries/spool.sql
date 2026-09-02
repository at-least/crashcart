-- Event payloads in the blob store: the spool ingest writes, the packs the
-- flusher builds from it, and where each packed event's bytes are
-- (internal/store/packs.go).

-- name: SpoolGroups :many
-- What the flusher chooses from: each (project, week) with spooled
-- payloads, its total size and its oldest row. The week is Monday 00:00
-- UTC like retention's partitions (date_trunc('week') is ISO, Monday-based).
SELECT project_id,
       date_trunc('week', occurred_at AT TIME ZONE 'UTC')::date AS week,
       sum(length(data))::bigint AS bytes,
       min(created_at)::timestamptz AS oldest
FROM payload_spool
GROUP BY project_id, date_trunc('week', occurred_at AT TIME ZONE 'UTC')
ORDER BY oldest;

-- name: SpoolRows :many
-- One group's rows in export order (payload_spool_order), oldest week
-- boundary [from, to): the flusher cuts at PackBytes in Go.
SELECT project_id, event_id, occurred_at, data
FROM payload_spool
WHERE project_id = $1 AND occurred_at >= sqlc.arg(from_at) AND occurred_at < sqlc.arg(to_at)
ORDER BY occurred_at, event_id
LIMIT $2;

-- name: InsertPack :one
INSERT INTO packs (project_id, week) VALUES ($1, $2) RETURNING id;

-- name: SetPackBytes :exec
UPDATE packs SET bytes = $2 WHERE id = $1;

-- name: DeletePack :exec
DELETE FROM packs WHERE id = $1;

-- name: InsertEventPack :batchexec
INSERT INTO event_packs (project_id, event_id, occurred_at, pack_id, pack_offset, pack_len)
VALUES ($1, $2, $3, $4, $5, $6)
ON CONFLICT (project_id, event_id, occurred_at) DO UPDATE SET pack_id = EXCLUDED.pack_id, pack_offset = EXCLUDED.pack_offset, pack_len = EXCLUDED.pack_len;

-- name: DeleteSpoolRow :batchexec
DELETE FROM payload_spool WHERE project_id = $1 AND event_id = $2 AND occurred_at = $3;

-- name: PayloadLocation :one
-- One statement, one snapshot: the spool row if the event is not packed
-- yet, else its place in a pack; neither when it has no payload.
SELECT s.data AS spooled, p.pack_id, p.pack_offset, p.pack_len, k.week
FROM (SELECT sqlc.arg(project_id)::bigint AS project_id, sqlc.arg(event_id)::uuid AS event_id, sqlc.arg(occurred_at)::timestamptz AS occurred_at) e
LEFT JOIN payload_spool s ON s.project_id = e.project_id AND s.event_id = e.event_id AND s.occurred_at = e.occurred_at
LEFT JOIN event_packs p ON p.project_id = e.project_id AND p.event_id = e.event_id AND p.occurred_at = e.occurred_at
LEFT JOIN packs k ON k.id = p.pack_id;

-- name: ExpiredPacks :many
-- Packs of weeks past retention: the same rule as the partition drop
-- (a week is expired once its end is at or before the cutoff).
SELECT id, project_id, week FROM packs WHERE week + 7 <= sqlc.arg(cutoff)::date;

-- name: ExpireSpool :execrows
-- Spool rows of expired weeks (the partition rule, not a plain cutoff: a
-- weekly partition keeps rows up to a week past the cutoff, and a row
-- still in the spool — a bucket outage — is their only payload).
DELETE FROM payload_spool WHERE date_trunc('week', occurred_at AT TIME ZONE 'UTC')::date + 7 <= sqlc.arg(cutoff)::date;

-- name: ProjectPacks :many
-- A project's packs, read before the project (and, by cascade, the rows)
-- is deleted, so the objects can be deleted after.
SELECT id, week FROM packs WHERE project_id = $1;

-- name: SpoolCount :one
SELECT count(*)::bigint FROM payload_spool;
