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

	"github.com/at-least/crashcart/internal/alerts"
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

const usage = `usage: crashcart <command>

  serve            HTTP server + job worker + schedulers (default)
  migrate          apply pending migrations and exit
  retention        reconcile policies and run one sweep
  alerts           run one crash-spike check
  seed [slug]      write a week of demo data (default project "demo")
  export [slug]    stream NDJSON to stdout (all projects, or one)
  import           load NDJSON from stdin (idempotent)
  project <slug> <name> [platform]   create a project and print its DSN
  rotate-key <slug>                  issue a new DSN key (old one stops within seconds)
                                     (platform: ios android flutter react-native web backend other) key
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
	ran, err := db.Migrate(ctx, pool)
	if err != nil {
		fatal(log, err)
	}
	for _, m := range ran {
		log.Info("migration applied", "name", m)
	}
	st := store.New(pool)
	syms := &symbolicate.Service{Store: st, DSYM: symbolicate.NewDSYMClient(cfg.SymbolicateURL)}
	in := &ingest.Ingester{Store: st, Cfg: cfg, Symbols: syms, Log: log}
	notifier := &alerts.Notifier{Store: st, Cfg: cfg, Log: log, HTTP: &http.Client{Timeout: 15 * time.Second}}

	args := os.Args[min(2, len(os.Args)):]
	switch cmd {
	case "migrate":
		return
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
	worker := &jobs.Worker{Store: st, Log: log, Handlers: map[string]jobs.Handler{
		"symbolicate": func(ctx context.Context, j sqlc.Job, args json.RawMessage) error {
			var a struct {
				Event int64 `json:"event"`
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
	}}
	for i := 0; i < max(cfg.Workers, 1); i++ {
		go worker.Run(ctx)
	}
	go every(ctx, cfg.AlertInterval, func() {
		if err := notifier.CheckSpikes(ctx); err != nil {
			log.Error("crash-spike check", "err", err)
		}
	})
	go every(ctx, time.Hour, func() {
		if err := retention.Sweep(ctx, st, cfg, log); err != nil {
			log.Error("retention sweep", "err", err)
		}
	})

	srv := &http.Server{
		Addr:              cfg.Addr,
		Handler:           server.New(server.Deps{Store: st, Cfg: cfg, Log: log, Symbols: syms}),
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
