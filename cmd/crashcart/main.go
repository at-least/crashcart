// Command crashcart is the CrashCart server and its maintenance commands.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"
	_ "time/tzdata" // the release image (distroless/static) carries no system tzdata; monitors' IANA timezones need it embedded

	"github.com/at-least/crashcart/internal/alerts"
	"github.com/at-least/crashcart/internal/auth"
	"github.com/at-least/crashcart/internal/cli"
	"github.com/at-least/crashcart/internal/config"
	"github.com/at-least/crashcart/internal/db"
	"github.com/at-least/crashcart/internal/db/sqlc"
	"github.com/at-least/crashcart/internal/export"
	"github.com/at-least/crashcart/internal/ingest"
	"github.com/at-least/crashcart/internal/jobs"
	"github.com/at-least/crashcart/internal/retention"
	"github.com/at-least/crashcart/internal/seed"
	"github.com/at-least/crashcart/internal/sentry"
	"github.com/at-least/crashcart/internal/server"
	"github.com/at-least/crashcart/internal/store"
	"github.com/at-least/crashcart/internal/symbolicate"
)

// version is set by the release build (-ldflags "-X main.version=v1.2.3").
var version = "dev"

func main() {
	log := slog.New(slog.NewTextHandler(os.Stderr, nil))
	cmd := "serve"
	if len(os.Args) > 1 {
		cmd = os.Args[1]
	}
	if cmd == "help" || cmd == "-h" || cmd == "--help" {
		fmt.Print(cli.Usage())
		return
	}
	if cmd == "version" || cmd == "-v" || cmd == "--version" {
		fmt.Println("crashcart", version)
		return
	}
	cfg, err := config.Load()
	if err != nil {
		fatal(log, err)
	}
	if cmd == "symbolicate" {
		if err := symbolicateSidecar(cfg, log); err != nil {
			fatal(log, err)
		}
		return
	}
	if cfg.DatabaseURL == "" {
		fatal(log, errors.New("DATABASE_URL is required"))
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	go func() { <-ctx.Done(); stop() }() // a second signal during shutdown kills the process the default way

	pool, err := db.Connect(ctx, cfg.DatabaseURL)
	if err != nil {
		fatal(log, err)
	}
	defer pool.Close()
	created, err := db.Init(ctx, pool)
	if err != nil {
		fatal(log, err)
	}
	if created {
		log.Info("schema created")
	}
	st := store.New(pool)
	syms := &symbolicate.Service{Store: st, DSYM: symbolicate.NewDSYMClient(cfg.SymbolicateURL)}
	in := &ingest.Ingester{Store: st, Cfg: cfg, Symbols: syms, Log: log}
	notifier := &alerts.Notifier{Store: st, Cfg: cfg, Log: log} // HTTP left nil: the hardened client (post-DNS address check, no redirects)

	args := os.Args[min(2, len(os.Args)):]
	switch cmd {
	case "init":
		return // the schema was created above
	case "retention":
		if err := retention.Reconcile(ctx, st, cfg, log); err != nil {
			fatal(log, err)
		}
		if err := retention.Sweep(ctx, st, cfg, log); err != nil {
			fatal(log, err)
		}
		if err := retention.RollupAll(ctx, st, cfg); err != nil {
			fatal(log, err)
		}
	case "alerts":
		if err := notifier.CheckSpikes(ctx); err != nil {
			fatal(log, err)
		}
	case "seed":
		slug := "demo"
		if len(args) > 0 {
			slug = args[0]
		}
		if err := retention.Reconcile(ctx, st, cfg, log); err != nil { // partitions
			fatal(log, err)
		}
		if err := seed.Run(ctx, in, slug); err != nil {
			fatal(log, err)
		}
		if err := retention.RollupAll(ctx, st, cfg); err != nil {
			fatal(log, err)
		}
	case "export":
		var opt export.Options
		if len(args) > 0 {
			opt.Project = args[0]
		}
		if err := export.Export(ctx, st, os.Stdout, opt); err != nil {
			fatal(log, err)
		}
	case "import":
		if err := retention.Reconcile(ctx, st, cfg, log); err != nil { // partitions
			fatal(log, err)
		}
		rep, err := export.Import(ctx, st, os.Stdin)
		if err != nil {
			fatal(log, err)
		}
		if err := retention.RollupAll(ctx, st, cfg); err != nil {
			fatal(log, err)
		}
		json.NewEncoder(os.Stderr).Encode(rep)
	case "project":
		fs := flag.NewFlagSet("project", flag.ExitOnError)
		fs.Parse(args)
		if fs.NArg() < 2 {
			fatal(log, errors.New("usage: crashcart project <slug> <name> [platform]"))
		}
		var platform *string
		if fs.NArg() > 2 {
			p := fs.Arg(2)
			if !sentry.IsFamily(p) {
				fatal(log, fmt.Errorf("platform must be one of %s", strings.Join(sentry.Families, ", ")))
			}
			platform = &p
		}
		p, err := st.CreateProject(ctx, sqlc.CreateProjectParams{Slug: fs.Arg(0), Name: fs.Arg(1), Platform: platform, PublicKey: auth.NewProjectKey()})
		if err != nil {
			fatal(log, err)
		}
		fmt.Printf("project %s (id %d)\nDSN: %s\n", p.Slug, p.ID, dsn(cfg, p))
	case "user":
		if err := userCmd(ctx, st, args); err != nil {
			fatal(log, err)
		}
	case "apikey":
		if err := apikeyCmd(ctx, st, args); err != nil {
			fatal(log, err)
		}
	case "rotate-key":
		if len(args) < 1 {
			fatal(log, errors.New("usage: crashcart rotate-key <slug>"))
		}
		p, err := st.GetProject(ctx, args[0])
		if err != nil {
			fatal(log, err)
		}
		if p, err = st.RotateProjectKey(ctx, p.ID, auth.NewProjectKey()); err != nil {
			fatal(log, err)
		}
		fmt.Printf("project %s: new DSN %s (old DSN keeps working until removed - see `project-keys`)\n", p.Slug, dsn(cfg, p))
	case "project-keys":
		if err := projectKeysCmd(ctx, st, cfg, args); err != nil {
			fatal(log, err)
		}
	case "serve":
		serve(ctx, cfg, st, in, syms, notifier, log)
	default:
		fmt.Fprint(os.Stderr, cli.Usage())
		os.Exit(2)
	}
}

func serve(ctx context.Context, cfg config.Config, st *store.Store, in *ingest.Ingester, syms *symbolicate.Service, notifier *alerts.Notifier, log *slog.Logger) {
	if err := retention.Reconcile(ctx, st, cfg, log); err != nil {
		fatal(log, err)
	}
	// Shutdown order: stop accepting, drain the HTTP handlers (an ingest
	// write may take up to ingest.WriteTimeout), then stop the workers and
	// schedulers (workCtx) and wait for the job in hand to record its
	// outcome. Envelopes accepted while the workers were already gone
	// would leave jobs to the other replicas or the next start.
	workCtx, stopWork := context.WithCancel(context.Background())
	defer stopWork()
	// One LISTEN connection wakes the workers and the SSE streams (polling
	// stays as the fallback).
	listener := &store.Listener{Pool: st.Pool, Log: log}
	go listener.Run(workCtx)
	handlers := map[string]jobs.Handler{
		"symbolicate": func(ctx context.Context, j sqlc.Job, args json.RawMessage) error {
			var a struct {
				Event sentry.ID `json:"event"`
				At    time.Time `json:"at"`
			}
			if err := json.Unmarshal(args, &a); err != nil {
				return err
			}
			return syms.Event(ctx, j.ProjectID, a.Event, a.At)
		},
		"resymbolicate": func(ctx context.Context, j sqlc.Job, args json.RawMessage) error {
			var a struct {
				Release string `json:"release"`
			}
			if err := json.Unmarshal(args, &a); err != nil {
				return err
			}
			return syms.Release(ctx, j.ProjectID, a.Release)
		},
		"alert": func(ctx context.Context, j sqlc.Job, args json.RawMessage) error {
			var a struct {
				Type        string `json:"type"`
				Fingerprint string `json:"fingerprint"`
				Monitor     string `json:"monitor"`
			}
			if err := json.Unmarshal(args, &a); err != nil {
				return err
			}
			if a.Monitor != "" {
				return notifier.Monitor(ctx, j.ProjectID, a.Type, a.Monitor)
			}
			return notifier.Issue(ctx, j.ProjectID, a.Type, a.Fingerprint)
		},
	}
	var work sync.WaitGroup
	for i := 0; i < max(cfg.Workers, 1); i++ {
		wake, _ := listener.Subscribe(store.ChannelJobs, "")
		work.Add(1)
		go func() {
			defer work.Done()
			(&jobs.Worker{Store: st, Log: log, Handlers: handlers, Wake: wake}).Run(workCtx)
		}()
	}
	// Scheduled work runs on one replica at a time: each tick takes a
	// Postgres advisory lock and skips when another process holds it.
	tick := func(d time.Duration, key int64, name string, fn func(context.Context) error) {
		work.Add(1)
		go func() {
			defer work.Done()
			every(workCtx, d, leader(workCtx, st, log, key, func() {
				if err := fn(workCtx); err != nil && workCtx.Err() == nil {
					log.Error(name, "err", err)
				}
			}))
		}()
	}
	tick(time.Minute, store.LeaderRollup, "stats rollup", func(ctx context.Context) error { _, err := retention.Rollup(ctx, st, cfg); return err })
	tick(cfg.AlertInterval, store.LeaderSpikeCheck, "unhandled-spike check", notifier.CheckSpikes)
	tick(time.Minute, store.LeaderIgnoreCheck, "ignored-issue check", notifier.CheckIgnored)
	tick(time.Minute, store.LeaderMonitorCheck, "monitor check", notifier.CheckMonitors)
	tick(time.Hour, store.LeaderSweep, "retention sweep", func(ctx context.Context) error { return retention.Sweep(ctx, st, cfg, log) })

	// At shutdown the SSE streams are told to end (they would otherwise
	// hold Shutdown until its deadline) while every other in-flight
	// request keeps its context and finishes: Shutdown drains them, and
	// ingest writes run on a detached context and finish regardless.
	stopping := make(chan struct{})
	srv := &http.Server{
		Addr:              cfg.Addr,
		Handler:           server.New(server.Deps{Store: st, Cfg: cfg, Log: log, Symbols: syms, Listener: listener, Stopping: stopping}),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       60 * time.Second,
		WriteTimeout:      0, // SSE
		IdleTimeout:       120 * time.Second,
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		<-ctx.Done()
		log.Info("shutting down")
		close(stopping)
		shutdown, cancel := context.WithTimeout(context.Background(), ingest.WriteTimeout+5*time.Second)
		defer cancel()
		if err := srv.Shutdown(shutdown); err != nil {
			log.Warn("shutdown: http", "err", err)
		}
	}()
	log.Info("listening", "addr", cfg.Addr, "workers", cfg.Workers)
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		fatal(log, err)
	}
	<-done
	stopWork()
	drained := make(chan struct{})
	go func() { work.Wait(); close(drained) }()
	select {
	case <-drained:
	case <-time.After(30 * time.Second):
		log.Warn("shutdown: workers still busy, exiting")
	}
}

