// Command crashcart is the CrashCart server: Sentry-compatible ingest,
// JSON API, server-rendered viewer, and the retention + alert schedulers —
// one binary in front of one Postgres.
//
//	crashcart serve       run the HTTP server (+ schedulers)   [default]
//	crashcart migrate     apply pending schema migrations and exit
//	crashcart retention   run one retention pass and exit
//	crashcart alerts      run one alert check and exit
//	crashcart seed        write a week of synthetic events (local dev)
//	crashcart export      stream every table as NDJSON to stdout (backup / migration)
//	crashcart import      load that NDJSON from stdin (idempotent; the D1 → Go upgrade path)
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/newlix/crashcart/internal/alerts"
	"github.com/newlix/crashcart/internal/config"
	"github.com/newlix/crashcart/internal/db"
	"github.com/newlix/crashcart/internal/export"
	"github.com/newlix/crashcart/internal/retention"
	"github.com/newlix/crashcart/internal/server"
	"github.com/newlix/crashcart/internal/store"
)

func main() {
	log := slog.New(slog.NewTextHandler(os.Stderr, nil))
	cmd := "serve"
	if len(os.Args) > 1 {
		cmd = os.Args[1]
	}
	if err := run(cmd, log); err != nil {
		log.Error("fatal", "err", err)
		os.Exit(1)
	}
}

func run(cmd string, log *slog.Logger) error {
	cfg, err := config.FromEnv()
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	pool, err := pgxpool.New(ctx, cfg.DatabaseURL)
	if err != nil {
		return fmt.Errorf("connect: %w", err)
	}
	defer pool.Close()

	applied, err := db.Migrate(ctx, pool)
	if err != nil {
		return fmt.Errorf("migrate: %w", err)
	}
	if len(applied) > 0 {
		log.Info("migrations applied", "versions", applied)
	}
	if created, err := db.EnsureUpcomingPartitions(ctx, pool, time.Now()); err != nil {
		return fmt.Errorf("partitions: %w", err)
	} else if len(created) > 0 {
		log.Info("event partitions created", "partitions", created)
	}
	st := store.New(pool)

	switch cmd {
	case "migrate":
		return nil
	case "retention":
		_, err := (&retention.Runner{Store: st, Days: cfg.RetentionDays, Log: log, Now: time.Now}).Run(ctx)
		return err
	case "alerts":
		return alerts.New(st, cfg, log).Run(ctx)
	case "seed":
		return seed(ctx, cfg, st, log)
	case "export":
		return export.All(ctx, pool, os.Stdout)
	case "import":
		rep, err := export.Load(ctx, pool, os.Stdin)
		log.Info("import complete", "rows", rep.Rows, "event_conflicts", rep.Conflict, "skipped_lines", rep.Skipped)
		return err
	case "serve":
		return serve(ctx, cfg, st, log)
	default:
		return fmt.Errorf("unknown command %q (serve | migrate | retention | alerts | seed | export | import)", cmd)
	}
}

func serve(ctx context.Context, cfg config.Config, st *store.Store, log *slog.Logger) error {
	mux := server.New(cfg, st, log)

	srv := &http.Server{
		Addr:              cfg.Addr,
		Handler:           logRequests(log, mux),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       60 * time.Second,
		WriteTimeout:      120 * time.Second,
		IdleTimeout:       120 * time.Second,
	}

	// Schedulers: retention (hourly) and alerts (every 10 min), in-process.
	go schedule(ctx, log, "retention", cfg.RetentionInterval, func(c context.Context) error {
		_, err := (&retention.Runner{Store: st, Days: cfg.RetentionDays, Log: log, Now: time.Now}).Run(c)
		return err
	})
	go schedule(ctx, log, "alerts", cfg.AlertInterval, alerts.New(st, cfg, log).Run)

	errCh := make(chan error, 1)
	go func() {
		log.Info("listening", "addr", cfg.Addr)
		errCh <- srv.ListenAndServe()
	}()
	select {
	case err := <-errCh:
		if !errors.Is(err, http.ErrServerClosed) {
			return err
		}
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		log.Info("shutting down")
		return srv.Shutdown(shutdownCtx)
	}
	return nil
}

// schedule runs fn every interval until ctx ends (first run after one interval).
func schedule(ctx context.Context, log *slog.Logger, name string, interval time.Duration, fn func(context.Context) error) {
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			runCtx, cancel := context.WithTimeout(ctx, interval)
			if err := fn(runCtx); err != nil {
				log.Error("scheduled job failed", "job", name, "err", err)
			}
			cancel()
		}
	}
}

func logRequests(log *slog.Logger, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(rec, r)
		if rec.status >= 500 {
			log.Error("request", "method", r.Method, "path", r.URL.Path, "status", rec.status, "ms", time.Since(start).Milliseconds())
		} else {
			log.Debug("request", "method", r.Method, "path", r.URL.Path, "status", rec.status, "ms", time.Since(start).Milliseconds())
		}
	})
}

type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (s *statusRecorder) WriteHeader(code int) {
	s.status = code
	s.ResponseWriter.WriteHeader(code)
}

// Unwrap lets http.ResponseController reach the underlying writer.
func (s *statusRecorder) Unwrap() http.ResponseWriter { return s.ResponseWriter }
