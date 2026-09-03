package store

import (
	"context"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/at-least/crashcart/internal/sentry"
)

// Issue is the full row.
type Issue struct {
	ProjectID             int64       `json:"project_id"`
	Fingerprint           sentry.ID   `json:"fingerprint"`
	Title                 string      `json:"title"`
	Level                 EventLevel  `json:"level"`
	ErrorType             *string     `json:"error_type"`
	Transaction           *string     `json:"transaction"`
	Platform              *string     `json:"platform"`
	Status                IssueStatus `json:"status"`
	StatusBy              *string     `json:"status_by"`
	EventCount            int64       `json:"event_count"`
	StoredCount           int64       `json:"stored_count"`
	FirstSeen             time.Time   `json:"first_seen"`
	LastSeen              time.Time   `json:"last_seen"`
	FirstRelease          *string     `json:"first_release"`
	LastRelease           *string     `json:"last_release"`
	Releases              []string    `json:"releases"`
	ResolvedReleases      []string    `json:"resolved_releases"`
	IgnoreUntil           *time.Time  `json:"ignore_until"`
	IgnoreUntilCount      *int64      `json:"ignore_until_count"`
	IgnoreUntilEscalating bool        `json:"ignore_until_escalating"`
	IgnoreBaseline        *int64      `json:"ignore_baseline"`
	CreatedAt             time.Time   `json:"created_at"`
	UpdatedAt             time.Time   `json:"updated_at"`
}

const issueColumns = `project_id, fingerprint, title, level, error_type, transaction, platform, status, status_by, event_count,
	stored_count, first_seen, last_seen, first_release, last_release, releases, resolved_releases,
	ignore_until, ignore_until_count, ignore_until_escalating, ignore_baseline, created_at, updated_at`

func GetIssue(ctx context.Context, db DB, projectID int64, fingerprint sentry.ID) (Issue, error) {
	return scanOne[Issue](db.Query(ctx, "SELECT "+issueColumns+" FROM issues WHERE project_id = $1 AND fingerprint = $2", projectID, fingerprint))
}

// UpsertIssueParams is called once per (project, fingerprint) per envelope
// with the folded count; Releases are the distinct releases of the folded
// events (empty for none); Level is the latest event's.
type UpsertIssueParams struct {
	ProjectID    int64
	Fingerprint  sentry.ID
	Title        string
	Level        EventLevel
	ErrorType    *string
	Transaction  *string
	Platform     *string
	EventCount   int64
	StoredCount  int64
	FirstSeen    time.Time
	LastSeen     time.Time
	FirstRelease *string
	Releases     []string
	// Regress: only ingest sets this true (symbolication moving an old
	// event between issues is not new evidence). Sentry's "resolved in
	// next release": a resolved issue seen again on a release outside the
	// set it had been seen on when it was resolved (old builds in the
	// field are inside that set; a fixed release is not) becomes a
	// regression.
	Regress bool
}

// UpsertIssueRow is the row after the update plus whether it was created
// in this call and whether this call flipped it to regression.
type UpsertIssueRow struct {
	Issue
	Created   bool `json:"created"`
	Regressed bool `json:"regressed"`
}

