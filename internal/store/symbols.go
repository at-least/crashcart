package store

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5"
)

// SymbolFile is the full row (data/blob_key included) — exactly one of
// Data / BlobKey is set (the row's location, see
// internal/symbolicate/files.go).
type SymbolFile struct {
	ID         int64      `json:"id"`
	ProjectID  int64      `json:"project_id"`
	Kind       SymbolKind `json:"kind"`
	Release    *string    `json:"release"`
	DebugID    *string    `json:"debug_id"`
	Filename   string     `json:"filename"`
	Size       int64      `json:"size"`
	Data       []byte     `json:"data"`
	BlobKey    *string    `json:"blob_key"`
	UploadedAt time.Time  `json:"uploaded_at"`
}

const symbolFileColumns = "id, project_id, kind, release, debug_id, filename, size, data, blob_key, uploaded_at"

func scanSymbolFile(row pgx.Row) (SymbolFile, error) {
	var f SymbolFile
	err := row.Scan(&f.ID, &f.ProjectID, &f.Kind, &f.Release, &f.DebugID, &f.Filename, &f.Size, &f.Data, &f.BlobKey, &f.UploadedAt)
	return f, err
}

// SymbolFileMeta is a symbol_files row without its bytes (data/blob_key) —
// list/metadata views that never need to move the payload.
type SymbolFileMeta struct {
	ID         int64      `json:"id"`
	ProjectID  int64      `json:"project_id"`
	Kind       SymbolKind `json:"kind"`
	Release    *string    `json:"release"`
	DebugID    *string    `json:"debug_id"`
	Filename   string     `json:"filename"`
	Size       int64      `json:"size"`
	UploadedAt time.Time  `json:"uploaded_at"`
}

const symbolFileMetaColumns = "id, project_id, kind, release, debug_id, filename, size, uploaded_at"

func scanSymbolFileMeta(row pgx.Row) (SymbolFileMeta, error) {
	var f SymbolFileMeta
	err := row.Scan(&f.ID, &f.ProjectID, &f.Kind, &f.Release, &f.DebugID, &f.Filename, &f.Size, &f.UploadedAt)
	return f, err
}

// UpsertSymbolFileParams: exactly one of Data / BlobKey is set (the row's
// location, see internal/symbolicate/files.go); DO UPDATE sets both so a
// re-upload can move a row between the two.
type UpsertSymbolFileParams struct {
	ProjectID int64
	Kind      SymbolKind
	Release   *string
	DebugID   *string
	Filename  string
	Size      int64
	Data      []byte
	BlobKey   *string
}

func UpsertSymbolFile(ctx context.Context, db DB, p UpsertSymbolFileParams) (SymbolFileMeta, error) {
	return scanSymbolFileMeta(db.QueryRow(ctx, `INSERT INTO symbol_files (project_id, kind, release, debug_id, filename, size, data, blob_key)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
		ON CONFLICT (project_id, kind, release, filename) DO UPDATE SET
		    debug_id = EXCLUDED.debug_id, size = EXCLUDED.size, data = EXCLUDED.data, blob_key = EXCLUDED.blob_key, uploaded_at = now()
		RETURNING `+symbolFileMetaColumns,
		p.ProjectID, p.Kind, p.Release, p.DebugID, p.Filename, p.Size, p.Data, p.BlobKey))
}

// SymbolFileBlobKey: the blob_key of the row an upload is about to
// replace (its unique key; IS NOT DISTINCT FROM so a NULL-release dSYM
// row matches).
func SymbolFileBlobKey(ctx context.Context, db DB, projectID int64, kind SymbolKind, release *string, filename string) (*string, error) {
	var blobKey *string
	err := db.QueryRow(ctx, "SELECT blob_key FROM symbol_files WHERE project_id = $1 AND kind = $2 AND release IS NOT DISTINCT FROM $4::text AND filename = $3",
		projectID, kind, filename, release).Scan(&blobKey)
	return blobKey, err
}

// LockSymbolFile serializes writers of one symbol_files row (its unique
// key as text) for the transaction: the previous blob_key read by
// SymbolFileBlobKey is then the one the upsert replaces, whatever a
// concurrent upload does.
func LockSymbolFile(ctx context.Context, db DB, key string) error {
	_, err := db.Exec(ctx, "SELECT pg_advisory_xact_lock(hashtext($1::text))", key)
	return err
}