// leader wraps a tick so it runs only on the replica that wins the lock.
func leader(ctx context.Context, st *store.Store, log *slog.Logger, key int64, fn func()) func() {
	return func() {
		if _, err := st.RunAsLeader(ctx, key, fn); err != nil && ctx.Err() == nil {
			log.Error("scheduler: leader lock", "err", err)
		}
	}
}

func every(ctx context.Context, d time.Duration, fn func()) {
	t := time.NewTicker(d)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			fn()
		}
	}
}

func fatal(log *slog.Logger, err error) {
	log.Error("fatal", "err", err)
	os.Exit(1)
}

// symbolicateSidecar runs the dSYM sidecar: an HTTP server around
// llvm-symbolizer with a disk cache (internal/symbolicate.Sidecar). It has
// no database; the main process reaches it through SYMBOLICATE_URL.
func symbolicateSidecar(cfg config.Config, log *slog.Logger) error {
	dir := cfg.SymbolicateCacheDir
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	sc := &symbolicate.Sidecar{Dir: dir, MaxBytes: int64(cfg.SymbolicateCacheMB) << 20, Log: log}
	if _, err := exec.LookPath("llvm-symbolizer"); err != nil {
		log.Warn("symbolicate: llvm-symbolizer not on PATH; /health reports it and requests will fail")
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	srv := &http.Server{Addr: cfg.Addr, Handler: sc.Handler(), ReadHeaderTimeout: 10 * time.Second}
	done := make(chan struct{})
	go func() {
		defer close(done)
		<-ctx.Done()
		shutdown, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		srv.Shutdown(shutdown)
	}()
	log.Info("symbolicate sidecar listening", "addr", cfg.Addr, "cache_dir", dir, "version", version)
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	<-done
	return nil
}