func UpsertIssue(ctx context.Context, db DB, p UpsertIssueParams) (UpsertIssueRow, error) {
	return scanOne[UpsertIssueRow](db.Query(ctx, `WITH prev AS (SELECT status FROM issues WHERE project_id = @ProjectID AND fingerprint = @Fingerprint)
INSERT INTO issues (project_id, fingerprint, title, level, error_type, transaction, platform,
                    event_count, stored_count, first_seen, last_seen, first_release, last_release, releases)
VALUES (@ProjectID, @Fingerprint, @Title, @Level, @ErrorType, @Transaction, @Platform, @EventCount, @StoredCount,
        @FirstSeen, @LastSeen, @FirstRelease, @FirstRelease, COALESCE(@Releases::text[], '{}'))
ON CONFLICT (project_id, fingerprint) DO UPDATE SET
    event_count  = issues.event_count + EXCLUDED.event_count,
    stored_count = issues.stored_count + EXCLUDED.stored_count,
    last_seen    = GREATEST(issues.last_seen, EXCLUDED.last_seen),
    first_seen   = LEAST(issues.first_seen, EXCLUDED.first_seen),
    last_release = CASE WHEN EXCLUDED.last_seen >= issues.last_seen THEN COALESCE(EXCLUDED.last_release, issues.last_release) ELSE issues.last_release END,
    level        = CASE WHEN EXCLUDED.last_seen >= issues.last_seen THEN EXCLUDED.level ELSE issues.level END,
    releases     = CASE WHEN issues.releases @> EXCLUDED.releases THEN issues.releases
                        ELSE (SELECT array_agg(DISTINCT r ORDER BY r) FROM unnest(issues.releases || EXCLUDED.releases) AS r) END,
    status       = CASE WHEN @Regress::bool AND issues.status = 'resolved'
                         AND NOT (COALESCE(issues.resolved_releases, '{}') @> EXCLUDED.releases)
                        THEN 'regression' ELSE issues.status END,
    updated_at   = now()
RETURNING `+issueColumns+`, (xmax = 0) AS created,
          COALESCE(issues.status = 'regression' AND (SELECT status FROM prev) = 'resolved', false)::bool AS regressed`,
		pgx.StrictStructArgs(p)))
}

// SetIssueStatusParams: resolving records the releases seen so far
// (regression detection). Ignoring records its conditions (Sentry's
// archive "until …"): a time, a number of further events
// (ignore_until_count = event_count + N), or escalation — for which the
// baseline is the issue's stored events over the 24 full hours before now
// (issue_stats_hourly), the same baseline the unhandled_spike rule uses.
// Any other status clears them.
type SetIssueStatusParams struct {
	ProjectID        int64
	Fingerprint      sentry.ID
	Status           IssueStatus
	StatusBy         *string
	IgnoreUntil      *time.Time
	IgnoreEvents     *int64
	IgnoreEscalating bool
}

const setIssueStatusSQL = `SET status = @Status::issue_status, status_by = @StatusBy,
    resolved_releases = CASE WHEN @Status::issue_status = 'resolved' THEN releases ELSE resolved_releases END,
    ignore_until = CASE WHEN @Status::issue_status = 'ignored' THEN @IgnoreUntil::timestamptz END,
    ignore_until_count = CASE WHEN @Status::issue_status = 'ignored' THEN event_count + @IgnoreEvents::bigint END,
    ignore_until_escalating = @Status::issue_status = 'ignored' AND @IgnoreEscalating::bool,
    ignore_baseline = CASE WHEN @Status::issue_status = 'ignored' AND @IgnoreEscalating::bool THEN
        (SELECT COALESCE(sum(h.events), 0)::bigint FROM issue_stats_hourly h
         WHERE h.project_id = issues.project_id AND h.fingerprint = issues.fingerprint
           AND h.bucket >= date_trunc('hour', now()) - INTERVAL '24 hours' AND h.bucket < date_trunc('hour', now())) END,
    updated_at = now()`

func SetIssueStatus(ctx context.Context, db DB, p SetIssueStatusParams) (Issue, error) {
	return scanOne[Issue](db.Query(ctx, "UPDATE issues "+setIssueStatusSQL+" WHERE issues.project_id = @ProjectID AND issues.fingerprint = @Fingerprint RETURNING "+issueColumns,
		pgx.StrictStructArgs(p)))
}

