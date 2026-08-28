-- name: UpsertSymbolFile :one
INSERT INTO symbol_files (platform, release, filename, size, data)
VALUES ($1, $2, $3, $4, $5)
ON CONFLICT (platform, release, filename) DO UPDATE SET
    size = EXCLUDED.size, data = EXCLUDED.data, uploaded_at = now()
RETURNING id, platform, release, filename, size, uploaded_at;

-- name: ListSymbolFiles :many
SELECT id, platform, release, filename, size, uploaded_at
FROM symbol_files ORDER BY platform, release, filename;

-- Newest upload wins when several files exist for a (platform, release).
-- name: LatestSymbolFile :one
SELECT id, platform, release, filename, size, uploaded_at
FROM symbol_files WHERE platform = $1 AND release = $2
ORDER BY uploaded_at DESC LIMIT 1;

-- name: GetSymbolFileData :one
SELECT data FROM symbol_files WHERE id = $1;
