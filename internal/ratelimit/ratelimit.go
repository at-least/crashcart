// Package ratelimit is an in-memory fixed-window limiter (60s windows).
//
// CrashCart runs as a single process, so per-key counters live in memory —
// no table, no extra round trip per request. Keys are hashed by the caller
// (see auth.RateKey) so no credential is ever held here.
package ratelimit

import (
	"sync"
	"time"
)

const window = time.Minute

// Limiter counts requests per key per window.
type Limiter struct {
	limit int
	now   func() time.Time

	mu      sync.Mutex
	buckets map[string]*bucket
	pruneAt time.Time
}

type bucket struct {
	windowStart time.Time
	count       int
}

// Decision is the outcome of one Allow call.
type Decision struct {
	Allowed   bool
	Limit     int
	Remaining int
	Reset     time.Time
}

// New returns a limiter allowing `limit` requests per key per minute.
func New(limit int) *Limiter {
	return &Limiter{limit: limit, now: time.Now, buckets: map[string]*bucket{}}
}

// Allow records one request for key.
func (l *Limiter) Allow(key string) Decision {
	now := l.now()
	start := now.Truncate(window)

	l.mu.Lock()
	defer l.mu.Unlock()
	if now.After(l.pruneAt) {
		for k, b := range l.buckets {
			if b.windowStart.Before(start) {
				delete(l.buckets, k)
			}
		}
		l.pruneAt = now.Add(window)
	}
	b := l.buckets[key]
	if b == nil || b.windowStart.Before(start) {
		b = &bucket{windowStart: start}
		l.buckets[key] = b
	}
	b.count++
	return Decision{
		Allowed:   b.count <= l.limit,
		Limit:     l.limit,
		Remaining: max(0, l.limit-b.count),
		Reset:     start.Add(window),
	}
}

// SetClock overrides the time source (tests).
func (l *Limiter) SetClock(now func() time.Time) { l.now = now }
