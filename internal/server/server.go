// Package server wires the HTTP handlers.
package server

import (
	"context"
	"log/slog"
	"math"
	"net/http"
	"regexp"
	"sync"
	"time"

	"github.com/crashcartapp/crashcart/internal/metrics"

	"github.com/crashcartapp/crashcart/internal/api"
	"github.com/crashcartapp/crashcart/internal/auth"
	"github.com/crashcartapp/crashcart/internal/config"
	"github.com/crashcartapp/crashcart/internal/db/sqlc"
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
	Stopping <-chan struct{} // optional: closed at shutdown, ends the SSE streams
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
	(&web.Web{Store: d.Store, Cfg: d.Cfg, Log: d.Log, Symbols: d.Symbols, Listener: d.Listener, Stopping: d.Stopping}).Register(mux)
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

// dbGauges are the database-backed gauges of GET /metrics: one query per
// scrape, cached for a second so several scrapers do not multiply it.
type dbGauges struct {
	st *store.Store
	mu sync.Mutex
	at time.Time
	v  sqlc.MetricsGaugesRow
	ok bool
}

func (g *dbGauges) read(ctx context.Context) (sqlc.MetricsGaugesRow, bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if time.Since(g.at) < time.Second {
		return g.v, g.ok
	}
	v, err := g.st.MetricsGauges(ctx)
	g.at, g.v, g.ok = time.Now(), v, err == nil
	return g.v, g.ok
}

func (g *dbGauges) gauge(name, help string, pick func(sqlc.MetricsGaugesRow) int64) {
	metrics.NewGauge(name, help, func(ctx context.Context) float64 {
		v, ok := g.read(ctx)
		if !ok {
			return math.NaN()
		}
		return float64(pick(v))
	})
}

func registerGauges(st *store.Store) {
	g := &dbGauges{st: st}
	g.gauge("crashcart_jobs_pending", "Jobs waiting to run (not leased, attempts left).", func(r sqlc.MetricsGaugesRow) int64 { return r.JobsPending })
	g.gauge("crashcart_jobs_dead", "Jobs that failed every attempt and are kept for inspection.", func(r sqlc.MetricsGaugesRow) int64 { return r.JobsDead })
	g.gauge("crashcart_stats_dirty_hours", "Hours awaiting the statistics rollup (events + sessions).", func(r sqlc.MetricsGaugesRow) int64 { return int64(r.DirtyHours) })
	g.gauge("crashcart_issues", "Issue rows in the database.", func(r sqlc.MetricsGaugesRow) int64 { return r.Issues })
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
