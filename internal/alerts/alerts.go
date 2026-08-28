// Package alerts runs the three built-in detectors and fans notifications
// out to every configured channel.
//
//	crash_spike  — crashes in the last 10 min > 3× the weekly baseline
//	new_error    — an error/fatal fingerprint first seen since the last run
//	regression   — a resolved issue that reappeared in a different release
package alerts

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/newlix/crashcart/internal/config"
	"github.com/newlix/crashcart/internal/db/sqlc"
	"github.com/newlix/crashcart/internal/pk"
	"github.com/newlix/crashcart/internal/store"
)

const (
	cooldown      = 20 * time.Minute
	defaultWindow = 10 * time.Minute
	maxWindow     = 24 * time.Hour
)

// Checker evaluates detectors.
type Checker struct {
	Store  *store.Store
	Notify Notifier
	Log    *slog.Logger
	Now    func() time.Time
}

// Notifier delivers an alert message.
type Notifier interface {
	Send(ctx context.Context, message string)
}

// New builds a Checker with the default multi-channel notifier.
func New(st *store.Store, cfg config.Config, log *slog.Logger) *Checker {
	return &Checker{Store: st, Notify: NewMultiChannel(cfg, log), Log: log, Now: time.Now}
}

// Run evaluates every enabled detector once. Called by the scheduler.
func (c *Checker) Run(ctx context.Context) error {
	types, err := c.Store.ListAlertTypes(ctx)
	if err != nil {
		return err
	}
	now := c.Now().UTC()
	q := c.Store.Queries()
	for _, t := range types {
		if !t.Enabled {
			continue
		}
		if t.CooldownUntil != nil && now.Before(*t.CooldownUntil) {
			continue
		}
		since := WindowStart(now, t.LastTriggered)
		msg, err := c.detect(ctx, t.Type, now, since)
		if err != nil {
			c.Log.Error("alert detection failed", "type", t.Type, "err", err)
			continue
		}
		if msg == "" {
			continue
		}
		c.Notify.Send(ctx, msg)
		until := now.Add(cooldown)
		if err := q.MarkAlertTriggered(ctx, sqlc.MarkAlertTriggeredParams{Type: t.Type, LastTriggered: &now, CooldownUntil: &until}); err != nil {
			c.Log.Error("alert bookkeeping failed", "type", t.Type, "err", err)
		}
		c.Log.Info("alert fired", "type", t.Type, "message", msg)
	}
	return nil
}

// WindowStart is the lower bound of "what happened since we last alerted":
// never later than one interval ago (so cooldown-skipped runs are not lost),
// never earlier than a day ago (so an idle detector doesn't replay history).
func WindowStart(now time.Time, lastTriggered *time.Time) time.Time {
	floor := now.Add(-defaultWindow)
	if lastTriggered == nil {
		return floor
	}
	start := *lastTriggered
	if oldest := now.Add(-maxWindow); start.Before(oldest) {
		start = oldest
	}
	if start.Before(floor) {
		return start
	}
	return floor
}

func (c *Checker) detect(ctx context.Context, typ string, now, since time.Time) (string, error) {
	switch typ {
	case "crash_spike":
		return c.crashSpike(ctx, now)
	case "new_error":
		return c.newError(ctx, since)
	case "regression":
		return c.regression(ctx, since)
	}
	return "", nil
}

func (c *Checker) crashSpike(ctx context.Context, now time.Time) (string, error) {
	q := c.Store.Queries()
	recent, err := q.CountCrashesSince(ctx, pk.Lower(now.Add(-10*time.Minute)))
	if err != nil || recent == 0 {
		return "", err
	}
	// Weekly baseline: daily average, excluding the last two days.
	avgDaily, err := q.CrashBaselineDailyAvg(ctx, sqlc.CrashBaselineDailyAvgParams{
		Hour:   now.Add(-7 * 24 * time.Hour).Truncate(time.Hour),
		Hour_2: now.Add(-2 * 24 * time.Hour).Truncate(time.Hour),
	})
	if err != nil {
		return "", err
	}
	per10m := avgDaily / 144
	if per10m > 0 && float64(recent) > per10m*3 {
		return fmt.Sprintf("🚨 Crash Spike: %d crashes in last 10 min vs baseline ~%.1f/10min (3× threshold exceeded)", recent, per10m), nil
	}
	return "", nil
}

func (c *Checker) newError(ctx context.Context, since time.Time) (string, error) {
	rows, err := c.Store.Queries().NewIssuesSince(ctx, since)
	if err != nil || len(rows) == 0 {
		return "", err
	}
	var b strings.Builder
	b.WriteString("🆕 New Error Type")
	if len(rows) > 1 {
		fmt.Fprintf(&b, "s (%d)", len(rows))
	}
	b.WriteString(":")
	for _, r := range rows {
		fmt.Fprintf(&b, "\n  • %s — %s", deref(r.ErrorType, "Unknown"), r.Title)
	}
	return b.String(), nil
}

func (c *Checker) regression(ctx context.Context, since time.Time) (string, error) {
	rows, err := c.Store.Queries().RegressionsSince(ctx, since)
	if err != nil || len(rows) == 0 {
		return "", err
	}
	var b strings.Builder
	b.WriteString("🔁 Regression — resolved issue reappeared:")
	for _, r := range rows {
		fmt.Fprintf(&b, "\n  • %s — %s", deref(r.ErrorType, "Unknown"), r.Title)
		if r.LastRelease != nil {
			fmt.Fprintf(&b, " (in %s)", *r.LastRelease)
		}
	}
	return b.String(), nil
}

func deref(s *string, def string) string {
	if s == nil || *s == "" {
		return def
	}
	return *s
}
