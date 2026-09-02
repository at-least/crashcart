package blob

import (
	"bytes"
	"context"
	"errors"
	"os"
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
	// Ranges: a payload out of a pack, from the start, the middle and the
	// end; an empty range; a missing key.
	for _, r := range [][2]int64{{0, 10}, {1000, 25}, {int64(len(big)) - 7, 7}, {5, 0}} {
		got, err := s.GetRange(ctx, "symbols/2/big", r[0], r[1])
		if err != nil || !bytes.Equal(got, big[r[0]:r[0]+r[1]]) {
			t.Fatalf("range %v: %d bytes %v", r, len(got), err)
		}
	}
	if _, err := s.GetRange(ctx, "symbols/9/nope", 0, 5); !errors.Is(err, ErrNotFound) {
		t.Fatalf("range of a missing key: %v", err)
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