// SymbolFileBlobKeys: every blob a project's symbol files point at;
// deleted after the project is.
func SymbolFileBlobKeys(ctx context.Context, db DB, projectID int64) ([]string, error) {
	rows, err := db.Query(ctx, "SELECT blob_key::text FROM symbol_files WHERE project_id = $1 AND blob_key IS NOT NULL", projectID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	items := []string{}
	for rows.Next() {
		var k string
		if err := rows.Scan(&k); err != nil {
			return nil, err
		}
		items = append(items, k)
	}
	return items, rows.Err()
}

func ListSymbolFiles(ctx context.Context, db DB, projectID int64) ([]SymbolFileMeta, error) {
	rows, err := db.Query(ctx, "SELECT "+symbolFileMetaColumns+" FROM symbol_files WHERE project_id = $1 ORDER BY uploaded_at DESC", projectID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	items := []SymbolFileMeta{}
	for rows.Next() {
		f, err := scanSymbolFileMeta(rows)
		if err != nil {
			return nil, err
		}
		items = append(items, f)
	}
	return items, rows.Err()
}

func SymbolFilesForRelease(ctx context.Context, db DB, projectID int64, kind SymbolKind, release string) ([]SymbolFile, error) {
	rows, err := db.Query(ctx, "SELECT "+symbolFileColumns+" FROM symbol_files WHERE project_id = $1 AND release = $3::text AND kind = $2", projectID, kind, release)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	items := []SymbolFile{}
	for rows.Next() {
		f, err := scanSymbolFile(rows)
		if err != nil {
			return nil, err
		}
		items = append(items, f)
	}
	return items, rows.Err()
}

func SymbolFileByDebugID(ctx context.Context, db DB, projectID int64, debugID *string) (SymbolFile, error) {
	return scanSymbolFile(db.QueryRow(ctx, "SELECT "+symbolFileColumns+" FROM symbol_files WHERE project_id = $1 AND debug_id = $2 LIMIT 1", projectID, debugID))
}

func SymbolFileExists(ctx context.Context, db DB, projectID int64, kind SymbolKind, release string, debugIDs []string) (bool, error) {
	var exists bool
	err := db.QueryRow(ctx, "SELECT EXISTS (SELECT 1 FROM symbol_files WHERE project_id = $1 AND kind = $2 AND (release = $3::text OR debug_id = ANY($4::text[])))",
		projectID, kind, release, debugIDs).Scan(&exists)
	return exists, err
}

// DeleteSymbolFile returns blob_key so the caller can delete the blob
// after commit (pgx.ErrNoRows: no such file).
func DeleteSymbolFile(ctx context.Context, db DB, projectID, id int64) (*string, error) {
	var blobKey *string
	err := db.QueryRow(ctx, "DELETE FROM symbol_files WHERE project_id = $1 AND id = $2 RETURNING blob_key", projectID, id).Scan(&blobKey)
	return blobKey, err
}

func ExpireSymbolFiles(ctx context.Context, db DB, before time.Time) ([]*string, error) {
	rows, err := db.Query(ctx, "DELETE FROM symbol_files WHERE uploaded_at < $1 RETURNING blob_key", before)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	items := []*string{}
	for rows.Next() {
		var k *string
		if err := rows.Scan(&k); err != nil {
			return nil, err
		}
		items = append(items, k)
	}
	return items, rows.Err()
}

// SetSymbolFileRelease tags a release onto mappings uploaded without one:
// the one identified by debug_id, or (when debug_id is null) the
// project's recent ProGuard uploads.
func SetSymbolFileRelease(ctx context.Context, db DB, release string, projectID int64, debugID *string, since time.Time) (int64, error) {
	tag, err := db.Exec(ctx, `UPDATE symbol_files SET release = $1
		WHERE project_id = $2 AND release IS NULL
		  AND (($3::text IS NOT NULL AND debug_id = $3::text)
		    OR ($3::text IS NULL AND kind = 'proguard' AND uploaded_at > $4))`,
		release, projectID, debugID, since)
	if err != nil {
		return 0, err
	}
	return tag.RowsAffected(), nil
}

// SymbolFileMetaByDebugID is the dSYM path: the row without its data (the
// sidecar keeps the bytes, SymbolFileData fetches them only when it does
// not have them yet).
func SymbolFileMetaByDebugID(ctx context.Context, db DB, projectID int64, debugID *string) (SymbolFileMeta, error) {
	return scanSymbolFileMeta(db.QueryRow(ctx, "SELECT "+symbolFileMetaColumns+" FROM symbol_files WHERE project_id = $1 AND debug_id = $2 LIMIT 1", projectID, debugID))
}

func SymbolFileMetasForRelease(ctx context.Context, db DB, projectID int64, kind SymbolKind, release string) ([]SymbolFileMeta, error) {
	rows, err := db.Query(ctx, "SELECT "+symbolFileMetaColumns+" FROM symbol_files WHERE project_id = $1 AND release = $3::text AND kind = $2", projectID, kind, release)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	items := []SymbolFileMeta{}
	for rows.Next() {
		f, err := scanSymbolFileMeta(rows)
		if err != nil {
			return nil, err
		}
		items = append(items, f)
	}
	return items, rows.Err()
}

// SymbolFilesVersionRow is the rows behind one mapping cache key (a
// release and kind, or a debug id), as a count and the latest upload: a
// cached mapping is served while this is unchanged.
type SymbolFilesVersionRow struct {
	N      int64
	Latest time.Time
}

func SymbolFilesVersion(ctx context.Context, db DB, projectID int64, kind SymbolKind, debugID, release string) (SymbolFilesVersionRow, error) {
	var r SymbolFilesVersionRow
	err := db.QueryRow(ctx, `SELECT count(*)::bigint AS n, COALESCE(max(uploaded_at), 'epoch'::timestamptz)::timestamptz AS latest
		FROM symbol_files
		WHERE project_id = $1 AND kind = $2
		  AND (($3::text <> '' AND debug_id = $3::text)
		    OR ($3::text = '' AND release = $4::text))`,
		projectID, kind, debugID, release).Scan(&r.N, &r.Latest)
	return r, err
}

// SymbolFileDataRow is the row's location: data, or the blob_key to fetch
// it by.
type SymbolFileDataRow struct {
	Data    []byte
	BlobKey *string
}

func SymbolFileData(ctx context.Context, db DB, id int64) (SymbolFileDataRow, error) {
	var r SymbolFileDataRow
	err := db.QueryRow(ctx, "SELECT data, blob_key FROM symbol_files WHERE id = $1", id).Scan(&r.Data, &r.BlobKey)
	return r, err
}
