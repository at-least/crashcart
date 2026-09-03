package store

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/crashcartapp/crashcart/internal/sentry"
)

type Monitor struct {
	ProjectID            int64          `json:"project_id"`
	Slug                 string         `json:"slug"`
	ScheduleType         string         `json:"schedule_type"`
	ScheduleValue        string         `json:"schedule_value"`
	ScheduleUnit         *string        `json:"schedule_unit"`
	Timezone             string         `json:"timezone"`
	CheckinMarginMin     int32          `json:"checkin_margin_min"`
	MaxRuntimeMin        int32          `json:"max_runtime_min"`
	FailureThreshold     int32          `json:"failure_threshold"`
	RecoveryThreshold    int32          `json:"recovery_threshold"`
	LastStatus           *CheckinStatus `json:"last_status"`
	ConsecutiveFailures  int32          `json:"consecutive_failures"`
	ConsecutiveSuccesses int32          `json:"consecutive_successes"`
	Alerting             bool           `json:"alerting"`
	NextExpectedAt       *time.Time     `json:"next_expected_at"`
	LastCheckinAt        *time.Time     `json:"last_checkin_at"`
	CreatedAt            time.Time      `json:"created_at"`
}

const monitorColumns = `project_id, slug, schedule_type, schedule_value, schedule_unit, timezone, checkin_margin_min,
	max_runtime_min, failure_threshold, recovery_threshold, last_status, consecutive_failures, consecutive_successes,
	alerting, next_expected_at, last_checkin_at, created_at`

// UpsertMonitorParams is the SDK's monitor_config upsert: schedule/thresholds
// are overwritten, state (last_status, consecutive_*, next_expected_at,
// last_checkin_at) is untouched — a re-upload of the same schedule must
// not reset a monitor's alerting history.
type UpsertMonitorParams struct {
	ProjectID         int64
	Slug              string
	ScheduleType      string
	ScheduleValue     string
	ScheduleUnit      *string
	Timezone          string
	CheckinMarginMin  int32
	MaxRuntimeMin     int32
	FailureThreshold  int32
	RecoveryThreshold int32
}

func UpsertMonitor(ctx context.Context, db DB, p UpsertMonitorParams) (Monitor, error) {
	return scanOne[Monitor](db.Query(ctx, `INSERT INTO monitors (project_id, slug, schedule_type, schedule_value, schedule_unit, timezone,
		                      checkin_margin_min, max_runtime_min, failure_threshold, recovery_threshold)
		VALUES (@ProjectID, @Slug, @ScheduleType, @ScheduleValue, @ScheduleUnit, @Timezone,
		        @CheckinMarginMin, @MaxRuntimeMin, @FailureThreshold, @RecoveryThreshold)
		ON CONFLICT (project_id, slug) DO UPDATE SET
		    schedule_type = EXCLUDED.schedule_type, schedule_value = EXCLUDED.schedule_value, schedule_unit = EXCLUDED.schedule_unit,
		    timezone = EXCLUDED.timezone, checkin_margin_min = EXCLUDED.checkin_margin_min, max_runtime_min = EXCLUDED.max_runtime_min,
		    failure_threshold = EXCLUDED.failure_threshold, recovery_threshold = EXCLUDED.recovery_threshold
		RETURNING `+monitorColumns,
		pgx.StrictStructArgs(p)))
}

func GetMonitor(ctx context.Context, db DB, projectID int64, slug string) (Monitor, error) {
	return scanOne[Monitor](db.Query(ctx, "SELECT "+monitorColumns+" FROM monitors WHERE project_id = $1 AND slug = $2", projectID, slug))
}

// ListMonitors: project-scoped, alphabetical: the Monitors page and its API.
func ListMonitors(ctx context.Context, db DB, projectID int64) ([]Monitor, error) {
	rows, err := db.Query(ctx, "SELECT "+monitorColumns+" FROM monitors WHERE project_id = $1 ORDER BY slug", projectID)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowToStructByName[Monitor])
}

func DeleteMonitor(ctx context.Context, db DB, projectID int64, slug string) (int64, error) {
	tag, err := db.Exec(ctx, "DELETE FROM monitors WHERE project_id = $1 AND slug = $2", projectID, slug)
	if err != nil {
		return 0, err
	}
	return tag.RowsAffected(), nil
}

// RecordMonitorResultParams advances a monitor's state after a terminal
// check-in (ingest) or a missed/timeout detection (alerts.CheckMonitors)
// — never after an in_progress one. next_expected_at and the consecutive
// counters are computed in Go (the schedule math needs the parsed
// cron/interval schedule); this just writes the result.
type RecordMonitorResultParams struct {
	ProjectID            int64
	Slug                 string
	LastStatus           CheckinStatus
	ConsecutiveFailures  int32
	ConsecutiveSuccesses int32
	Alerting             bool
	NextExpectedAt       time.Time
	LastCheckinAt        time.Time
}

// RecordMonitorResult binds by name: ConsecutiveFailures/ConsecutiveSuccesses
// (adjacent int32) and NextExpectedAt/LastCheckinAt (adjacent time.Time)
// are exactly the pairs a positional arg list silently swaps on reorder.
func RecordMonitorResult(ctx context.Context, db DB, p RecordMonitorResultParams) error {
	_, err := db.Exec(ctx, `UPDATE monitors SET
		    last_status = @LastStatus::checkin_status,
		    consecutive_failures = @ConsecutiveFailures::int,
		    consecutive_successes = @ConsecutiveSuccesses::int,
		    alerting = @Alerting::bool,
		    next_expected_at = @NextExpectedAt::timestamptz,
		    last_checkin_at = @LastCheckinAt::timestamptz
		WHERE project_id = @ProjectID AND slug = @Slug`,
		pgx.StrictStructArgs(p))
	return err
}

