// Package retention deletes data older than RETENTION_DAYS from every table.
package retention

import (
	"context"
	"log/slog"
	"time"

	"github.com/newlix/crashcart/internal/db/sqlc"
	"github.com/newlix/crashcart/internal/store"
)

// batchSize bounds each events DELETE so a large backlog is trimmed in
// short transactions instead of one long lock-holding statement.
const batchSize = 5000

// Runner performs one retention pass per Run call.
type Runner struct {
	Store *store.Store
	Days  int
	Log   *slog.Logger
	Now   func() time.Time
}

// Report counts what one pass removed.
type Report struct {
	Events, Issues, UserDevices, HourlyStats, ReleaseHealth int64
}

// Run deletes everything older than the cutoff.
func (r *Runner) Run(ctx context.Context) (Report, error) {
	now := r.Now().UTC()
	cutoff := now.Add(-time.Duration(r.Days) * 24 * time.Hour)
	q := r.Store.Queries()
	var rep Report

	for {
		n, err := q.DeleteEventsBefore(ctx, sqlc.DeleteEventsBeforeParams{OccurredAt: cutoff, Limit: batchSize})
		if err != nil {
			return rep, err
		}
		rep.Events += n
		if n < batchSize {
			break
		}
	}
	var err error
	if rep.HourlyStats, err = q.DeleteHourlyStatsBefore(ctx, cutoff.Truncate(time.Hour)); err != nil {
		return rep, err
	}
	if rep.Issues, err = q.DeleteIssuesBefore(ctx, cutoff); err != nil {
		return rep, err
	}
	if rep.UserDevices, err = q.DeleteUserDevicesBefore(ctx, cutoff); err != nil {
		return rep, err
	}
	day := time.Date(cutoff.Year(), cutoff.Month(), cutoff.Day(), 0, 0, 0, 0, time.UTC)
	if rep.ReleaseHealth, err = q.DeleteReleaseHealthBefore(ctx, day); err != nil {
		return rep, err
	}
	r.Log.Info("retention complete", "cutoff", cutoff.Format(time.RFC3339), "events", rep.Events,
		"issues", rep.Issues, "user_devices", rep.UserDevices, "hourly_stats", rep.HourlyStats, "release_health", rep.ReleaseHealth)
	return rep, nil
}