// SetIssuesStatus is the bulk form of SetIssueStatus (same rules). p's
// ProjectID and Fingerprint are ignored — the target set comes from
// projectID and fingerprints instead — so its args are bound by a
// StrictNamedArgs map (mixing p's fields with the two extra params) rather
// than StrictStructArgs(p), which would reject p's unreferenced fields.
func SetIssuesStatus(ctx context.Context, db DB, projectID int64, fingerprints []sentry.ID, p SetIssueStatusParams) (int64, error) {
	tag, err := db.Exec(ctx, "UPDATE issues "+setIssueStatusSQL+" WHERE issues.project_id = @TargetProjectID AND issues.fingerprint = ANY(@Fingerprints::uuid[])",
		pgx.StrictNamedArgs{
			"TargetProjectID":  projectID,
			"Fingerprints":     fingerprints,
			"Status":           p.Status,
			"StatusBy":         p.StatusBy,
			"IgnoreUntil":      p.IgnoreUntil,
			"IgnoreEvents":     p.IgnoreEvents,
			"IgnoreEscalating": p.IgnoreEscalating,
		})
	if err != nil {
		return 0, err
	}
	return tag.RowsAffected(), nil
}

// UnignoreDueRow is one ignored issue whose time or count condition is
// met, with the reason.
type UnignoreDueRow struct {
	ProjectID   int64     `json:"project_id"`
	Fingerprint sentry.ID `json:"fingerprint"`
	Reason      string    `json:"reason"`
}

// UnignoreDue flips ignored issues whose time or count condition is met
// back to unresolved (alerts.CheckIgnored). Returns them with the reason
// (read before the update: RETURNING sees the cleared columns).
func UnignoreDue(ctx context.Context, db DB) ([]UnignoreDueRow, error) {
	rows, err := db.Query(ctx, `WITH due AS (
    SELECT project_id, fingerprint, (ignore_until IS NOT NULL AND ignore_until <= now()) AS by_time
    FROM issues
    WHERE status = 'ignored'
      AND ((ignore_until IS NOT NULL AND ignore_until <= now()) OR (ignore_until_count IS NOT NULL AND event_count >= ignore_until_count))
    FOR UPDATE)
UPDATE issues i SET status = 'unresolved', ignore_until = NULL, ignore_until_count = NULL,
    ignore_until_escalating = false, ignore_baseline = NULL, updated_at = now()
FROM due d
WHERE i.project_id = d.project_id AND i.fingerprint = d.fingerprint
RETURNING i.project_id, i.fingerprint, (CASE WHEN d.by_time THEN 'time' ELSE 'count' END)::text AS reason`)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowToStructByName[UnignoreDueRow])
}

// EscalationInputsRow is one ignored-until-escalating issue's stored
// events in the exact last hour next to the baseline recorded when it was
// ignored.
type EscalationInputsRow struct {
	ProjectID   int64     `json:"project_id"`
	Fingerprint sentry.ID `json:"fingerprint"`
	Baseline    int64     `json:"baseline"`
	Recent      int64     `json:"recent"`
}

// EscalationInputs: ignored-until-escalating issues with their stored
// events in the exact last hour (raw rows through the
// events_project_fingerprint index) next to the baseline recorded when
// they were ignored.
func EscalationInputs(ctx context.Context, db DB, recentFrom time.Time) ([]EscalationInputsRow, error) {
	rows, err := db.Query(ctx, `SELECT i.project_id, i.fingerprint, COALESCE(i.ignore_baseline, 0)::bigint AS baseline,
       (SELECT count(*) FROM events e WHERE e.project_id = i.project_id AND e.fingerprint = i.fingerprint
          AND e.occurred_at >= $1::timestamptz)::bigint AS recent
FROM issues i
WHERE i.status = 'ignored' AND i.ignore_until_escalating`, recentFrom)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowToStructByName[EscalationInputsRow])
}

// EscalateIssue flips one escalating issue back to unresolved (only while
// it is still ignored-until-escalating: a concurrent status change wins).
func EscalateIssue(ctx context.Context, db DB, projectID int64, fingerprint sentry.ID) (Issue, error) {
	return scanOne[Issue](db.Query(ctx, `UPDATE issues SET status = 'unresolved', ignore_until = NULL, ignore_until_count = NULL,
    ignore_until_escalating = false, ignore_baseline = NULL, updated_at = now()
WHERE project_id = $1 AND fingerprint = $2 AND status = 'ignored' AND ignore_until_escalating
RETURNING `+issueColumns, projectID, fingerprint))
}

