package auth

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestRateLimitWindow(t *testing.T) {
	h := RateLimit("test", 2, IPCredential(false))(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(204) }))
	get := func(ip string) *httptest.ResponseRecorder {
		req := httptest.NewRequest("GET", "/", nil)
		req.RemoteAddr = ip + ":1234"
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec
	}
	if r := get("10.0.0.1"); r.Code != 204 || r.Header().Get("X-RateLimit-Remaining") != "1" {
		t.Fatalf("first: %d %v", r.Code, r.Header())
	}
	if r := get("10.0.0.1"); r.Code != 204 || r.Header().Get("X-RateLimit-Remaining") != "0" {
		t.Fatalf("second: %d %v", r.Code, r.Header())
	}
	if r := get("10.0.0.1"); r.Code != 429 || r.Header().Get("Retry-After") == "" {
		t.Fatalf("third must be limited: %d %v", r.Code, r.Header())
	}
	if r := get("10.0.0.2"); r.Code != 204 {
		t.Fatalf("other credential has its own bucket: %d", r.Code)
	}
	// A new window starts fresh.
	l := &limiter{}
	if n, _ := l.bump("k", 60); n != 1 {
		t.Fatal(n)
	}
	if n, _ := l.bump("k", 61); n != 2 {
		t.Fatal(n)
	}
	if n, w := l.bump("k", 120); n != 1 || w != 120 {
		t.Fatalf("new window: n=%d w=%d", n, w)
	}
	if len(l.counts) != 1 {
		t.Fatal("old window must be dropped")
	}
}
