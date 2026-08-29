-- name: PutUploadChunk :exec
INSERT INTO upload_chunks (sha1, data) VALUES ($1, $2) ON CONFLICT (sha1) DO NOTHING;

-- name: UploadChunksPresent :many
SELECT sha1 FROM upload_chunks WHERE sha1 = ANY($1::text[]);

-- name: GetUploadChunks :many
-- The chunks of one file, in the order the assemble request lists them.
SELECT c.sha1, c.data FROM unnest(sqlc.arg(sha1s)::text[]) WITH ORDINALITY AS w(sha1, n)
JOIN upload_chunks c ON c.sha1 = w.sha1 ORDER BY w.n;

-- name: DeleteUploadChunks :exec
DELETE FROM upload_chunks WHERE sha1 = ANY($1::text[]);

-- name: ExpireUploadChunks :execrows
DELETE FROM upload_chunks WHERE created_at < $1;
