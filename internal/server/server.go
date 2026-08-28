// Package server assembles the HTTP handler: middleware, JSON API, ingest,
// health and the viewer. main and the integration tests share it.
package server

import (
	"log/slog"
	"net/http"

	"github.com/newlix/crashcart/internal/api"
	"github.com/newlix/crashcart/internal/auth"
	"github.com/newlix/crashcart/internal/config"
	"github.com/newlix/crashcart/internal/ingest"
	"github.com/newlix/crashcart/internal/ratelimit"
	"github.com/newlix/crashcart/internal/store"
	"github.com/newlix/crashcart/internal/symbolicate"
	"github.com/newlix/crashcart/internal/web"
)

// New builds the root handler.
func New(cfg config.Config, st *store.Store, log *slog.Logger) http.Handler {
	ing := ingest.New(st.Pool(), ingest.Options{Redact: cfg.PIIRedact, SampleRate: cfg.SampleRate})
	apiH := &api.Handler{Store: st, Ingester: ing, Config: cfg, DSYM: symbolicate.NewDSYMClient(cfg.SymbolicateURL), Log: log}

	var limiter *ratelimit.Limiter
	if cfg.RateLimit > 0 {
		limiter = ratelimit.New(cfg.RateLimit)
	}
	cors := auth.CORS(cfg.CORSOrigin)
	rl := auth.RateLimit(limiter)
	apiMW := func(h http.Handler) http.Handler { return auth.Chain(h, cors, auth.APIKey(cfg.APIKeys), rl) }
	ingestMW := func(h http.Handler) http.Handler { return auth.Chain(h, cors, auth.Ingest(cfg.IngestToken), rl) }

	mux := http.NewServeMux()
	apiH.Register(mux, apiMW, ingestMW)
	mux.HandleFunc("GET /health", apiH.Health)
	web.New(st, cfg, apiH, log).Register(mux)
	return mux
}
