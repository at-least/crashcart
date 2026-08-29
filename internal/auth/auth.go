// Package auth holds the HTTP middleware: bearer keys for /api, basic auth
// for the viewer, CORS, and the Postgres-backed rate limiter.
package auth

import (
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/crashcartapp/crashcart/internal/db/sqlc"
	"github.com/crashcartapp/crashcart/internal/store"
)

// Chain applies middlewares right-to-left (the first listed runs outermost).
func Chain(h http.Handler, mws ...func(http.Handler) http.Handler) http.Handler {
	for i := len(mws) - 1; i >= 0; i-- {
		h = mws[i](h)
	}
	return h
}

// Bearer requires `Authorization: Bearer <key>` matching one of keys.
// An empty key list leaves the route open.
func Bearer(keys []string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		if len(keys) == 0 {
			return next
		}
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			tok := strings.TrimSpace(strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer"))
			if tok == "" || !matchAny(tok, keys) {
				w.Header().Set("WWW-Authenticate", `Bearer realm="crashcart"`)
				http.Error(w, `{"error":"unauthorized"}`, http.StatusUnauthorized)
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

// Basic requires HTTP basic auth with any username and the given password.
// An empty password leaves the route open.
func Basic(password string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		if password == "" {
			return next
		}
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			_, pw, ok := r.BasicAuth()
			if !ok || subtle.ConstantTimeCompare([]byte(pw), []byte(password)) != 1 {
				w.Header().Set("WWW-Authenticate", `Basic realm="crashcart"`)
				http.Error(w, "unauthorized", http.StatusUnauthorized)
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

func matchAny(tok string, keys []string) bool {
	ok := false
	for _, k := range keys {
		if subtle.ConstantTimeCompare([]byte(tok), []byte(k)) == 1 {
			ok = true
		}
	}
	return ok
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

// RateLimit enforces limit requests per fixed 60 s window per credential.
// Buckets are keyed by the SHA-256 of the credential; limit <= 0 disables.
func RateLimit(st *store.Store, limit int, cred Credential) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		if limit <= 0 {
			return next
		}
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			c := cred(r)
			if c == "" {
				c = "anon:" + clientIP(r)
			}
			sum := sha256.Sum256([]byte(c))
			now := time.Now().Unix()
			window := now - now%60
			n, err := st.BumpRateLimit(r.Context(), sqlc.BumpRateLimitParams{RlKey: hex.EncodeToString(sum[:]), WindowStart: window})
			if err != nil {
				http.Error(w, `{"error":"rate limiter unavailable"}`, http.StatusServiceUnavailable)
				return
			}
			remaining := int64(limit) - int64(n)
			if remaining < 0 {
				remaining = 0
			}
			h := w.Header()
			h.Set("X-RateLimit-Limit", strconv.Itoa(limit))
			h.Set("X-RateLimit-Remaining", strconv.FormatInt(remaining, 10))
			h.Set("X-RateLimit-Reset", strconv.FormatInt(window+60, 10))
			if int(n) > limit {
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

func clientIP(r *http.Request) string {
	if xf := r.Header.Get("X-Forwarded-For"); xf != "" {
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
