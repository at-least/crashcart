package blob

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// conformance is what every Store must do; each implementation runs it.
func conformance(t *testing.T, s Store) {
	t.Helper()
	ctx := context.Background()
	big := bytes.Repeat([]byte("com.example.Foo -> a.b:\n"), 200_000) // ~4.8 MB, a mapping-shaped body
	for _, c := range []struct {
		key  string
		data []byte
	}{
		{"symbols/1/aa", []byte("hello")},
		{"symbols/1/empty", []byte{}},
		{"symbols/2/big", big},
		{"symbols/3/My App.dSYM+é", []byte("odd name")}, // spaces, plus, unicode: signing and paths
	} {
		if err := s.Put(ctx, c.key, c.data); err != nil {
			t.Fatalf("put %q: %v", c.key, err)
		}
		got, err := s.Get(ctx, c.key)
		if err != nil || !bytes.Equal(got, c.data) {
			t.Fatalf("get %q: %d bytes, %v (want %d)", c.key, len(got), err, len(c.data))
		}
	}
	// Overwrite replaces.
	if err := s.Put(ctx, "symbols/1/aa", []byte("hello again")); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.Get(ctx, "symbols/1/aa"); string(got) != "hello again" {
		t.Fatalf("overwrite: %q", got)
	}
	// Missing is ErrNotFound, exactly.
	if _, err := s.Get(ctx, "symbols/9/nope"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing key: %v", err)
	}
	// Delete, then delete again: both fine, then gone.
	if err := s.Delete(ctx, "symbols/1/aa"); err != nil {
		t.Fatal(err)
	}
	if err := s.Delete(ctx, "symbols/1/aa"); err != nil {
		t.Fatalf("delete twice: %v", err)
	}
	if _, err := s.Get(ctx, "symbols/1/aa"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("after delete: %v", err)
	}
	// Other keys untouched.
	if got, err := s.Get(ctx, "symbols/2/big"); err != nil || !bytes.Equal(got, big) {
		t.Fatalf("big after unrelated delete: %d %v", len(got), err)
	}
	// Bad keys are refused before touching the store.
	for _, k := range []string{"", "/abs", "a//b", "a/../b", "./x"} {
		if err := s.Put(ctx, k, []byte("x")); err == nil {
			t.Errorf("key %q accepted", k)
		}
	}
}

func TestMemory(t *testing.T) {
	m := &Memory{}
	conformance(t, m)
	if keys := m.Keys(); len(keys) != 3 || keys[0] != "symbols/1/empty" {
		t.Fatalf("keys after conformance: %v", keys)
	}
}

func TestFS(t *testing.T) {
	dir := t.TempDir()
	f := &FS{Dir: dir}
	if err := f.Ping(context.Background()); err != nil {
		t.Fatal(err)
	}
	conformance(t, f)
	// Objects are plain files under Dir, and no temporaries are left behind.
	if _, err := os.Stat(filepath.Join(dir, "symbols", "2", "big")); err != nil {
		t.Fatal(err)
	}
	filepath.WalkDir(dir, func(p string, d os.DirEntry, err error) error {
		if strings.HasSuffix(p, ".tmp") {
			t.Errorf("temporary left behind: %s", p)
		}
		return nil
	})
	// A key can never escape Dir.
	if err := f.Put(context.Background(), "../escape", []byte("x")); err == nil {
		t.Fatal("traversal key accepted")
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(dir), "escape")); err == nil {
		t.Fatal("file written outside Dir")
	}
}

// TestS3 runs against a MinIO from cmd/testminio (make test-db) or any
// S3-compatible endpoint in TEST_S3_ENDPOINT; skipped otherwise.
func TestS3(t *testing.T) {
	ep := os.Getenv("TEST_S3_ENDPOINT")
	if ep == "" {
		t.Skip("TEST_S3_ENDPOINT not set")
	}
	ctx := context.Background()
	s, err := NewS3(ctx, S3Config{
		Bucket: "crashcart-test", Endpoint: ep, Region: "us-east-1",
		AccessKey: "crashcart", SecretKey: "crashcart12", Prefix: "t/" + strings.ToLower(t.Name()) + "/",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.EnsureBucket(ctx); err != nil {
		t.Fatal(err)
	}
	if err := s.Ping(ctx); err != nil {
		t.Fatal(err)
	}
	conformance(t, s)
	// A wrong bucket fails Ping, not the first upload.
	wrong, _ := NewS3(ctx, S3Config{Bucket: "crashcart-nope", Endpoint: ep, AccessKey: "crashcart", SecretKey: "crashcart12"})
	if err := wrong.Ping(ctx); err == nil {
		t.Fatal("Ping on a missing bucket succeeded")
	}
}
