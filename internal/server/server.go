// Package server wires the HTTP handlers.
package server

import (
	"context"
	"github.com/crashcartapp/crashcart/internal/metrics"
	"log/slog"
	"math"
	"net/http"
	"regexp"
	"sync"

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
	// GET /metrics: Prometheus text format, behind an API key (scrapers
	// send it as a bearer token) and the API rate limit.
	registerGauges(d.Store)
	access := &auth.Access{Store: d.Store}
	mux.Handle("GET /metrics", auth.Chain(metrics.Handler(), auth.RateLimit("api", d.Cfg.RateLimit, auth.BearerCredential), access.APIKey))
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

var gaugesOnce sync.Once

// registerGauges adds the database-backed gauges (once per process): the
// job queue and the stats backlog, the same on every replica.
func registerGauges(st *store.Store) {
	gaugesOnce.Do(func() {
		count := func(sql string) func(ctx context.Context) float64 {
			return func(ctx context.Context) float64 {
				var n float64
				if err := st.Pool.QueryRow(ctx, sql).Scan(&n); err != nil {
					return math.NaN()
				}
				return n
			}
		}
		metrics.NewGauge("crashcart_jobs_pending", "Jobs waiting to run (not leased, attempts left).", count("SELECT count(*) FROM jobs WHERE locked_until IS NULL AND attempts < 8"))
		metrics.NewGauge("crashcart_jobs_dead", "Jobs that failed every attempt and are kept for inspection.", count("SELECT count(*) FROM jobs WHERE attempts >= 8"))
		metrics.NewGauge("crashcart_stats_dirty_hours", "Hours awaiting the statistics rollup (events + sessions).", count("SELECT (SELECT count(*) FROM event_stats_dirty) + (SELECT count(*) FROM session_stats_dirty)"))
		metrics.NewGauge("crashcart_issues", "Issue rows in the database.", count("SELECT count(*) FROM issues"))
	})
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
