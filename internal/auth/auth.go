// Package auth holds the HTTP middleware: API bearer keys, the ingest
// token (DSN-style), rate limiting and CORS.
package auth

import (
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"net"
	"net/http"
	"strconv"
	"strings"

	"github.com/newlix/crashcart/internal/ratelimit"
)

// equal is a constant-time string comparison.
func equal(a, b string) bool {
	return subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1
}

// APIKey requires `Authorization: Bearer <key>` when keys is non-empty.
func APIKey(keys []string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		if len(keys) == 0 {
			return next
		}
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			token, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
			if !ok {
				http.Error(w, "missing Authorization header", http.StatusUnauthorized)
				return
			}
			for _, k := range keys {
				if equal(k, token) {
					next.ServeHTTP(w, r)
					return
				}
			}
			http.Error(w, "invalid API key", http.StatusUnauthorized)
		})
	}
}

// IngestToken extracts the credential a Sentry SDK sends. Any of:
//   - ?token=…            (CrashCart DSN: http://TOKEN@host/ingest?token=…)
//   - ?sentry_key=…       (standard Sentry query auth)
//   - X-Sentry-Auth: Sentry sentry_key=…, sentry_version=7
func IngestToken(r *http.Request) string {
	q := r.URL.Query()
	if t := q.Get("token"); t != "" {
		return t
	}
	if t := q.Get("sentry_key"); t != "" {
		return t
	}
	for _, part := range strings.Split(r.Header.Get("X-Sentry-Auth"), ",") {
		part = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(part), "Sentry "))
		if v, ok := strings.CutPrefix(part, "sentry_key="); ok {
			return strings.TrimSpace(v)
		}
	}
	return ""
}

// Ingest requires the ingest token when one is configured.
func Ingest(token string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		if token == "" {
			return next
		}
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if !equal(IngestToken(r), token) {
				http.Error(w, "invalid ingest token", http.StatusUnauthorized)
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

// RateKey identifies the caller for rate limiting: API key, ingest token
// or client IP — credentials are digested so the key is never the secret.
func RateKey(r *http.Request) string {
	if t, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer "); ok {
		return "api:" + digest(t)
	}
	if t := IngestToken(r); t != "" {
		return "ingest:" + digest(t)
	}
	return "ip:" + ClientIP(r)
}

// ClientIP prefers X-Forwarded-For / X-Real-IP (reverse proxy), then RemoteAddr.
func ClientIP(r *http.Request) string {
	if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
		if first, _, _ := strings.Cut(xff, ","); strings.TrimSpace(first) != "" {
			return strings.TrimSpace(first)
		}
	}
	if ip := r.Header.Get("X-Real-IP"); ip != "" {
		return ip
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

func digest(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:16])
}

// RateLimit applies l per RateKey and sets X-RateLimit-* headers. Register
// it AFTER auth so only authenticated callers consume buckets.
func RateLimit(l *ratelimit.Limiter) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		if l == nil {
			return next
		}
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			d := l.Allow(RateKey(r))
			h := w.Header()
			h.Set("X-RateLimit-Limit", strconv.Itoa(d.Limit))
			h.Set("X-RateLimit-Remaining", strconv.Itoa(d.Remaining))
			h.Set("X-RateLimit-Reset", strconv.FormatInt(d.Reset.Unix(), 10))
			if !d.Allowed {
				http.Error(w, "rate limit exceeded", http.StatusTooManyRequests)
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

// CORS answers preflights and stamps Access-Control-* on responses.
func CORS(origin string) func(http.Handler) http.Handler {
	if origin == "" {
		origin = "*"
	}
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			h := w.Header()
			h.Set("Access-Control-Allow-Origin", origin)
			h.Set("Access-Control-Allow-Methods", "GET, POST, PATCH, OPTIONS")
			h.Set("Access-Control-Allow-Headers", "Authorization, Content-Type, X-Sentry-Auth")
			if origin != "*" {
				h.Add("Vary", "Origin")
			}
			if r.Method == http.MethodOptions {
				w.WriteHeader(http.StatusNoContent)
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

// Chain applies middlewares left-to-right (first wraps outermost).
func Chain(h http.Handler, mws ...func(http.Handler) http.Handler) http.Handler {
	for i := len(mws) - 1; i >= 0; i-- {
		h = mws[i](h)
	}
	return h
}
