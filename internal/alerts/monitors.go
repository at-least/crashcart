package alerts

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"net/url"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/crashcartapp/crashcart/internal/monitors"
	"github.com/crashcartapp/crashcart/internal/sentry"
	"github.com/crashcartapp/crashcart/internal/store"
)

// CheckMonitors (scheduler, every minute like CheckIgnored) notices
// monitors that missed their expected check-in and runs that outlived
// their max_runtime_min, and fires the one-shot monitor_failed /
// monitor_recovered alert on a threshold crossing — the same job
// CheckIgnored does for issues, using a plain counter (monitors.Record)
// instead of a stats-rollup baseline.
func (n *Notifier) CheckMonitors(ctx context.Context) error {
	now := time.Now().UTC()
	due, err := store.DueMonitors(ctx, n.Store.Pool, now)
	if err != nil {
		return fmt.Errorf("due monitors: %w", err)
	}
	var errs []error
	for _, m := range due {
		if err := n.monitorMissed(ctx, m, now); err != nil {
			errs = append(errs, fmt.Errorf("monitor %s (project %d): %w", m.Slug, m.ProjectID, err))
		}
	}
	timedOut, err := store.TimedOutCheckIns(ctx, n.Store.Pool, now)
	if err != nil {
		errs = append(errs, fmt.Errorf("timed out check-ins: %w", err))
		return errors.Join(errs...)
	}
	for _, c := range timedOut {
		if err := n.monitorTimeout(ctx, c, now); err != nil {
			errs = append(errs, fmt.Errorf("check-in %s (project %d): %w", c.CheckInID, c.ProjectID, err))
		}
	}
	return errors.Join(errs...)
}

// monitorMissed records a synthetic `missed` check-in for one overdue
// monitor (so its history shows the gap, not just silence) and advances
// its state — a miss counts as a failure, like a terminal `error` does.
// next_expected_at is advanced past now, bounded, so a monitor dead for a
// long time does not spin this loop and is not re-flagged every tick.
func (n *Notifier) monitorMissed(ctx context.Context, m store.Monitor, now time.Time) error {
	sched, err := monitors.ParseSchedule(m.ScheduleType, m.ScheduleValue, strOrEmpty(m.ScheduleUnit))
	if err != nil {
		return fmt.Errorf("stored schedule no longer parses: %w", err)
	}
	loc, err := time.LoadLocation(m.Timezone)
	if err != nil {
		loc = time.UTC
	}
	next := *m.NextExpectedAt
	for i := 0; i < 10000 && !next.After(now); i++ {
		next = sched.Next(next.In(loc)).UTC().Add(time.Duration(m.CheckinMarginMin) * time.Minute)
	}
	id, err := randomCheckInID()
	if err != nil {
		return err
	}
	if err := store.InsertCheckIn(ctx, n.Store.Pool, store.InsertCheckInParams{
		StartedAt: now, ProjectID: m.ProjectID, MonitorSlug: m.Slug, CheckInID: id, Status: "missed",
	}); err != nil {
		return fmt.Errorf("insert missed check-in: %w", err)
	}
	return n.recordAndAlert(ctx, m, "missed", next, now)
}

// monitorTimeout flips one in_progress check-in past its monitor's
// max_runtime_min to `timeout` and counts it as a failure. It does not
// move next_expected_at: only a terminal check-in or a miss does — the
// monitor's own next scheduled slot is unaffected by this run overstaying.
func (n *Notifier) monitorTimeout(ctx context.Context, c store.TimedOutCheckInsRow, now time.Time) error {
	if err := store.UpdateCheckIn(ctx, n.Store.Pool, store.UpdateCheckInParams{
		ProjectID: c.ProjectID, MonitorSlug: c.MonitorSlug, CheckInID: c.CheckInID, StartedAt: c.StartedAt, Status: "timeout",
	}); err != nil {
		return fmt.Errorf("mark timeout: %w", err)
	}
	m, err := store.GetMonitor(ctx, n.Store.Pool, c.ProjectID, c.MonitorSlug)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil // deleted meanwhile
	}
	if err != nil {
		return err
	}
	next := now
	if m.NextExpectedAt != nil {
		next = *m.NextExpectedAt
	}
	return n.recordAndAlert(ctx, m, "timeout", next, now)
}

// recordAndAlert bumps the monitor's consecutive counters and, on a
// threshold crossing, delivers directly through Monitor — this runs on
// the scheduler's own goroutine (like CheckIgnored's escalate/CheckSpikes'
// spike), not inside a client-facing ingest transaction, so unlike
// ingest (which only enqueues a job for the worker to deliver) there is
// no reason to defer it.
func (n *Notifier) recordAndAlert(ctx context.Context, m store.Monitor, status string, next, now time.Time) error {
	tr := monitors.Record(m.ConsecutiveFailures, m.ConsecutiveSuccesses, m.Alerting, m.FailureThreshold, m.RecoveryThreshold, status == "ok")
	if err := store.RecordMonitorResult(ctx, n.Store.Pool, store.RecordMonitorResultParams{
		ProjectID: m.ProjectID, Slug: m.Slug, LastStatus: store.CheckinStatus(status),
		ConsecutiveFailures: tr.ConsecutiveFailures, ConsecutiveSuccesses: tr.ConsecutiveSuccesses, Alerting: tr.Alerting,
		NextExpectedAt: next, LastCheckinAt: now,
	}); err != nil {
		return fmt.Errorf("record monitor result: %w", err)
	}
	switch {
	case tr.Failed:
		return n.Monitor(ctx, m.ProjectID, TypeMonitorFailed, m.Slug)
	case tr.Recovered:
		return n.Monitor(ctx, m.ProjectID, TypeMonitorRecovered, m.Slug)
	}
	return nil
}

// Monitor handles the monitor_failed / monitor_recovered alert: the job
// kind "alert" dispatches here (ingest enqueues one when a check-in's own
// state update crosses a threshold — an outbound webhook must not block
// an envelope write, the same reason Issue is job-driven) and
// recordAndAlert calls it directly (already off the request path). By
// the time this runs the monitor's counters are already current — it
// only builds and delivers the payload.
func (n *Notifier) Monitor(ctx context.Context, projectID int64, typ, slug string) error {
	if typ != TypeMonitorFailed && typ != TypeMonitorRecovered {
		return fmt.Errorf("alert: unknown type %q", typ)
	}
	m, err := store.GetMonitor(ctx, n.Store.Pool, projectID, slug)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil // deleted meanwhile
	}
	if err != nil {
		return err
	}
	if err := EnsureRules(ctx, n.Store, projectID); err != nil {
		return err
	}
	p, err := store.GetProjectByID(ctx, n.Store.Pool, projectID)
	if err != nil {
		return err
	}
	title := fmt.Sprintf("%s: recovered after %d consecutive successful check-ins", m.Slug, m.ConsecutiveSuccesses)
	if typ == TypeMonitorFailed {
		title = fmt.Sprintf("%s: %d consecutive failed check-ins (threshold %d)", m.Slug, m.ConsecutiveFailures, m.FailureThreshold)
	}
	return n.deliver(ctx, projectID, store.AlertType(typ), func(*time.Time) Payload {
		return Payload{Type: typ, Project: p.Name, ProjectSlug: p.Slug, Title: title, URL: n.link(p.Slug, "/monitors/"+url.PathEscape(m.Slug))}
	})
}

func strOrEmpty(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

func randomCheckInID() (sentry.ID, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return sentry.ID(hex.EncodeToString(b[:])), nil
}
