// Package retention deletes data older than RETENTION_DAYS from every table.
package retention

import (
	"context"
	"log/slog"
	"time"

	"github.com/newlix/crashcart/internal/db"
	"github.com/newlix/crashcart/internal/db/sqlc"
	"github.com/newlix/crashcart/internal/pk"
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
	PartitionsDropped, PartitionsCreated                    []string
}

// Run deletes everything older than the cutoff.
func (r *Runner) Run(ctx context.Context) (Report, error) {
	now := r.Now().UTC()
	cutoff := now.Add(-time.Duration(r.Days) * 24 * time.Hour)
	q := r.Store.Queries()
	var rep Report
	pool := r.Store.Pool()

	// Whole days age out as a DROP TABLE (no dead tuples, no vacuum); the
	// batched DELETE below only has to handle the default partition.
	var err error
	if rep.PartitionsDropped, err = db.DropPartitionsBefore(ctx, pool, cutoff); err != nil {
		return rep, err
	}
	if rep.PartitionsCreated, err = db.EnsureUpcomingPartitions(ctx, pool, now); err != nil {
		return rep, err
	}
	for {
		n, err := q.DeleteEventsBefore(ctx, sqlc.DeleteEventsBeforeParams{CutoffID: pk.Lower(cutoff), Batch: batchSize})
		if err != nil {
			return rep, err
		}
		rep.Events += n
		if n < batchSize {
			break
		}
	}
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
		"partitions_dropped", rep.PartitionsDropped, "partitions_created", rep.PartitionsCreated,
		"issues", rep.Issues, "user_devices", rep.UserDevices, "hourly_stats", rep.HourlyStats, "release_health", rep.ReleaseHealth)
	return rep, nil
}
