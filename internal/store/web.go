package store

// Queries used only by the viewer (internal/web).

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/at-least/crashcart/internal/sentry"
)

// IssuesIntroducedPerReleaseRow is how many issues were first seen on one
// release.
type IssuesIntroducedPerReleaseRow struct {
	Release *string `json:"release"`
	N       int64   `json:"n"`
}

func IssuesIntroducedPerRelease(ctx context.Context, db DB, projectID int64) ([]IssuesIntroducedPerReleaseRow, error) {
	rows, err := db.Query(ctx, `SELECT first_release AS release, count(*)::bigint AS n
FROM issues WHERE project_id = $1 AND first_release IS NOT NULL
GROUP BY first_release`, projectID)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowToStructByName[IssuesIntroducedPerReleaseRow])
}

func ListIssuesIntroducedIn(ctx context.Context, db DB, projectID int64, release *string, limit int32) ([]Issue, error) {
	rows, err := db.Query(ctx, "SELECT "+issueColumns+` FROM issues WHERE project_id = $1 AND first_release = $2
ORDER BY event_count DESC LIMIT $3`, projectID, release, limit)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowToStructByName[Issue])
}

// ListIssuesPresentIn is issues still open whose latest event came from
// this release.
func ListIssuesPresentIn(ctx context.Context, db DB, projectID int64, release *string, limit int32) ([]Issue, error) {
	rows, err := db.Query(ctx, "SELECT "+issueColumns+` FROM issues WHERE project_id = $1 AND last_release = $2 AND status NOT IN ('resolved', 'ignored')
ORDER BY event_count DESC LIMIT $3`, projectID, release, limit)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowToStructByName[Issue])
}

// PortalUnhandledRow is one project's unhandled count in a window.
type PortalUnhandledRow struct {
	ProjectID int64 `json:"project_id"`
	Unhandled int64 `json:"unhandled"`
}

// PortalUnhandled: unhandled per project in a window (one row per
// project) — the portal reads one query per statistic across every
// project, not four per project.
func PortalUnhandled(ctx context.Context, db DB, from, to time.Time) ([]PortalUnhandledRow, error) {
	rows, err := db.Query(ctx, `SELECT project_id, sum(unhandled)::bigint AS unhandled
FROM event_stats_hourly WHERE bucket >= $1::timestamptz AND bucket < $2::timestamptz
GROUP BY 1`, from, to)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowToStructByName[PortalUnhandledRow])
}

// PortalPlatformsRow is one project's platform event count in a window.
type PortalPlatformsRow struct {
	ProjectID int64  `json:"project_id"`
	Platform  string `json:"platform"`
	Events    int64  `json:"events"`
}

// PortalPlatforms is raw platforms per project in a window, most events
// first.
func PortalPlatforms(ctx context.Context, db DB, from, to time.Time) ([]PortalPlatformsRow, error) {
	rows, err := db.Query(ctx, `SELECT project_id, platform, sum(events)::bigint AS events
FROM event_stats_hourly WHERE bucket >= $1::timestamptz AND bucket < $2::timestamptz
GROUP BY 1, 2 ORDER BY 1, 3 DESC, 2`, from, to)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowToStructByName[PortalPlatformsRow])
}

// PortalLatestReleasesRow is one project's most recently active release.
type PortalLatestReleasesRow struct {
	ProjectID int64  `json:"project_id"`
	Release   string `json:"release"`
}

// PortalLatestReleases is the most recently active release per project
// (ties by name, like LatestReleaseHealth).
func PortalLatestReleases(ctx context.Context, db DB, from, to time.Time) ([]PortalLatestReleasesRow, error) {
	rows, err := db.Query(ctx, `SELECT DISTINCT ON (project_id) project_id, release
FROM (SELECT project_id, release, max(bucket) AS last FROM event_stats_hourly
      WHERE bucket >= $1::timestamptz AND bucket < $2::timestamptz AND release <> ''
      GROUP BY 1, 2) t
ORDER BY project_id, last DESC, release DESC`, from, to)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowToStructByName[PortalLatestReleasesRow])
}

// PortalOpenIssuesRow is one project's open-issue count.
type PortalOpenIssuesRow struct {
	ProjectID int64 `json:"project_id"`
	N         int64 `json:"n"`
}

func PortalOpenIssues(ctx context.Context, db DB) ([]PortalOpenIssuesRow, error) {
	rows, err := db.Query(ctx, `SELECT project_id, count(*)::bigint AS n FROM issues
WHERE status IN ('unresolved', 'regression') GROUP BY 1`)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowToStructByName[PortalOpenIssuesRow])
}

// PortalReleaseHealthRow is one project's session totals for its latest
// active release.
type PortalReleaseHealthRow struct {
	ProjectID int64 `json:"project_id"`
	Total     int64 `json:"total"`
	Crashed   int64 `json:"crashed"`
}

// PortalReleaseHealth is the session totals of one release per project
// (the latest active one).
func PortalReleaseHealth(ctx context.Context, db DB, projectIDs []int64, releases []string, from, to time.Time) ([]PortalReleaseHealthRow, error) {
	rows, err := db.Query(ctx, `SELECT k.project_id::bigint AS project_id, COALESCE(sum(h.total), 0)::bigint AS total, COALESCE(sum(h.crashed), 0)::bigint AS crashed
FROM (SELECT unnest($1::bigint[]) AS project_id, unnest($2::text[]) AS release) AS k
JOIN release_health_hourly h ON h.project_id = k.project_id AND h.release = k.release
  AND h.bucket >= $3::timestamptz AND h.bucket < $4::timestamptz
GROUP BY k.project_id`, projectIDs, releases, from, to)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowToStructByName[PortalReleaseHealthRow])
}

// LatestIssueEvent is bounded to the issue's own [first_seen, last_seen]
// so only those partitions are read (the issue row is the exact range of
// its events).
func LatestIssueEvent(ctx context.Context, db DB, projectID int64, fingerprint *sentry.ID, from, to time.Time) (Event, error) {
	return scanEvent(db.QueryRow(ctx, "SELECT "+eventColumns+" FROM events WHERE project_id = $1 AND fingerprint = $2 AND occurred_at >= $3::timestamptz AND occurred_at < $4::timestamptz ORDER BY occurred_at DESC LIMIT 1",
		projectID, fingerprint, from, to))
}
