// Queries used only by internal/api (Overview): simple issues-table
// counts that don't need the full Issue row shape (issues.go, phase 7).
package store

import (
	"context"
	"time"
)

// CountRegressions: issues currently in 'regression' that were seen since lastSeen.
func CountRegressions(ctx context.Context, db DB, projectID int64, lastSeen time.Time) (int64, error) {
	var n int64
	err := db.QueryRow(ctx, "SELECT count(*) FROM issues WHERE project_id = $1 AND status = 'regression' AND last_seen >= $2", projectID, lastSeen).Scan(&n)
	return n, err
}

// CountRegressionsIn: issues currently in 'regression' that were seen in [from, to).
func CountRegressionsIn(ctx context.Context, db DB, projectID int64, from, to time.Time) (int64, error) {
	var n int64
	err := db.QueryRow(ctx, "SELECT count(*) FROM issues WHERE project_id = $1 AND status = 'regression' AND last_seen >= $2 AND last_seen < $3", projectID, from, to).Scan(&n)
	return n, err
}

// NewIssuesByReleaseRow is one release's count of issues first seen in a window.
type NewIssuesByReleaseRow struct {
	Release *string
	N       int64
}

// NewIssuesByRelease: issues first seen in the window, grouped by the
// release that introduced them.
func NewIssuesByRelease(ctx context.Context, db DB, projectID int64, from, to time.Time) ([]NewIssuesByReleaseRow, error) {
	rows, err := db.Query(ctx, `SELECT first_release AS release, count(*)::bigint AS n
		FROM issues
		WHERE project_id = $1 AND first_seen >= $2 AND first_seen < $3 AND first_release IS NOT NULL
		GROUP BY first_release`, projectID, from, to)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	items := []NewIssuesByReleaseRow{}
	for rows.Next() {
		var r NewIssuesByReleaseRow
		if err := rows.Scan(&r.Release, &r.N); err != nil {
			return nil, err
		}
		items = append(items, r)
	}
	return items, rows.Err()
}
