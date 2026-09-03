package symbolicate

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/crashcartapp/crashcart/internal/testdb"
)

// TestCacheEviction: the mapping cache is bounded — a lookup that would
// grow it past cacheMax drops the least recently loaded entry first, and
// Invalidate drops the release's entries and every debug-id one.
func TestCacheEviction(t *testing.T) {
	st := testdb.New(t)
	p := newProject(t, st)
	s := &Service{Store: st, cache: map[cacheKey]*cacheEntry{}}
	base := time.Now().Add(-time.Hour)
	for i := 0; i < cacheMax; i++ {
		s.cache[cacheKey{p.ID, KindProGuard, fmt.Sprintf("r%d", i)}] = &cacheEntry{mapping: &ProGuardMapping{}, loadedAt: base.Add(time.Duration(i) * time.Second)}
	}
	oldest := cacheKey{p.ID, KindProGuard, "r0"}
	// Nothing uploaded for "new": a negative entry, cached like any other.
	if m, err := s.load(context.Background(), cacheKey{p.ID, KindProGuard, "new"}); m != nil || err != nil {
		t.Fatalf("mapping for an unknown release = %v %v", m, err)
	}
	if len(s.cache) != cacheMax {
		t.Fatalf("cache size = %d, want %d", len(s.cache), cacheMax)
	}
	if _, ok := s.cache[oldest]; ok {
		t.Error("the oldest entry must be the one evicted")
	}
	if _, ok := s.cache[cacheKey{p.ID, KindProGuard, "new"}]; !ok {
		t.Error("the new lookup must be cached")
	}
	if _, ok := s.cache[cacheKey{p.ID, KindProGuard, "r1"}]; !ok {
		t.Error("newer entries survive")
	}
	// A cached negative entry is served without a fetch until missTTL.
	s.cache[cacheKey{p.ID, KindProGuard, "new"}].loadedAt = time.Now().Add(-2 * missTTL)
	s.cache[cacheKey{p.ID, KindProGuard, debugPrefix + "abc"}] = &cacheEntry{mapping: &ProGuardMapping{}, loadedAt: time.Now(), checkedAt: time.Now()}
	s.load(context.Background(), cacheKey{p.ID, KindProGuard, "new"})
	if e := s.cache[cacheKey{p.ID, KindProGuard, "new"}]; e == nil || time.Since(e.loadedAt) > time.Minute {
		t.Error("an expired negative entry is re-fetched")
	}
	s.Invalidate(p.ID, "r1")
	if _, ok := s.cache[cacheKey{p.ID, KindProGuard, "r1"}]; ok {
		t.Error("Invalidate must drop the release")
	}
	if _, ok := s.cache[cacheKey{p.ID, KindProGuard, debugPrefix + "abc"}]; ok {
		t.Error("Invalidate must drop debug-id entries (a release may have been tagged onto one)")
	}
	if _, ok := s.cache[cacheKey{p.ID, KindProGuard, "r2"}]; !ok {
		t.Error("Invalidate must keep other releases")
	}
	// Empty cache: nothing to evict, no panic.
	(&Service{}).evictOldestLocked()
}

// TestCacheRevalidation: a parsed mapping is served until its rows change
// — an upload through another replica (Invalidate never reaches this
// process) must be picked up after revalidateTTL, and a mapping whose
// rows are unchanged is not re-read.
func TestCacheRevalidation(t *testing.T) {
	st := testdb.New(t)
	p := newProject(t, st)
	ctx := context.Background()
	s := &Service{Store: st}
	upload(t, st, p, KindProGuard, "1.0", "", "mapping.txt", []byte("com.example.A -> a.b:\n    void load() -> c\n"))
	k := cacheKey{p.ID, KindProGuard, "1.0"}
	v, err := s.load(ctx, k)
	m1, _ := v.(*ProGuardMapping)
	if err != nil || m1 == nil {
		t.Fatalf("load: %v %v", v, err)
	}
	// Rows unchanged: the same parsed object is served after the TTL.
	s.cache[k].checkedAt = time.Now().Add(-2 * revalidateTTL)
	if v, _ := s.load(ctx, k); v != any(m1) {
		t.Fatal("unchanged rows must keep the parsed mapping")
	}
	// A re-upload elsewhere (a new row, no Invalidate here) is seen once the TTL passes.
	upload(t, st, p, KindProGuard, "1.0", "", "mapping2.txt", []byte("com.example.B -> a.c:\n    void run() -> d\n"))
	if v, _ := s.load(ctx, k); v != any(m1) {
		t.Fatal("within the TTL the cached mapping is served")
	}
	s.cache[k].checkedAt = time.Now().Add(-2 * revalidateTTL)
	v, err = s.load(ctx, k)
	m2, _ := v.(*ProGuardMapping)
	if err != nil || m2 == nil || m2 == m1 || len(m2.Classes) != 2 {
		t.Fatalf("after the upload: %+v %v (same object: %v)", v, err, m2 == m1)
	}
	// A database failure while loading is an error, not "no mapping".
	s.cache = nil
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := s.load(cancelled, k); err == nil {
		t.Fatal("a failed load must be an error (the job retries), not a miss")
	}
}
