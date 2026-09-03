package symbolicate

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/at-least/crashcart/internal/blob"
	"github.com/at-least/crashcart/internal/config"
	"github.com/at-least/crashcart/internal/retention"
	"github.com/at-least/crashcart/internal/store"
	"github.com/at-least/crashcart/internal/testdb"
)

const mappingA = "com.example.Foo -> a.b:\n    void bar() -> c\n"
const mappingB = "com.example.Baz -> d.e:\n    void qux() -> f\n"

// location is where a symbol_files row keeps its bytes.
func location(t *testing.T, s *Service, id int64) (data []byte, key *string) {
	t.Helper()
	if err := s.Store.Pool.QueryRow(context.Background(), "SELECT data, blob_key FROM symbol_files WHERE id = $1", id).Scan(&data, &key); err != nil {
		t.Fatal(err)
	}
	return data, key
}

// TestBlobStoreRows: with a store configured an upload writes the object
// and a row that points at it (data NULL); a row written the old way
// (data, no key) is read alongside it; a re-upload replaces the object
// and deletes the one it replaced; delete removes row and object.
func TestBlobStoreRows(t *testing.T) {
	st := testdb.New(t)
	ctx := context.Background()
	p, err := store.CreateProject(ctx, st.Pool, "app", "App", nil, "k")
	if err != nil {
		t.Fatal(err)
	}
	mem := &blob.Memory{}
	st.Blobs = mem
	s := &Service{Store: st, DSYM: NewDSYMClient("")}

	rows, err := s.Upload(ctx, p.ID, "1.0", KindProGuard, "mapping.txt", []byte(mappingA))
	if err != nil || len(rows) != 1 {
		t.Fatalf("upload: %v %v", rows, err)
	}
	data, key := location(t, s, rows[0].ID)
	if data != nil || key == nil || !strings.HasPrefix(*key, "symbols/") {
		t.Fatalf("row location: data=%v key=%v", data, key)
	}
	if got, err := mem.Get(ctx, *key); err != nil || string(got) != mappingA {
		t.Fatalf("object: %q %v", got, err)
	}
	// Symbolication reads through the store.
	m, _, err := s.fetch(ctx, cacheKey{projectID: p.ID, kind: KindProGuard, key: "1.0"})
	if err != nil || m == nil || len(m.(*ProGuardMapping).Classes) != 1 {
		t.Fatalf("fetch through the store: %v %v", m, err)
	}

	// Mixed state: a second file of the release written the old way.
	if _, err := st.Pool.Exec(ctx, `INSERT INTO symbol_files (project_id, kind, release, filename, size, data) VALUES ($1, 'proguard', '1.0', 'legacy.txt', $2, $3)`, p.ID, len(mappingB), []byte(mappingB)); err != nil {
		t.Fatal(err)
	}
	m, _, err = s.fetch(ctx, cacheKey{projectID: p.ID, kind: KindProGuard, key: "1.0"})
	if err != nil || m == nil || len(m.(*ProGuardMapping).Classes) != 2 {
		t.Fatalf("mixed rows: %v %v", m, err)
	}

	// Re-upload: a new object, the previous one gone, the legacy row untouched.
	rows2, err := s.Upload(ctx, p.ID, "1.0", KindProGuard, "mapping.txt", []byte(mappingA+"\n# v2\n"))
	if err != nil || rows2[0].ID != rows[0].ID {
		t.Fatalf("re-upload: %v %v", rows2, err)
	}
	_, key2 := location(t, s, rows[0].ID)
	if key2 == nil || *key2 == *key {
		t.Fatalf("re-upload key: %v (was %s)", key2, *key)
	}
	if keys := mem.Keys(); len(keys) != 1 || keys[0] != *key2 {
		t.Fatalf("objects after re-upload: %v", keys)
	}

	// Delete: row and object.
	if found, err := s.DeleteSymbolFile(ctx, p.ID, rows[0].ID); err != nil || !found {
		t.Fatalf("delete: %v %v", found, err)
	}
	if found, err := s.DeleteSymbolFile(ctx, p.ID, rows[0].ID); err != nil || found {
		t.Fatalf("delete again: %v %v", found, err)
	}
	if keys := mem.Keys(); len(keys) != 0 {
		t.Fatalf("objects after delete: %v", keys)
	}
	// The other project's or the legacy row's absence of a key is not an error.
	if found, err := s.DeleteSymbolFile(ctx, p.ID+1, rows[0].ID); err != nil || found {
		t.Fatalf("delete on another project: %v %v", found, err)
	}
}

