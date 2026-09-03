package server

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/crashcartapp/crashcart/internal/auth"
	"github.com/crashcartapp/crashcart/internal/config"
	"github.com/crashcartapp/crashcart/internal/store"
	"github.com/crashcartapp/crashcart/internal/symbolicate"
	"github.com/crashcartapp/crashcart/internal/testdb"
)

// TestCORSScopes: CORS_ORIGIN reaches only the SDK ingest endpoints and
// API_CORS_ORIGIN only /api/…; neither leaks onto the other or the viewer.
func TestCORSScopes(t *testing.T) {
	st := testdb.New(t)
	ctx := context.Background()
	p, err := store.CreateProject(ctx, st.Pool, "shop", "Shop", nil, "dsnkey")
	if err != nil {
		t.Fatal(err)
	}
	_, apiKey, err := (&auth.Access{Store: st}).CreateAPIKey(ctx, "test", nil)
	if err != nil {
		t.Fatal(err)
	}
	serve := func(cfg config.Config, method, path string, hdr map[string]string) *httptest.ResponseRecorder {
		h := New(Deps{Store: st, Cfg: cfg, Log: slog.Default(), Symbols: &symbolicate.Service{Store: st, DSYM: symbolicate.NewDSYMClient("")}})
		req := httptest.NewRequest(method, path, nil)
		req.Header.Set("Origin", "https://app.example")
		for k, v := range hdr {
			req.Header.Set(k, v)
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec
	}
	ingestPath := fmt.Sprintf("/api/%d/envelope/", p.ID)
	bearer := map[string]string{"Authorization": "Bearer " + apiKey}

	// Only the SDK origin configured.
	cfg := config.Config{CORSOrigin: "https://sdk.example", RetentionDays: 30}
	if rec := serve(cfg, "OPTIONS", ingestPath, nil); rec.Code != 204 || rec.Header().Get("Access-Control-Allow-Origin") != "https://sdk.example" {
		t.Errorf("ingest preflight: %d %v", rec.Code, rec.Header())
	}
	if rec := serve(cfg, "OPTIONS", "/api/projects/shop/issues", bearer); rec.Header().Get("Access-Control-Allow-Origin") != "" {
		t.Errorf("CORS_ORIGIN leaked onto /api/projects: %v", rec.Header())
	}
	if rec := serve(cfg, "GET", "/api/projects/shop", bearer); rec.Code != 200 || rec.Header().Get("Access-Control-Allow-Origin") != "" {
		t.Errorf("CORS_ORIGIN leaked onto an API response: %d %v", rec.Code, rec.Header())
	}
	if rec := serve(cfg, "GET", "/login", nil); rec.Header().Get("Access-Control-Allow-Origin") != "" {
		t.Errorf("CORS on the viewer: %v", rec.Header())
	}

	// Only the API origin configured.
	cfg = config.Config{APICORSOrigin: "https://dash.example", RetentionDays: 30}
	if rec := serve(cfg, "OPTIONS", "/api/projects/shop/issues", nil); rec.Code != 204 || rec.Header().Get("Access-Control-Allow-Origin") != "https://dash.example" {
		t.Errorf("API preflight (no key needed): %d %v", rec.Code, rec.Header())
	}
	if rec := serve(cfg, "GET", "/api/projects/shop", bearer); rec.Header().Get("Access-Control-Allow-Origin") != "https://dash.example" {
		t.Errorf("API response without its origin: %v", rec.Header())
	}
	if rec := serve(cfg, "OPTIONS", ingestPath, nil); rec.Header().Get("Access-Control-Allow-Origin") == "https://dash.example" {
		t.Errorf("API_CORS_ORIGIN leaked onto the SDK endpoint: %v", rec.Header())
	}
	// The sentry-cli routes are /api/0/…: they belong to the API's scope.
	if rec := serve(cfg, "OPTIONS", "/api/0/organizations/o/chunk-upload/", nil); rec.Header().Get("Access-Control-Allow-Origin") != "https://dash.example" {
		t.Errorf("sentry-cli preflight: %v", rec.Header())
	}
	if rec := serve(cfg, "POST", ingestPath, map[string]string{"X-Sentry-Auth": "Sentry sentry_key=dsnkey"}); rec.Code == http.StatusNoContent {
		t.Errorf("an empty POST must not be answered like a preflight: %d", rec.Code)
	}
}
