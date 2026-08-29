package store

import (
	"context"
	"strconv"
	"strings"
	"time"

	"github.com/crashcartapp/crashcart/internal/db/sqlc"
)

// IssueFilter is the optional WHERE / ORDER of ListIssues. Zero values are
// ignored. From/To bound last_seen; Sort must be one of
// last_seen, first_seen, events (default last_seen), always descending.
type IssueFilter struct {
	ProjectID int64
	Status    string
	Level     string
	Release   string    // first_release = r OR last_release = r
	Query     string    // title / error_type ILIKE %q%
	From, To  time.Time // last_seen range [From, To); zero = unbounded
	Sort      string
	Limit     int
	Offset    int
}

// issueSorts is the ORDER BY allowlist.
var issueSorts = map[string]string{
	"last_seen":  "last_seen DESC",
	"first_seen": "first_seen DESC",
	"events":     "event_count DESC, last_seen DESC",
}

func (f IssueFilter) where() (string, []any) {
	var w []string
	var args []any
	add := func(cond string, v any) {
		args = append(args, v)
		w = append(w, strings.ReplaceAll(cond, "?", "$"+strconv.Itoa(len(args))))
	}
	add("project_id = ?", f.ProjectID)
	if f.Status != "" {
		add("status = ?", f.Status)
	}
	if f.Level != "" {
		add("level = ?", f.Level)
	}
	if f.Release != "" {
		args = append(args, f.Release)
		n := strconv.Itoa(len(args))
		w = append(w, "(first_release = $"+n+" OR last_release = $"+n+")")
	}
	if f.Query != "" {
		args = append(args, "%"+escapeLike(f.Query)+"%")
		n := strconv.Itoa(len(args))
		w = append(w, "(title ILIKE $"+n+" OR error_type ILIKE $"+n+")")
	}
	if !f.From.IsZero() {
		add("last_seen >= ?", f.From)
	}
	if !f.To.IsZero() {
		add("last_seen < ?", f.To)
	}
	return strings.Join(w, " AND "), args
}

const issueColumns = `project_id, fingerprint, title, level, error_type, screen, platform, status, status_by, event_count,
	stored_count, first_seen, last_seen, first_release, last_release, releases, resolved_releases, created_at, updated_at`

// ListIssues returns one page of issues matching f plus the total count.
// Limit defaults to 50 and caps at 500.
func (s *Store) ListIssues(ctx context.Context, f IssueFilter) (issues []sqlc.Issue, total int64, err error) {
	order, ok := issueSorts[f.Sort]
	if !ok {
		order = issueSorts["last_seen"]
	}
	limit := f.Limit
	if limit <= 0 {
		limit = 50
	}
	if limit > 500 {
		limit = 500
	}
	offset := max(f.Offset, 0)
	where, args := f.where()
	// The page and the total in one round trip: count(*) OVER () is the
	// unpaged match count on every row. An empty page (offset past the
	// end) carries no rows, so the total is re-counted only then.
	sql := "SELECT " + issueColumns + ", count(*) OVER () FROM issues WHERE " + where + " ORDER BY " + order +
		" LIMIT " + strconv.Itoa(limit) + " OFFSET " + strconv.Itoa(offset)
	r, err := s.Pool.Query(ctx, sql, args...)
	if err != nil {
		return nil, 0, err
	}
	defer r.Close()
	for r.Next() {
		var is sqlc.Issue
		if err := r.Scan(&is.ProjectID, &is.Fingerprint, &is.Title, &is.Level, &is.ErrorType, &is.Screen, &is.Platform, &is.Status, &is.StatusBy,
			&is.EventCount, &is.StoredCount, &is.FirstSeen, &is.LastSeen, &is.FirstRelease, &is.LastRelease, &is.Releases, &is.ResolvedReleases,
			&is.CreatedAt, &is.UpdatedAt, &total); err != nil {
			return nil, 0, err
		}
		issues = append(issues, is)
	}
	if err := r.Err(); err != nil {
		return nil, 0, err
	}
	if len(issues) == 0 && offset > 0 {
		if err := s.Pool.QueryRow(ctx, "SELECT count(*) FROM issues WHERE "+where, args...).Scan(&total); err != nil {
			return nil, 0, err
		}
	}
	return issues, total, nil
}
