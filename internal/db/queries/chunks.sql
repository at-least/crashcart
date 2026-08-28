-- name: PutUploadChunk :exec
INSERT INTO upload_chunks (sha1, data) VALUES ($1, $2) ON CONFLICT (sha1) DO NOTHING;

-- name: UploadChunksPresent :many
SELECT sha1 FROM upload_chunks WHERE sha1 = ANY($1::text[]);

-- name: GetUploadChunk :one
SELECT data FROM upload_chunks WHERE sha1 = $1;

-- name: DeleteUploadChunks :exec
DELETE FROM upload_chunks WHERE sha1 = ANY($1::text[]);

-- name: ExpireUploadChunks :execrows
DELETE FROM upload_chunks WHERE created_at < $1;
