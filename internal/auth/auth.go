// Package auth holds the HTTP middleware — API keys for /api, session
// cookies for the viewer (see access.go), CORS, an in-memory rate limiter —
// and the credential helpers behind them.
package auth

import (
	"github.com/crashcartapp/crashcart/internal/metrics"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Chain applies middlewares right-to-left (the first listed runs outermost).
func Chain(h http.Handler, mws ...func(http.Handler) http.Handler) http.Handler {
	for i := len(mws) - 1; i >= 0; i-- {
		h = mws[i](h)
	}
	return h
}

// CORS answers preflights and stamps the allow headers.
func CORS(origin string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			h := w.Header()
			h.Set("Access-Control-Allow-Origin", origin)
			h.Set("Access-Control-Allow-Methods", "GET, POST, PUT, PATCH, DELETE, OPTIONS")
			h.Set("Access-Control-Allow-Headers", "Authorization, Content-Type, X-Sentry-Auth, HX-Request")
			if r.Method == http.MethodOptions {
				w.WriteHeader(http.StatusNoContent)
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

// Credential extracts the string a rate-limit bucket is keyed by.
type Credential func(r *http.Request) string

// limiter counts requests per credential in the current fixed 60 s window,
// in memory. The map is dropped at each window boundary, so it holds at
// most one minute of distinct credentials. Per process: with several
// replicas each enforces the limit on its own share of the traffic.
type limiter struct {
	mu     sync.Mutex
	window int64 // unix seconds, start of the counted window
	counts map[string]int
}

// bump counts one request for key at now and returns the count so far in
// the window plus the window start.
func (l *limiter) bump(key string, now int64) (n int, window int64) {
	window = now - now%60
	l.mu.Lock()
	defer l.mu.Unlock()
	if window != l.window {
		l.window, l.counts = window, map[string]int{}
	}
	l.counts[key]++
	return l.counts[key], window
}

// RateLimited counts 429s by scope (ingest, api, web).
var RateLimited = metrics.NewCounter("crashcart_rate_limited_total", "Requests refused with 429 by the in-memory rate limiter, by scope.", "scope")

// RateLimit enforces limit requests per fixed 60 s window per credential
// (in memory, per process); limit <= 0 disables. scope names the limiter
// in metrics.
func RateLimit(scope string, limit int, cred Credential) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		if limit <= 0 {
			return next
		}
		l := &limiter{}
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			c := cred(r)
			if c == "" {
				c = "anon:" + clientIP(r)
			}
			now := time.Now().Unix()
			n, window := l.bump(c, now)
			remaining := int64(limit) - int64(n)
			if remaining < 0 {
				remaining = 0
			}
			h := w.Header()
			h.Set("X-RateLimit-Limit", strconv.Itoa(limit))
			h.Set("X-RateLimit-Remaining", strconv.FormatInt(remaining, 10))
			h.Set("X-RateLimit-Reset", strconv.FormatInt(window+60, 10))
			if int(n) > limit {
				RateLimited.Inc(scope)
				h.Set("Retry-After", strconv.FormatInt(window+60-now, 10))
				http.Error(w, `{"error":"rate limited"}`, http.StatusTooManyRequests)
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

// BearerCredential keys buckets by the bearer token.
func BearerCredential(r *http.Request) string {
	return strings.TrimSpace(strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer"))
}

// IPCredential keys buckets by client IP (for unauthenticated HTML routes).
func IPCredential(r *http.Request) string { return "ip:" + clientIP(r) }

// TrustProxy: honour X-Forwarded-For (the first address) as the client IP.
// Off by default — a client can send the header itself and choose its own
// rate-limit bucket. Set from TRUST_PROXY at startup.
var TrustProxy bool

func clientIP(r *http.Request) string {
	if xf := r.Header.Get("X-Forwarded-For"); TrustProxy && xf != "" {
		if i := strings.IndexByte(xf, ','); i >= 0 {
			xf = xf[:i]
		}
		return strings.TrimSpace(xf)
	}
	host := r.RemoteAddr
	if i := strings.LastIndexByte(host, ':'); i >= 0 {
		host = host[:i]
	}
	return host
}

// SentryKey extracts the DSN public key from a Sentry SDK request: the
// X-Sentry-Auth header, the `sentry_key` query param, or the Authorization
// header in the same `Sentry sentry_key=…` form.
func SentryKey(r *http.Request) string {
	for _, h := range []string{r.Header.Get("X-Sentry-Auth"), r.Header.Get("Authorization")} {
		if k := sentryKeyFrom(h); k != "" {
			return k
		}
	}
	return r.URL.Query().Get("sentry_key")
}

func sentryKeyFrom(h string) string {
	h = strings.TrimSpace(h)
	if !strings.HasPrefix(h, "Sentry ") {
		return ""
	}
	for _, kv := range strings.Split(h[len("Sentry "):], ",") {
		k, v, ok := strings.Cut(strings.TrimSpace(kv), "=")
		if ok && strings.TrimSpace(k) == "sentry_key" {
			return strings.Trim(strings.TrimSpace(v), `"`)
		}
	}
	return ""
}
