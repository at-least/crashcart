// Package server wires the HTTP handlers.
package server

import (
	"log/slog"
	"net/http"
	"regexp"

	"github.com/crashcartapp/crashcart/internal/api"
	"github.com/crashcartapp/crashcart/internal/auth"
	"github.com/crashcartapp/crashcart/internal/config"
	"github.com/crashcartapp/crashcart/internal/ingest"
	"github.com/crashcartapp/crashcart/internal/store"
	"github.com/crashcartapp/crashcart/internal/symbolicate"
	"github.com/crashcartapp/crashcart/internal/web"
)

// Deps is everything the HTTP layer needs.
type Deps struct {
	Store    *store.Store
	Cfg      config.Config
	Log      *slog.Logger
	Symbols  *symbolicate.Service
	Listener *store.Listener // optional: wakes the viewer's SSE stream on issue notifications
}

// New builds the root handler.
func New(d Deps) http.Handler {
	auth.TrustProxy = d.Cfg.TrustProxy
	mux := http.NewServeMux()
	in := &ingest.Ingester{Store: d.Store, Cfg: d.Cfg, Symbols: d.Symbols, Log: d.Log}
	mux.Handle("POST /api/{project}/envelope/{$}", in.Handler())
	mux.Handle("POST /api/{project}/envelope", in.Handler())
	mux.Handle("POST /api/{project}/store/{$}", in.Handler())
	mux.Handle("POST /api/{project}/store", in.Handler())

	(&api.Handler{Store: d.Store, Cfg: d.Cfg, Symbols: d.Symbols, Log: d.Log}).Register(mux)
	(&web.Web{Store: d.Store, Cfg: d.Cfg, Log: d.Log, Symbols: d.Symbols, Listener: d.Listener}).Register(mux)
	mux.HandleFunc("GET /health", func(w http.ResponseWriter, r *http.Request) {
		if err := d.Store.Pool.Ping(r.Context()); err != nil {
			http.Error(w, `{"status":"db unavailable"}`, http.StatusServiceUnavailable)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"status":"ok"}`))
	})
	return preflight(in.Handler(), mux)
}

// sentryEndpoint matches the SDK endpoints (/api/<id>/envelope/, /store/).
var sentryEndpoint = regexp.MustCompile(`^/api/[^/]+/(envelope|store)/?$`)

// preflight routes CORS preflights for the SDK endpoints to the ingest
// handler (whose CORS middleware answers them). They cannot be mux
// patterns: "OPTIONS /api/{project}/envelope/" would conflict with the
// API's "OPTIONS /api/projects/".
func preflight(ingest http.Handler, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodOptions && sentryEndpoint.MatchString(r.URL.Path) {
			ingest.ServeHTTP(w, r)
			return
		}
		next.ServeHTTP(w, r)
	})
}
