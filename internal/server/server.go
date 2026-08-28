// Package server wires the HTTP handlers.
package server

import (
	"log/slog"
	"net/http"

	"github.com/newlix/crashcart/internal/api"
	"github.com/newlix/crashcart/internal/config"
	"github.com/newlix/crashcart/internal/ingest"
	"github.com/newlix/crashcart/internal/store"
	"github.com/newlix/crashcart/internal/symbolicate"
	"github.com/newlix/crashcart/internal/web"
)

// Deps is everything the HTTP layer needs.
type Deps struct {
	Store   *store.Store
	Cfg     config.Config
	Log     *slog.Logger
	Symbols *symbolicate.Service
}

// New builds the root handler.
func New(d Deps) http.Handler {
	mux := http.NewServeMux()
	in := &ingest.Ingester{Store: d.Store, Cfg: d.Cfg, Symbols: d.Symbols, Log: d.Log}
	mux.Handle("POST /api/{project}/envelope/{$}", in.Handler())
	mux.Handle("POST /api/{project}/envelope", in.Handler())
	mux.Handle("POST /api/{project}/store/{$}", in.Handler())
	mux.Handle("POST /api/{project}/store", in.Handler())
	(&api.Handler{Store: d.Store, Cfg: d.Cfg, Symbols: d.Symbols, Log: d.Log}).Register(mux)
	(&web.Web{Store: d.Store, Cfg: d.Cfg, Log: d.Log, Symbols: d.Symbols}).Register(mux)
	mux.HandleFunc("GET /health", func(w http.ResponseWriter, r *http.Request) {
		if err := d.Store.Pool.Ping(r.Context()); err != nil {
			http.Error(w, `{"status":"db unavailable"}`, http.StatusServiceUnavailable)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"status":"ok"}`))
	})
	return mux
}
