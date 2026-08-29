-- Payload packs: packs is the set of pack objects being filled (closed =
-- false, any number, chosen with SKIP LOCKED so concurrent envelopes never
-- wait on each other) or waiting for upload (closed = true);
-- payload_spool holds the payloads of those packs, each at its offset.
-- The event row's pack_id / pack_offset / pack_len were written with them.

-- name: ClaimOpenPack :one
-- The fullest open pack no other transaction is writing to (its row lock
-- is ours until commit). ErrNoRows: open one (OpenPack).
SELECT id, next_offset FROM packs WHERE NOT closed
ORDER BY next_offset DESC LIMIT 1 FOR UPDATE SKIP LOCKED;

-- name: OpenPack :one
INSERT INTO packs DEFAULT VALUES RETURNING id, next_offset;

-- name: AdvancePack :one
-- Reserves n bytes in a claimed pack; the pack closes when that reaches
-- max_bytes. Rolling the transaction back returns the bytes: no gaps.
UPDATE packs SET next_offset = next_offset + sqlc.arg(n)::bigint, closed = next_offset + sqlc.arg(n)::bigint >= sqlc.arg(max_bytes)::bigint
WHERE id = $1 RETURNING (next_offset - sqlc.arg(n)::bigint)::bigint AS off, closed;

-- name: SpoolPayloads :exec
INSERT INTO payload_spool (pack_id, "offset", data, size)
SELECT d.pack_id, d.off, d.data, octet_length(d.data)
FROM (SELECT unnest(sqlc.arg(pack_ids)::bigint[]) AS pack_id, unnest(sqlc.arg(offsets)::int[]) AS off, unnest(sqlc.arg(datas)::bytea[]) AS data) AS d
ON CONFLICT (pack_id, "offset") DO NOTHING;

-- name: SpooledPayload :one
SELECT data FROM payload_spool WHERE pack_id = $1 AND "offset" = $2;

-- name: ClosedPacks :many
SELECT id FROM packs WHERE closed ORDER BY id;

-- name: LockClosedPack :one
-- Claims a closed pack for upload; another process's claim is skipped.
SELECT id FROM packs WHERE id = $1 AND closed FOR UPDATE SKIP LOCKED;

-- name: CloseOpenPacks :execrows
-- Everything waiting goes out now (the CLI after an import or seed).
UPDATE packs SET closed = true WHERE NOT closed;

-- name: PackRows :many
SELECT "offset", data FROM payload_spool WHERE pack_id = $1 ORDER BY "offset";

-- name: DeletePack :exec
-- The spool rows go with it (ON DELETE CASCADE).
DELETE FROM packs WHERE id = $1;

-- name: CountSpool :one
SELECT count(*) FROM payload_spool;

-- name: ExpireSpool :execrows
-- Rows older than retention: their events are gone (a pack that never
-- filled on a very quiet project).
DELETE FROM payload_spool WHERE created_at < $1;
