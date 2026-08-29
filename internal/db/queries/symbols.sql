-- name: UpsertSymbolFile :one
INSERT INTO symbol_files (project_id, kind, release, debug_id, filename, size, data)
VALUES ($1, $2, $3, $4, $5, $6, $7)
ON CONFLICT (project_id, kind, release, filename) DO UPDATE SET
    debug_id = EXCLUDED.debug_id, size = EXCLUDED.size, data = EXCLUDED.data, uploaded_at = now()
RETURNING id, project_id, kind, release, debug_id, filename, size, uploaded_at;

-- name: ListSymbolFiles :many
SELECT id, project_id, kind, release, debug_id, filename, size, uploaded_at
FROM symbol_files WHERE project_id = $1 ORDER BY uploaded_at DESC;

-- name: SymbolFilesForRelease :many
SELECT * FROM symbol_files WHERE project_id = $1 AND release = $2 AND kind = $3;

-- name: SymbolFileByDebugID :one
SELECT * FROM symbol_files WHERE project_id = $1 AND debug_id = $2 LIMIT 1;

-- name: SymbolFileExists :one
SELECT EXISTS (SELECT 1 FROM symbol_files WHERE project_id = $1 AND kind = $2 AND (release = $3 OR debug_id = ANY($4::text[])));

-- name: DeleteSymbolFile :execrows
DELETE FROM symbol_files WHERE project_id = $1 AND id = $2;

-- name: ExpireSymbolFiles :execrows
DELETE FROM symbol_files WHERE uploaded_at < $1;

-- name: SetSymbolFileRelease :execrows
-- Tags a release onto mappings uploaded without one: the one identified by
-- debug_id, or (when debug_id is null) the project's recent ProGuard uploads.
UPDATE symbol_files SET release = sqlc.arg(release)
WHERE project_id = sqlc.arg(project_id) AND release = ''
  AND ((sqlc.narg(debug_id)::text IS NOT NULL AND debug_id = sqlc.narg(debug_id)::text)
    OR (sqlc.narg(debug_id)::text IS NULL AND kind = 'proguard' AND uploaded_at > sqlc.arg(since)));
