package symbolicate

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/at-least/crashcart/internal/blob"
	"github.com/at-least/crashcart/internal/store"
)

// Where a symbol file's bytes live is a property of its row, not of the
// process: data (Postgres, the default) or blob_key (Service.Blobs, when
// BLOB_STORE=s3). A row is written the way the process is configured; it
// is read the way it was written, so a database holds both kinds after
// the backend changes and nothing has to be moved
// (internal/db/migrations/00001_baseline.sql).

// errNoBlobStore: a row points at the blob store, but this process has
// none configured.
var errNoBlobStore = errors.New("symbol file is in the blob store, but BLOB_STORE is not configured")

// LockKey names a symbol_files row's unique key for LockSymbolFile — the
// one text every writer of that row (upload, import) locks on.
func LockKey(projectID int64, kind string, release *string, filename string) string {
	rel := ""
	if release != nil {
		rel = *release
	}
	return fmt.Sprintf("symbol_files/%d/%s/%s/%s", projectID, kind, rel, filename)
}

// putSymbolFile stores data and upserts its row. With a blob store the
// object is written first, under a fresh key, outside any transaction (a
// 500 MB write must not hold one); the row then moves to it under an
// advisory lock on its unique key, so the previous blob_key read there is
// exactly the one the upsert replaces whatever a concurrent upload of the
// same file does — and is deleted only after the commit. A row failure
// deletes the new object; nothing points at it.
func (s *Service) putSymbolFile(ctx context.Context, p store.UpsertSymbolFileParams, data []byte) (store.SymbolFileMeta, error) {
	if s.Store.Blobs == nil {
		p.Data, p.BlobKey = data, nil
		return store.UpsertSymbolFile(ctx, s.Store.Pool, p)
	}
	key := blob.SymbolKey(p.ProjectID)
	if err := s.Store.Blobs.Put(ctx, key, data); err != nil {
		return store.SymbolFileMeta{}, fmt.Errorf("blob store: %w", err)
	}
	p.Data, p.BlobKey = nil, &key
	var row store.SymbolFileMeta
	var old *string
	err := s.Store.Tx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		if err := store.LockSymbolFile(ctx, tx, LockKey(p.ProjectID, string(p.Kind), p.Release, p.Filename)); err != nil {
			return err
		}
		prev, err := store.SymbolFileBlobKey(ctx, tx, p.ProjectID, p.Kind, p.Release, p.Filename)
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		old = prev
		row, err = store.UpsertSymbolFile(ctx, tx, p)
		return err
	})
	if err != nil {
		s.Store.Blobs.Delete(context.WithoutCancel(ctx), key) // best effort; nothing references it
		return store.SymbolFileMeta{}, err
	}
	s.deleteBlobs(context.WithoutCancel(ctx), old)
	return row, nil
}

// symbolBytes is a row's bytes: data as stored, or the object blob_key
// names. blob.ErrNotFound means a re-upload replaced the object between
// the row read and this call — the caller re-reads the row.
func (s *Service) symbolBytes(ctx context.Context, data []byte, blobKey *string) ([]byte, error) {
	if blobKey == nil {
		return data, nil
	}
	if s.Store.Blobs == nil {
		return nil, errNoBlobStore
	}
	return s.Store.Blobs.Get(ctx, *blobKey)
}

// deleteBlobs removes objects (nil keys are rows that live in Postgres) —
// after the rows that pointed at them are gone. Best effort: a failure
// leaves an orphaned object, never a row without its bytes; it is
// returned for the caller to log.
func (s *Service) deleteBlobs(ctx context.Context, keys ...*string) error {
	var first error
	for _, k := range keys {
		if k == nil {
			continue
		}
		if s.Store.Blobs == nil {
			if first == nil {
				first = fmt.Errorf("%w: object %s left behind", errNoBlobStore, *k)
			}
			continue
		}
		if err := s.Store.Blobs.Delete(ctx, *k); err != nil && first == nil {
			first = fmt.Errorf("delete blob %s: %w", *k, err)
		}
	}
	return first
}

// DeleteBlobs is deleteBlobs for callers outside the package that already
// removed the rows: the project delete (internal/api).
func (s *Service) DeleteBlobs(ctx context.Context, keys []string) error {
	ptrs := make([]*string, len(keys))
	for i := range keys {
		ptrs[i] = &keys[i]
	}
	return s.deleteBlobs(ctx, ptrs...)
}

// DeleteSymbolFile removes one symbol file: the row, then its object.
// found is false when the project has no such file.
func (s *Service) DeleteSymbolFile(ctx context.Context, projectID, id int64) (found bool, err error) {
	key, err := store.DeleteSymbolFile(ctx, s.Store.Pool, projectID, id)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, s.deleteBlobs(context.WithoutCancel(ctx), key)
}
