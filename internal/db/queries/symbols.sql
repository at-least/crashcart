-- name: UpsertSymbolFile :one
-- Exactly one of data / blob_key is set (the row's location, see
-- internal/symbolicate/files.go); DO UPDATE sets both so a re-upload can
-- move a row between the two.
INSERT INTO symbol_files (project_id, kind, release, debug_id, filename, size, data, blob_key)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
ON CONFLICT (project_id, kind, release, filename) DO UPDATE SET
    debug_id = EXCLUDED.debug_id, size = EXCLUDED.size, data = EXCLUDED.data, blob_key = EXCLUDED.blob_key, uploaded_at = now()
RETURNING id, project_id, kind, release, debug_id, filename, size, uploaded_at;

-- name: SymbolFileBlobKey :one
-- The blob_key of the row an upload is about to replace (its unique key;
-- IS NOT DISTINCT FROM so a NULL-release dSYM row matches).
SELECT blob_key FROM symbol_files
WHERE project_id = $1 AND kind = $2 AND release IS NOT DISTINCT FROM sqlc.narg(release)::text AND filename = $3;

-- name: LockSymbolFile :exec
-- Serializes writers of one symbol_files row (its unique key as text) for
-- the transaction: the previous blob_key read by SymbolFileBlobKey is then
-- the one the upsert replaces, whatever a concurrent upload does.
SELECT pg_advisory_xact_lock(hashtext(sqlc.arg(key)::text));

-- name: SymbolFileBlobKeys :many
-- Every blob a project's symbol files point at: deleted after the project is.
SELECT blob_key::text FROM symbol_files WHERE project_id = $1 AND blob_key IS NOT NULL;

-- name: ListSymbolFiles :many
SELECT id, project_id, kind, release, debug_id, filename, size, uploaded_at
FROM symbol_files WHERE project_id = $1 ORDER BY uploaded_at DESC;

-- name: SymbolFilesForRelease :many
SELECT * FROM symbol_files WHERE project_id = $1 AND release = sqlc.arg(release)::text AND kind = $2;

-- name: SymbolFileByDebugID :one
SELECT * FROM symbol_files WHERE project_id = $1 AND debug_id = $2 LIMIT 1;

-- name: SymbolFileExists :one
SELECT EXISTS (SELECT 1 FROM symbol_files WHERE project_id = $1 AND kind = $2 AND (release = sqlc.arg(release)::text OR debug_id = ANY(sqlc.arg(debug_ids)::text[])));

-- name: DeleteSymbolFile :one
-- RETURNING blob_key so the caller can delete the blob after commit
-- (pgx.ErrNoRows: no such file).
DELETE FROM symbol_files WHERE project_id = $1 AND id = $2 RETURNING blob_key;

-- name: ExpireSymbolFiles :many
DELETE FROM symbol_files WHERE uploaded_at < $1 RETURNING blob_key;

-- name: SetSymbolFileRelease :execrows
-- Tags a release onto mappings uploaded without one: the one identified by
-- debug_id, or (when debug_id is null) the project's recent ProGuard uploads.
UPDATE symbol_files SET release = sqlc.arg(release)
WHERE project_id = sqlc.arg(project_id) AND release IS NULL
  AND ((sqlc.narg(debug_id)::text IS NOT NULL AND debug_id = sqlc.narg(debug_id)::text)
    OR (sqlc.narg(debug_id)::text IS NULL AND kind = 'proguard' AND uploaded_at > sqlc.arg(since)));

-- name: SymbolFileMetaByDebugID :one
-- The dSYM path: the row without its data (the sidecar keeps the bytes,
-- SymbolFileData fetches them only when it does not have them yet).
SELECT id, project_id, kind, release, debug_id, filename, size, uploaded_at
FROM symbol_files WHERE project_id = $1 AND debug_id = $2 LIMIT 1;

-- name: SymbolFileMetasForRelease :many
SELECT id, project_id, kind, release, debug_id, filename, size, uploaded_at
FROM symbol_files WHERE project_id = $1 AND release = sqlc.arg(release)::text AND kind = $2;

-- name: SymbolFilesVersion :one
-- The rows behind one mapping cache key (a release and kind, or a debug
-- id), as a count and the latest upload: a cached mapping is served while
-- this is unchanged.
SELECT count(*)::bigint AS n, COALESCE(max(uploaded_at), 'epoch'::timestamptz)::timestamptz AS latest
FROM symbol_files
WHERE project_id = $1 AND kind = $2
  AND ((sqlc.arg(debug_id)::text <> '' AND debug_id = sqlc.arg(debug_id)::text)
    OR (sqlc.arg(debug_id)::text = '' AND release = sqlc.arg(release)::text));

-- name: SymbolFileData :one
-- The row's location: data, or the blob_key to fetch it by.
SELECT data, blob_key FROM symbol_files WHERE id = $1;
