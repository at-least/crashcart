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
	if m := s.load(context.Background(), cacheKey{p.ID, KindProGuard, "new"}); m != nil {
		t.Fatalf("mapping for an unknown release = %v", m)
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
	s.cache[cacheKey{p.ID, KindProGuard, debugPrefix + "abc"}] = &cacheEntry{mapping: &ProGuardMapping{}, loadedAt: time.Now()}
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