func AdjustIssueStoredCount(ctx context.Context, db DB, projectID int64, fingerprint sentry.ID, storedCount int64) error {
	_, err := db.Exec(ctx, "UPDATE issues SET stored_count = GREATEST(0, stored_count + $3), event_count = GREATEST(0, event_count + $3), updated_at = now() WHERE project_id = $1 AND fingerprint = $2",
		projectID, fingerprint, storedCount)
	return err
}

func DeleteEmptyIssue(ctx context.Context, db DB, projectID int64, fingerprint sentry.ID) error {
	_, err := db.Exec(ctx, "DELETE FROM issues WHERE project_id = $1 AND fingerprint = $2 AND event_count <= 0 AND status = 'unresolved'", projectID, fingerprint)
	return err
}

// CountIssuesByStatusRow is a project's issue count at one status.
type CountIssuesByStatusRow struct {
	Status IssueStatus `json:"status"`
	N      int64       `json:"n"`
}

func CountIssuesByStatus(ctx context.Context, db DB, projectID int64) ([]CountIssuesByStatusRow, error) {
	rows, err := db.Query(ctx, "SELECT status, count(*) AS n FROM issues WHERE project_id = $1 GROUP BY status", projectID)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowToStructByName[CountIssuesByStatusRow])
}

func CountNewIssues(ctx context.Context, db DB, projectID int64, firstSeen time.Time) (int64, error) {
	var n int64
	err := db.QueryRow(ctx, "SELECT count(*) FROM issues WHERE project_id = $1 AND first_seen >= $2", projectID, firstSeen).Scan(&n)
	return n, err
}

// CountNewIssuesIn is new issues in [from, to).
func CountNewIssuesIn(ctx context.Context, db DB, projectID int64, from, to time.Time) (int64, error) {
	var n int64
	err := db.QueryRow(ctx, "SELECT count(*) FROM issues WHERE project_id = $1 AND first_seen >= $2 AND first_seen < $3", projectID, from, to).Scan(&n)
	return n, err
}

func ListRegressions(ctx context.Context, db DB, projectID int64, limit int32) ([]Issue, error) {
	rows, err := db.Query(ctx, "SELECT "+issueColumns+" FROM issues WHERE project_id = $1 AND status = 'regression' ORDER BY last_seen DESC LIMIT $2", projectID, limit)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowToStructByName[Issue])
}

func ListNewIssues(ctx context.Context, db DB, projectID int64, firstSeen time.Time, limit int32) ([]Issue, error) {
	rows, err := db.Query(ctx, "SELECT "+issueColumns+" FROM issues WHERE project_id = $1 AND first_seen >= $2 ORDER BY first_seen DESC LIMIT $3", projectID, firstSeen, limit)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowToStructByName[Issue])
}

func ListIssuesByRelease(ctx context.Context, db DB, projectID int64, release *string, limit int32) ([]Issue, error) {
	rows, err := db.Query(ctx, "SELECT "+issueColumns+" FROM issues WHERE project_id = $1 AND (first_release = $2 OR last_release = $2) ORDER BY event_count DESC LIMIT $3",
		projectID, release, limit)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowToStructByName[Issue])
}

// IssueSparklinesRow is one fingerprint's event counts of every bucket in
// the window (gap-filled, in bucket order); see the chart-query note in
// stats.go.
type IssueSparklinesRow struct {
	Fingerprint sentry.ID `json:"fingerprint"`
	Counts      []int64   `json:"counts"`
}

func IssueSparklines(ctx context.Context, db DB, projectID int64, fingerprints []sentry.ID, from, to time.Time, width int64) ([]IssueSparklinesRow, error) {
	rows, err := db.Query(ctx, `WITH h AS (
    SELECT fingerprint, crashcart_bucket(bucket, $4::bigint) AS bucket, sum(events) AS events
    FROM issue_stats_hourly
    WHERE project_id = $5::bigint AND fingerprint = ANY($1::uuid[])
      AND bucket >= $2::timestamptz AND bucket < $3::timestamptz
    GROUP BY 1, 2)
SELECT f.fingerprint::uuid AS fingerprint, array_agg(COALESCE(h.events, 0)::bigint ORDER BY b)::bigint[] AS counts
FROM unnest($1::uuid[]) AS f(fingerprint)
CROSS JOIN crashcart_buckets($2::timestamptz, $3::timestamptz, $4::bigint) AS b
LEFT JOIN h ON h.fingerprint = f.fingerprint AND h.bucket = b
GROUP BY f.fingerprint`, fingerprints, from, to, width, projectID)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowToStructByName[IssueSparklinesRow])
}

