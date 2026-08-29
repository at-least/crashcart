package blob

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"
)

// s3Test returns a client on TEST_S3_* (a MinIO, say) or skips.
func s3Test(t *testing.T) *S3 {
	t.Helper()
	cfg := S3Config{
		Bucket: os.Getenv("TEST_S3_BUCKET"), Endpoint: os.Getenv("TEST_S3_ENDPOINT"),
		AccessKey: os.Getenv("TEST_S3_ACCESS_KEY"), SecretKey: os.Getenv("TEST_S3_SECRET_KEY"), Prefix: "t/" + t.Name(),
	}
	if cfg.Bucket == "" || cfg.Endpoint == "" {
		t.Skip("TEST_S3_BUCKET / TEST_S3_ENDPOINT not set")
	}
	s, err := NewS3(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.EnsureBucket(context.Background()); err != nil {
		t.Fatal(err)
	}
	return s
}

func TestS3RoundTrip(t *testing.T) {
	s := s3Test(t)
	ctx := context.Background()
	key := SymbolKey(7, 42)
	if key != "symbols/7/42" {
		t.Fatalf("key = %q", key)
	}
	if _, err := s.Get(ctx, key); !errors.Is(err, ErrNotFound) {
		t.Fatalf("get missing: %v", err)
	}
	want := []byte(`{"event_id":"x","message":"hello hello hello hello"}`)
	if err := s.Put(ctx, key, want); err != nil {
		t.Fatal(err)
	}
	got, err := s.Get(ctx, key)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(want) {
		t.Fatalf("got %q", got)
	}
	// Keys with characters that need encoding sign correctly too.
	odd := "symbols/1/My App.dSYM+é"
	if err := s.Put(ctx, odd, []byte("x")); err != nil {
		t.Fatal(err)
	}
	if b, err := s.Get(ctx, odd); err != nil || string(b) != "x" {
		t.Fatalf("odd key: %q %v", b, err)
	}
	if err := s.Delete(ctx, key); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Get(ctx, key); !errors.Is(err, ErrNotFound) {
		t.Fatalf("get after delete: %v", err)
	}
	if err := s.Delete(ctx, key); err != nil {
		t.Fatalf("delete twice: %v", err)
	}
	if err := s.SetLifecycle(ctx, []LifecycleRule{{ID: "events", Prefix: PrefixEvents, Days: 37}, {ID: "chunks", Prefix: PrefixChunks, Days: 1}}); err != nil {
		t.Fatal(err)
	}
	// Packs: raw bytes, ranged reads, 404 / 416 as ErrNotFound.
	if err := s.PutRaw(ctx, "events/2026-08-30/pack1", []byte("0123456789")); err != nil {
		t.Fatal(err)
	}
	if b, err := s.GetRange(ctx, "events/2026-08-30/pack1", 3, 4); err != nil || string(b) != "3456" {
		t.Fatalf("range = %q %v", b, err)
	}
	if _, err := s.GetRange(ctx, "events/2026-08-30/pack1", 20, 4); !errors.Is(err, ErrNotFound) {
		t.Fatalf("range past end: %v", err)
	}
	if _, err := s.GetRange(ctx, "events/2026-08-30/nope", 0, 4); !errors.Is(err, ErrNotFound) {
		t.Fatalf("range of missing: %v", err)
	}
	testPacker(t, s)
}

// testPacker runs the pack round trip on any store.
func testPacker(t *testing.T, s Store) {
	t.Helper()
	ctx := context.Background()
	var payloads [][]byte
	for i := 0; i < 3; i++ {
		payloads = append(payloads, []byte(fmt.Sprintf(`{"event_id":"%d","message":"payload number %d"}`, i, i)))
	}
	// Laid out with a gap after the first member (a rolled-back envelope).
	key := PackKey(time.Now())
	var members []PackMember
	var refs []Ref
	off := int64(0)
	for i, p := range payloads {
		gz := Gzip(p)
		if i == 1 {
			off += 100
		}
		members = append(members, PackMember{Off: off, Data: gz})
		refs = append(refs, NewRef(key, off, int64(len(gz))))
		off += int64(len(gz))
	}
	data := AssemblePack(members)
	if int64(len(data)) != off {
		t.Fatalf("pack is %d bytes, want %d", len(data), off)
	}
	// Not uploaded yet: the refs point at a pack that does not exist.
	if _, err := ReadRef(ctx, s, string(refs[0])); !errors.Is(err, ErrNotFound) {
		t.Fatalf("before upload: %v", err)
	}
	if err := s.PutRaw(ctx, key, data); err != nil {
		t.Fatal(err)
	}
	for i, ref := range refs {
		key, _, _, ok := ParseRef(string(ref))
		if !ok || !strings.HasPrefix(key, PrefixEvents) {
			t.Fatalf("ref %q", ref)
		}
		b, err := ReadRef(ctx, s, string(ref))
		if err != nil {
			t.Fatal(err)
		}
		if want := fmt.Sprintf(`{"event_id":"%d","message":"payload number %d"}`, i, i); string(b) != want {
			t.Fatalf("payload %d = %q", i, b)
		}
	}
	if PackKey(time.Now()) == key {
		t.Fatal("pack keys repeat")
	}
}

func TestPackerMemory(t *testing.T) {
	testPacker(t, NewMemory())
	if _, _, _, ok := ParseRef("events/2026-08-30/abc#12#x"); ok {
		t.Fatal("bad ref parsed")
	}
	if k, off, n, ok := ParseRef("events/2026-08-30/abc#12#34"); !ok || k != "events/2026-08-30/abc" || off != 12 || n != 34 {
		t.Fatalf("parse = %q %d %d %v", k, off, n, ok)
	}
}

func TestMemory(t *testing.T) {
	m := NewMemory()
	ctx := context.Background()
	if _, err := m.Get(ctx, "k"); !errors.Is(err, ErrNotFound) {
		t.Fatal(err)
	}
	m.Put(ctx, "k", []byte("v"))
	if b, _ := m.Get(ctx, "k"); string(b) != "v" {
		t.Fatal(string(b))
	}
	m.Delete(ctx, "k")
	if m.Len() != 0 {
		t.Fatal("not deleted")
	}
}