// DueMonitors: monitors whose next expected check-in has passed
// (alerts.CheckMonitors, every minute): each becomes one synthetic
// `missed` check-in.
func DueMonitors(ctx context.Context, db DB, now time.Time) ([]Monitor, error) {
	rows, err := db.Query(ctx, "SELECT "+monitorColumns+" FROM monitors WHERE next_expected_at IS NOT NULL AND next_expected_at < $1::timestamptz", now)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowToStructByName[Monitor])
}

// TimedOutCheckInsRow is an in_progress check-in that outlived its
// monitor's max_runtime_min.
type TimedOutCheckInsRow struct {
	ProjectID   int64
	MonitorSlug string
	CheckInID   sentry.ID
	StartedAt   time.Time
}

// TimedOutCheckIns: in_progress check-ins that outlived their monitor's
// max_runtime_min (alerts.CheckMonitors): each is flipped to `timeout`.
func TimedOutCheckIns(ctx context.Context, db DB, before time.Time) ([]TimedOutCheckInsRow, error) {
	rows, err := db.Query(ctx, `SELECT c.project_id, c.monitor_slug, c.check_in_id, c.started_at
		FROM monitor_checkins c JOIN monitors m ON m.project_id = c.project_id AND m.slug = c.monitor_slug
		WHERE c.status = 'in_progress' AND c.started_at + make_interval(mins => m.max_runtime_min) < $1`, before)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowToStructByName[TimedOutCheckInsRow])
}

type MonitorCheckin struct {
	StartedAt   time.Time     `json:"started_at"`
	ProjectID   int64         `json:"project_id"`
	MonitorSlug string        `json:"monitor_slug"`
	CheckInID   sentry.ID     `json:"check_in_id"`
	Status      CheckinStatus `json:"status"`
	DurationS   *float32      `json:"duration_s"`
	Release     *string       `json:"release"`
	Environment *string       `json:"environment"`
}

const checkinColumns = "started_at, project_id, monitor_slug, check_in_id, status, duration_s, release, environment"

// FindOpenCheckIn: the row a check-in item resolves to before writing: by
// its own check_in_id, or — the all-zero shorthand — the monitor's
// latest in_progress row. No match means a fresh row (a new check_in_id,
// or a zero id with nothing open to update).
func FindOpenCheckIn(ctx context.Context, db DB, projectID int64, monitorSlug string, zero bool, checkInID sentry.ID) (MonitorCheckin, error) {
	return scanOne[MonitorCheckin](db.Query(ctx, `SELECT `+checkinColumns+` FROM monitor_checkins
		WHERE project_id = $1 AND monitor_slug = $2
		  AND (($3::bool AND status = 'in_progress') OR (NOT $3::bool AND check_in_id = $4::uuid))
		ORDER BY started_at DESC LIMIT 1`, projectID, monitorSlug, zero, checkInID))
}

type InsertCheckInParams struct {
	StartedAt   time.Time
	ProjectID   int64
	MonitorSlug string
	CheckInID   sentry.ID
	Status      CheckinStatus
	DurationS   *float32
	Release     *string
	Environment *string
}

func InsertCheckIn(ctx context.Context, db DB, p InsertCheckInParams) error {
	_, err := db.Exec(ctx, "INSERT INTO monitor_checkins (started_at, project_id, monitor_slug, check_in_id, status, duration_s, release, environment) "+
		"VALUES (@StartedAt, @ProjectID, @MonitorSlug, @CheckInID, @Status, @DurationS, @Release, @Environment)",
		pgx.StrictStructArgs(p))
	return err
}

// UpdateCheckInParams is a later check-in of the same run (the same
// check_in_id, or the all-zero shorthand resolved by FindOpenCheckIn to
// an existing row): status advances, duration/release/environment are
// kept if this one did not send them. started_at (the partition key) is
// never touched.
type UpdateCheckInParams struct {
	ProjectID   int64
	MonitorSlug string
	CheckInID   sentry.ID
	StartedAt   time.Time
	Status      CheckinStatus
	DurationS   *float32
	Release     *string
	Environment *string
}

func UpdateCheckIn(ctx context.Context, db DB, p UpdateCheckInParams) error {
	_, err := db.Exec(ctx, `UPDATE monitor_checkins SET status = @Status, duration_s = COALESCE(@DurationS::real, duration_s),
		    release = COALESCE(@Release::text, release), environment = COALESCE(@Environment::text, environment)
		WHERE project_id = @ProjectID AND monitor_slug = @MonitorSlug AND check_in_id = @CheckInID AND started_at = @StartedAt`,
		pgx.StrictStructArgs(p))
	return err
}

// ListCheckIns: one monitor's recent check-ins, newest first: the
// Monitors detail page.
func ListCheckIns(ctx context.Context, db DB, projectID int64, monitorSlug string, limit int32) ([]MonitorCheckin, error) {
	rows, err := db.Query(ctx, "SELECT "+checkinColumns+" FROM monitor_checkins WHERE project_id = $1 AND monitor_slug = $2 ORDER BY started_at DESC, check_in_id DESC LIMIT $3",
		projectID, monitorSlug, limit)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowToStructByName[MonitorCheckin])
}