// TestBlobStoreExpireAndProjectDelete: the two other deletion paths.
func TestBlobStoreExpireAndProjectDelete(t *testing.T) {
	st := testdb.New(t)
	ctx := context.Background()
	p, _ := store.CreateProject(ctx, st.Pool, "app", "App", nil, "k")
	p2, _ := store.CreateProject(ctx, st.Pool, "other", "Other", nil, "k2")
	mem := &blob.Memory{}
	st.Blobs = mem
	s := &Service{Store: st, DSYM: NewDSYMClient("")}
	for _, c := range []struct {
		pid  int64
		name string
	}{{p.ID, "old.txt"}, {p.ID, "new.txt"}, {p2.ID, "other.txt"}} {
		if _, err := s.Upload(ctx, c.pid, "1.0", KindProGuard, c.name, []byte(mappingA)); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := st.Pool.Exec(ctx, "UPDATE symbol_files SET uploaded_at = now() - interval '100 days' WHERE filename = 'old.txt'"); err != nil {
		t.Fatal(err)
	}
	// Retention drops old.txt (2 × 7 days) and its object; the others stay.
	if err := retention.Sweep(ctx, st, config.Config{RetentionDays: 7}, slog.New(slog.NewTextHandler(&bytes.Buffer{}, nil))); err != nil {
		t.Fatal(err)
	}
	if keys := mem.Keys(); len(keys) != 2 {
		t.Fatalf("objects after sweep: %v", keys)
	}
	// Project delete: the rows cascade, the objects are deleted by the
	// caller from the keys it read first (internal/api does this).
	keys, err := store.SymbolFileBlobKeys(ctx, st.Pool, p.ID)
	if err != nil || len(keys) != 1 {
		t.Fatalf("project keys: %v %v", keys, err)
	}
	if err := store.DeleteProject(ctx, st.Pool, p.ID); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteBlobs(ctx, keys); err != nil {
		t.Fatal(err)
	}
	if got, err := mem.Get(ctx, keys[0]); !errors.Is(err, blob.ErrNotFound) {
		t.Fatalf("deleted project's object still there: %q %v", got, err)
	}
	if keys := mem.Keys(); len(keys) != 1 {
		t.Fatalf("other project's object must survive: %v", keys)
	}
}

// failing is a Store whose Put fails.
type failing struct {
	blob.Memory
	err error
}

func (f *failing) Put(ctx context.Context, key string, data []byte) error {
	if f.err != nil {
		return f.err
	}
	return f.Memory.Put(ctx, key, data)
}

// TestBlobStoreFailures: a store that cannot take the object leaves no
// row; a row that cannot be written leaves no object.
func TestBlobStoreFailures(t *testing.T) {
	st := testdb.New(t)
	ctx := context.Background()
	p, _ := store.CreateProject(ctx, st.Pool, "app", "App", nil, "k")
	f := &failing{err: errors.New("bucket down")}
	st.Blobs = f
	s := &Service{Store: st, DSYM: NewDSYMClient("")}
	if _, err := s.Upload(ctx, p.ID, "1.0", KindProGuard, "mapping.txt", []byte(mappingA)); err == nil || !strings.Contains(err.Error(), "bucket down") {
		t.Fatalf("put failure: %v", err)
	}
	if ok, _ := store.SymbolFileExists(ctx, st.Pool, p.ID, KindProGuard, "1.0", nil); ok {
		t.Fatal("a row was written although the object was not")
	}
	// Row failure: a project that does not exist (FK). The object written
	// first is removed again.
	f.err = nil
	if _, err := s.Upload(ctx, p.ID+99, "1.0", KindProGuard, "mapping.txt", []byte(mappingA)); err == nil {
		t.Fatal("upload for a missing project succeeded")
	}
	if keys := f.Keys(); len(keys) != 0 {
		t.Fatalf("object left behind after the row failed: %v", keys)
	}
	// Reading a blob row without a store configured is an error, not a
	// silently empty mapping.
	if _, err := s.Upload(ctx, p.ID, "1.0", KindProGuard, "mapping.txt", []byte(mappingA)); err != nil {
		t.Fatal(err)
	}
	st.Blobs = nil
	if _, _, err := s.fetch(ctx, cacheKey{projectID: p.ID, kind: KindProGuard, key: "1.0"}); !errors.Is(err, errNoBlobStore) {
		t.Fatalf("fetch without a store: %v", err)
	}
	st.Blobs = f
	// And the transient case: the object vanishes under a reader once.
	rows, _ := store.SymbolFilesForRelease(ctx, st.Pool, p.ID, KindProGuard, "1.0")
	f.Delete(ctx, *rows[0].BlobKey)
	if _, _, err := s.fetch(ctx, cacheKey{projectID: p.ID, kind: KindProGuard, key: "1.0"}); !errors.Is(err, blob.ErrNotFound) {
		t.Fatalf("gone object must be a transient error, got %v", err)
	}
	_ = time.Now
}
