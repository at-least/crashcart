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
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/crashcartapp/crashcart/internal/alerts"
	"github.com/crashcartapp/crashcart/internal/config"
	"github.com/crashcartapp/crashcart/internal/db"
	"github.com/crashcartapp/crashcart/internal/db/sqlc"
	"github.com/crashcartapp/crashcart/internal/export"
	"github.com/crashcartapp/crashcart/internal/ingest"
	"github.com/crashcartapp/crashcart/internal/jobs"
	"github.com/crashcartapp/crashcart/internal/retention"
	"github.com/crashcartapp/crashcart/internal/seed"
	"github.com/crashcartapp/crashcart/internal/sentry"
	"github.com/crashcartapp/crashcart/internal/server"
	"github.com/crashcartapp/crashcart/internal/store"
	"github.com/crashcartapp/crashcart/internal/symbolicate"
)

// version is set by the release build (-ldflags "-X main.version=v1.2.3").
var version = "dev"

const usage = `usage: crashcart <command>

  serve            HTTP server + job worker + schedulers (default)
  init             create the schema and exit
  retention        reconcile policies and run one sweep
  alerts           run one crash-spike check
  seed [slug]      write a week of demo data (default project "demo")
  export [slug]    stream NDJSON to stdout (all projects, or one)
  import           load NDJSON from stdin (idempotent)
  project <slug> <name> [platform]   create a project and print its DSN
  rotate-key <slug>                  issue a new DSN key (old one stops within seconds)
                                     (platform: ios android flutter react-native web backend other) key
  user add <email> [name]            create a viewer account (password from CRASHCART_PASSWORD, else prompted)
  user passwd <email>                set a viewer account's password (same source)
  apikey create <name>               create an API key and print its secret (shown once)
  apikey list                        list API keys
  apikey revoke <id>                 revoke an API key
  version          print the version and exit
`

func main() {
	log := slog.New(slog.NewTextHandler(os.Stderr, nil))
	cmd := "serve"
	if len(os.Args) > 1 {
		cmd = os.Args[1]
	}
	if cmd == "help" || cmd == "-h" || cmd == "--help" {
		fmt.Print(usage)
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
	if cfg.DatabaseURL == "" {
		fatal(log, errors.New("DATABASE_URL is required"))
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

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
	notifier := &alerts.Notifier{Store: st, Cfg: cfg, Log: log, HTTP: &http.Client{Timeout: 15 * time.Second}}

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
	case "alerts":
		if err := notifier.CheckSpikes(ctx); err != nil {
			fatal(log, err)
		}
	case "seed":
		slug := "demo"
		if len(args) > 0 {
			slug = args[0]
		}
		if err := seed.Run(ctx, in, slug); err != nil {
			fatal(log, err)
		}
		if err := retention.RefreshAggregates(ctx, st); err != nil {
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
		rep, err := export.Import(ctx, st, os.Stdin)
		if err != nil {
			fatal(log, err)
		}
		if err := retention.RefreshAggregates(ctx, st); err != nil {
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
		p, err := st.CreateProject(ctx, sqlc.CreateProjectParams{Slug: fs.Arg(0), Name: fs.Arg(1), Platform: platform, PublicKey: newKey()})
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
		if p, err = st.RotateProjectKey(ctx, sqlc.RotateProjectKeyParams{ID: p.ID, PublicKey: newKey()}); err != nil {
			fatal(log, err)
		}
		fmt.Printf("project %s: new DSN %s\n", p.Slug, dsn(cfg, p))
	case "serve":
		serve(ctx, cfg, st, in, syms, notifier, log)
	default:
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
}

func serve(ctx context.Context, cfg config.Config, st *store.Store, in *ingest.Ingester, syms *symbolicate.Service, notifier *alerts.Notifier, log *slog.Logger) {
	if err := retention.Reconcile(ctx, st, cfg, log); err != nil {
		fatal(log, err)
	}
	// One LISTEN connection wakes the workers and the SSE streams (polling
	// stays as the fallback).
	listener := &store.Listener{Pool: st.Pool, Log: log}
	go listener.Run(ctx)
	handlers := map[string]jobs.Handler{
		"symbolicate": func(ctx context.Context, j sqlc.Job, args json.RawMessage) error {
			var a struct {
				Event sentry.ID `json:"event"`
			}
			if err := json.Unmarshal(args, &a); err != nil {
				return err
			}
			return syms.Event(ctx, j.ProjectID, a.Event)
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
			}
			if err := json.Unmarshal(args, &a); err != nil {
				return err
			}
			return notifier.Issue(ctx, j.ProjectID, a.Type, a.Fingerprint)
		},
	}
	for i := 0; i < max(cfg.Workers, 1); i++ {
		wake, _ := listener.Subscribe(store.ChannelJobs, "")
		go (&jobs.Worker{Store: st, Log: log, Handlers: handlers, Wake: wake}).Run(ctx)
	}
	// Scheduled work runs on one replica at a time: each tick takes a
	// Postgres advisory lock and skips when another process holds it.
	// (Compression, chunk retention and aggregate refresh are TimescaleDB
	// policies — they run inside the database.)
	go every(ctx, cfg.AlertInterval, leader(st, log, store.LeaderSpikeCheck, func() {
		if err := notifier.CheckSpikes(ctx); err != nil {
			log.Error("crash-spike check", "err", err)
		}
	}))
	go every(ctx, time.Hour, leader(st, log, store.LeaderSweep, func() {
		if err := retention.Sweep(ctx, st, cfg, log); err != nil {
			log.Error("retention sweep", "err", err)
		}
	}))

	srv := &http.Server{
		Addr:              cfg.Addr,
		Handler:           server.New(server.Deps{Store: st, Cfg: cfg, Log: log, Symbols: syms, Listener: listener}),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       60 * time.Second,
		WriteTimeout:      0, // SSE
		IdleTimeout:       120 * time.Second,
	}
	go func() {
		<-ctx.Done()
		shutdown, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		srv.Shutdown(shutdown)
	}()
	log.Info("listening", "addr", cfg.Addr, "workers", cfg.Workers)
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		fatal(log, err)
	}
}

// leader wraps a tick so it runs only on the replica that wins the lock.
func leader(st *store.Store, log *slog.Logger, key int64, fn func()) func() {
	return func() {
		if _, err := st.RunAsLeader(context.Background(), key, fn); err != nil {
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
