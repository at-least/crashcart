-- payload_spool: payloads written in the ingest transaction, drained into
-- packs in the object store by retention.PackPayloads.

-- name: SpoolPayloads :exec
INSERT INTO payload_spool (project_id, event_id, occurred_at, data, size)
SELECT sqlc.arg(project_id)::bigint, d.event_id, d.occurred_at, d.data, octet_length(d.data)
FROM (SELECT unnest(sqlc.arg(event_ids)::uuid[]) AS event_id, unnest(sqlc.arg(occurred_ats)::timestamptz[]) AS occurred_at, unnest(sqlc.arg(datas)::bytea[]) AS data) AS d
ON CONFLICT (project_id, event_id, occurred_at) DO NOTHING;

-- name: SpooledPayload :one
SELECT data FROM payload_spool WHERE project_id = $1 AND event_id = $2 AND occurred_at = $3;

-- name: ClaimSpool :many
-- One pack's worth, oldest first: the rows whose running size stays
-- under max_bytes (the first row always qualifies), at most max_rows.
-- The window runs over size only; data is read for the chosen rows. The
-- lock keeps another process from packing the same rows (they pack the
-- next batch instead).
SELECT s.project_id, s.event_id, s.occurred_at, s.data FROM payload_spool s
WHERE (s.project_id, s.event_id, s.occurred_at) IN (
    SELECT project_id, event_id, occurred_at FROM (
        SELECT project_id, event_id, occurred_at, size,
               sum(size) OVER (ORDER BY created_at, event_id) AS running
        FROM payload_spool ORDER BY created_at, event_id LIMIT sqlc.arg(max_rows)::int) AS w
    WHERE running - size < sqlc.arg(max_bytes)::bigint)
ORDER BY s.created_at, s.event_id
FOR UPDATE SKIP LOCKED;

-- name: SetPayloadRefs :exec
UPDATE events e SET payload_ref = r.ref
FROM (SELECT unnest(sqlc.arg(project_ids)::bigint[]) AS project_id, unnest(sqlc.arg(event_ids)::uuid[]) AS event_id,
             unnest(sqlc.arg(occurred_ats)::timestamptz[]) AS occurred_at, unnest(sqlc.arg(refs)::text[]) AS ref) AS r
WHERE e.project_id = r.project_id AND e.event_id = r.event_id AND e.occurred_at = r.occurred_at;

-- name: DeleteSpooled :exec
DELETE FROM payload_spool s
USING (SELECT unnest(sqlc.arg(project_ids)::bigint[]) AS project_id, unnest(sqlc.arg(event_ids)::uuid[]) AS event_id,
              unnest(sqlc.arg(occurred_ats)::timestamptz[]) AS occurred_at) AS r
WHERE s.project_id = r.project_id AND s.event_id = r.event_id AND s.occurred_at = r.occurred_at;

-- name: CountSpool :one
SELECT count(*) FROM payload_spool;

-- name: SpoolReady :one
-- Whether a pack is due: max_bytes of payloads waiting, or max_rows of
-- them. Cheap (sizes only, no data), checked before claiming.
SELECT (coalesce(sum(size), 0) >= sqlc.arg(max_bytes)::bigint OR count(*) >= sqlc.arg(max_rows)::int)::boolean AS ready
FROM (SELECT size FROM payload_spool LIMIT sqlc.arg(max_rows)::int) AS s;

-- name: ExpireSpool :execrows
-- Payloads whose events have been dropped by retention before a pack
-- filled up (a very quiet project).
DELETE FROM payload_spool WHERE created_at < $1;
