package auth

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/newlix/crashcart/internal/ratelimit"
)

func ok() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
}

func TestAPIKey(t *testing.T) {
	h := APIKey([]string{"k1", "k2"})(ok())
	for _, tc := range []struct {
		header string
		want   int
	}{{"", 401}, {"Bearer nope", 401}, {"Bearer k2", 200}, {"Basic k2", 401}} {
		r := httptest.NewRequest("GET", "/api/events", nil)
		if tc.header != "" {
			r.Header.Set("Authorization", tc.header)
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != tc.want {
			t.Errorf("%q: got %d want %d", tc.header, w.Code, tc.want)
		}
	}
	// no keys configured → open
	w := httptest.NewRecorder()
	APIKey(nil)(ok()).ServeHTTP(w, httptest.NewRequest("GET", "/", nil))
	if w.Code != 200 {
		t.Error("open when unconfigured")
	}
}

func TestIngestTokenSources(t *testing.T) {
	h := Ingest("secret")(ok())
	cases := []struct {
		url, hdr string
		want     int
	}{
		{"/ingest", "", 401},
		{"/ingest?token=secret", "", 200},
		{"/api/1/envelope/?sentry_key=secret", "", 200},
		{"/api/1/envelope/", "Sentry sentry_version=7, sentry_key=secret, sentry_client=x", 200},
		{"/api/1/envelope/", "Sentry sentry_key=wrong", 401},
	}
	for _, tc := range cases {
		r := httptest.NewRequest("POST", tc.url, nil)
		if tc.hdr != "" {
			r.Header.Set("X-Sentry-Auth", tc.hdr)
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != tc.want {
			t.Errorf("%s %q: got %d want %d", tc.url, tc.hdr, w.Code, tc.want)
		}
	}
}

func TestRateLimit(t *testing.T) {
	l := ratelimit.New(2)
	h := RateLimit(l)(ok())
	codes := []int{}
	for i := 0; i < 3; i++ {
		r := httptest.NewRequest("GET", "/api/events", nil)
		r.Header.Set("Authorization", "Bearer abc")
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		codes = append(codes, w.Code)
		if i == 0 && w.Header().Get("X-RateLimit-Remaining") != "1" {
			t.Errorf("remaining header = %q", w.Header().Get("X-RateLimit-Remaining"))
		}
	}
	if codes[0] != 200 || codes[1] != 200 || codes[2] != 429 {
		t.Errorf("codes = %v", codes)
	}
	// a different key has its own bucket
	r := httptest.NewRequest("GET", "/api/events", nil)
	r.Header.Set("Authorization", "Bearer other")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != 200 {
		t.Error("separate bucket per key")
	}
	if RateKey(r) == "api:"+"other" {
		t.Error("key must be digested")
	}
}

func TestRateLimiterWindowReset(t *testing.T) {
	l := ratelimit.New(1)
	base := time.Date(2026, 8, 29, 12, 0, 30, 0, time.UTC)
	cur := base
	l.SetClock(func() time.Time { return cur })
	if !l.Allow("k").Allowed || l.Allow("k").Allowed {
		t.Fatal("limit 1")
	}
	cur = base.Add(31 * time.Second) // next minute window
	if !l.Allow("k").Allowed {
		t.Error("new window should reset")
	}
}

func TestCORS(t *testing.T) {
	h := CORS("https://app.example")(ok())
	r := httptest.NewRequest("OPTIONS", "/api/events", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != 204 || w.Header().Get("Access-Control-Allow-Origin") != "https://app.example" {
		t.Errorf("preflight: %d %v", w.Code, w.Header())
	}
}
