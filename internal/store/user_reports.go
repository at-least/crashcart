package store

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/at-least/crashcart/internal/sentry"
)

type UserReport struct {
	ProjectID  int64     `json:"project_id"`
	EventID    sentry.ID `json:"event_id"`
	ReceivedAt time.Time `json:"received_at"`
	Name       *string   `json:"name"`
	Email      *string   `json:"email"`
	Comments   string    `json:"comments"`
}

const userReportColumns = "project_id, event_id, received_at, name, email, comments"

// UpsertUserReport: one report per event, as the protocol requires; a
// resend overwrites (received_at keeps the report's original arrival time).
func UpsertUserReport(ctx context.Context, db DB, projectID int64, eventID sentry.ID, name, email *string, comments string) error {
	_, err := db.Exec(ctx, `INSERT INTO user_reports (project_id, event_id, name, email, comments)
		VALUES ($1, $2, $3, $4, $5)
		ON CONFLICT (project_id, event_id) DO UPDATE SET
		    name = EXCLUDED.name, email = EXCLUDED.email, comments = EXCLUDED.comments`,
		projectID, eventID, name, email, comments)
	return err
}

// GetUserReport: whether an event has a report, for the event page / API
// — no join to events (the report outlives a sampled-out or
// not-yet-arrived event).
func GetUserReport(ctx context.Context, db DB, projectID int64, eventID sentry.ID) (UserReport, error) {
	return scanOne[UserReport](db.Query(ctx, "SELECT "+userReportColumns+" FROM user_reports WHERE project_id = $1 AND event_id = $2",
		projectID, eventID))
}

// ListUserReports: newest first, project-scoped: the Feedback page and
// its API endpoint. Not tied to events, so a report whose event was
// sampled out still shows.
func ListUserReports(ctx context.Context, db DB, projectID int64, limit, offset int32) ([]UserReport, error) {
	rows, err := db.Query(ctx, "SELECT "+userReportColumns+" FROM user_reports WHERE project_id = $1 ORDER BY received_at DESC, event_id DESC LIMIT $2 OFFSET $3",
		projectID, limit, offset)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowToStructByName[UserReport])
}

func CountUserReports(ctx context.Context, db DB, projectID int64) (int64, error) {
	var n int64
	err := db.QueryRow(ctx, "SELECT count(*) FROM user_reports WHERE project_id = $1", projectID).Scan(&n)
	return n, err
}

// SweepUserReports: its own expiry (no join to events) — a report
// survives independently of whether its event was ever stored, so it is
// cut by its own clock.
func SweepUserReports(ctx context.Context, db DB, before time.Time) (int64, error) {
	tag, err := db.Exec(ctx, "DELETE FROM user_reports WHERE received_at < $1", before)
	if err != nil {
		return 0, err
	}
	return tag.RowsAffected(), nil
}
