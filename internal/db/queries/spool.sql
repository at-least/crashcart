-- Payload packs: packs is the set of pack objects being filled (closed =
-- false, any number, chosen with SKIP LOCKED so concurrent envelopes never
-- wait on each other) or waiting for upload (closed = true);
-- payload_spool holds the payloads of those packs, each at its offset.
-- The event row's payload_ref was written with them.

-- name: ClaimOpenPack :one
-- The fullest open pack no other transaction is writing to (its row lock
-- is ours until commit). ErrNoRows: open one (OpenPack).
SELECT pack_key, next_offset FROM packs WHERE NOT closed
ORDER BY next_offset DESC LIMIT 1 FOR UPDATE SKIP LOCKED;

-- name: OpenPack :one
INSERT INTO packs (pack_key) VALUES ($1) RETURNING pack_key, next_offset;

-- name: AdvancePack :one
-- Reserves n bytes in a claimed pack; the pack closes when that reaches
-- max_bytes. Rolling the transaction back returns the bytes: no gaps.
UPDATE packs SET next_offset = next_offset + sqlc.arg(n)::bigint, closed = next_offset + sqlc.arg(n)::bigint >= sqlc.arg(max_bytes)::bigint
WHERE pack_key = $1 RETURNING (next_offset - sqlc.arg(n)::bigint)::bigint AS off, closed;

-- name: SpoolPayloads :exec
INSERT INTO payload_spool (pack_key, "offset", data, size)
SELECT d.pack_key, d.off, d.data, octet_length(d.data)
FROM (SELECT unnest(sqlc.arg(pack_keys)::text[]) AS pack_key, unnest(sqlc.arg(offsets)::bigint[]) AS off, unnest(sqlc.arg(datas)::bytea[]) AS data) AS d
ON CONFLICT (pack_key, "offset") DO NOTHING;

-- name: SpooledPayload :one
SELECT data FROM payload_spool WHERE pack_key = $1 AND "offset" = $2;

-- name: ClosedPacks :many
SELECT pack_key FROM packs WHERE closed ORDER BY created_at;

-- name: LockClosedPack :one
-- Claims a closed pack for upload; another process's claim is skipped.
SELECT pack_key FROM packs WHERE pack_key = $1 AND closed FOR UPDATE SKIP LOCKED;

-- name: CloseOpenPacks :execrows
-- Everything waiting goes out now (the CLI after an import or seed).
UPDATE packs SET closed = true WHERE NOT closed;

-- name: PackRows :many
SELECT "offset", data FROM payload_spool WHERE pack_key = $1 ORDER BY "offset";

-- name: DeletePack :exec
DELETE FROM payload_spool WHERE pack_key = $1;

-- name: DeletePackRow :exec
DELETE FROM packs WHERE pack_key = $1;

-- name: CountSpool :one
SELECT count(*) FROM payload_spool;

-- name: ExpireSpool :execrows
-- Rows older than retention: their events are gone (a pack that never
-- filled on a very quiet project).
DELETE FROM payload_spool WHERE created_at < $1;