// IssueTimelineRow is one bucket of an issue's event count timeline.
type IssueTimelineRow struct {
	Bucket time.Time `json:"bucket"`
	Events int64     `json:"events"`
}

func IssueTimeline(ctx context.Context, db DB, projectID int64, fingerprint sentry.ID, from, to time.Time, width int64) ([]IssueTimelineRow, error) {
	rows, err := db.Query(ctx, `WITH h AS (
    SELECT crashcart_bucket(bucket, $3::bigint) AS bucket, sum(events) AS events
    FROM issue_stats_hourly
    WHERE project_id = $4::bigint AND fingerprint = $5::uuid
      AND bucket >= $1::timestamptz AND bucket < $2::timestamptz
    GROUP BY 1)
SELECT b::timestamptz AS bucket, COALESCE(h.events, 0)::bigint AS events
FROM crashcart_buckets($1::timestamptz, $2::timestamptz, $3::bigint) AS b
LEFT JOIN h ON h.bucket = b
ORDER BY b`, from, to, width, projectID, fingerprint)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowToStructByName[IssueTimelineRow])
}

func AddIssueStored(ctx context.Context, db DB, projectID int64, fingerprint sentry.ID, storedCount int64) error {
	_, err := db.Exec(ctx, "UPDATE issues SET stored_count = stored_count + $3 WHERE project_id = $1 AND fingerprint = $2", projectID, fingerprint, storedCount)
	return err
}

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
	if f.Query = clip(f.Query, MaxFilterLen); f.Query != "" {
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

// Bounds on a filter: an OFFSET is a sort-and-discard in Postgres, and a
// filter value becomes an ILIKE pattern or an index key. Both are
// enforced here, whichever door (viewer, API) the values came through.
const (
	MaxOffset    = 10000
	MaxFilterLen = 200
)

// issuesPageRow is one row of ListIssues' page query: an Issue plus the
// unpaged match count carried on every row (RowToStructByName flattens the
// embedded Issue, matching its fields by column name same as issueColumns
// alone would).
type issuesPageRow struct {
	Issue
	Total int64
}

// ListIssues returns one page of issues matching f plus the total count.
// Limit defaults to 50 and caps at 500, Offset at MaxOffset.
func (s *Store) ListIssues(ctx context.Context, f IssueFilter) (issues []Issue, total int64, err error) {
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
	offset := min(max(f.Offset, 0), MaxOffset)
	where, args := f.where()
	// The page and the total in one round trip: count(*) OVER () is the
	// unpaged match count on every row. An empty page (offset past the
	// end) carries no rows, so the total is re-counted only then.
	sql := "SELECT " + issueColumns + ", count(*) OVER () AS total FROM issues WHERE " + where + " ORDER BY " + order +
		" LIMIT " + strconv.Itoa(limit) + " OFFSET " + strconv.Itoa(offset)
	r, err := s.Pool.Query(ctx, sql, args...)
	if err != nil {
		return nil, 0, err
	}
	pageRows, err := pgx.CollectRows(r, pgx.RowToStructByName[issuesPageRow])
	if err != nil {
		return nil, 0, err
	}
	issues = make([]Issue, len(pageRows))
	for i, pr := range pageRows {
		issues[i] = pr.Issue
		total = pr.Total
	}
	if len(issues) == 0 && offset > 0 {
		if err := s.Pool.QueryRow(ctx, "SELECT count(*) FROM issues WHERE "+where, args...).Scan(&total); err != nil {
			return nil, 0, err
		}
	}
	return issues, total, nil
}
