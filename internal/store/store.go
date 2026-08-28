// Package store is the read/update layer shared by the JSON API and the
// server-rendered viewer. It wraps the sqlc queries with domain-level
// filters and zero-filled time series.
package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/newlix/crashcart/internal/db/sqlc"
	"github.com/newlix/crashcart/internal/pk"
	"github.com/newlix/crashcart/internal/timerange"
)

// ErrNotFound is returned for missing rows.
var ErrNotFound = errors.New("not found")

// Store is safe for concurrent use.
type Store struct {
	pool *pgxpool.Pool
	q    *sqlc.Queries
	now  func() time.Time
}

// New wraps a pool.
func New(pool *pgxpool.Pool) *Store {
	return &Store{pool: pool, q: sqlc.New(pool), now: time.Now}
}

// Pool exposes the underlying pool (migrations, health checks).
func (s *Store) Pool() *pgxpool.Pool { return s.pool }

// Ping verifies connectivity.
func (s *Store) Ping(ctx context.Context) error { return s.pool.Ping(ctx) }

// ── events ──────────────────────────────────────────────────

// EventFilter selects events. Zero values mean "no filter". Without a
// Range the scan is bounded to the last DefaultLookback.
type EventFilter struct {
	Levels        []string
	Range         timerange.Range
	HasRange      bool
	DeviceID      string
	UserID        string
	Platform      string
	Release       string
	ErrorType     string
	Fingerprint   string
	CrashesOnly   bool
	DeviceModel   string
	OSVersion     string
	ErrorLocation string // substring
	Query         string // message substring
	Tags          map[string]string
	Limit         int
	Offset        int
}

// Event is the list-view projection (no payload / breadcrumbs).
type Event = sqlc.ListEventsRow

// DefaultLookback bounds unranged event scans (events has no time index
// other than its PK, so every scan must have a lower bound).
const DefaultLookback = 30 * 24 * time.Hour

// idRange converts a window to [since, until) ids; an open window ends
// "far enough in the future" to include clock-skewed events.
func (s *Store) idRange(r timerange.Range, has bool) (int64, int64) {
	now := s.now().UTC()
	if !has {
		return pk.Lower(now.Add(-DefaultLookback)), pk.Upper(now.Add(24 * time.Hour))
	}
	until := now.Add(24 * time.Hour)
	if r.Until != nil {
		until = *r.Until
	}
	return pk.Lower(r.Since), pk.Upper(until)
}

// ListEvents returns events newest first.
func (s *Store) ListEvents(ctx context.Context, f EventFilter) ([]Event, error) {
	p := sqlc.ListEventsParams{
		Levels:        nonNil(f.Levels),
		DeviceID:      opt(f.DeviceID),
		UserID:        opt(f.UserID),
		UserDevices:   []string{},
		Platform:      opt(f.Platform),
		Release:       opt(f.Release),
		ErrorType:     opt(f.ErrorType),
		Fingerprint:   opt(f.Fingerprint),
		CrashesOnly:   f.CrashesOnly,
		DeviceModel:   opt(f.DeviceModel),
		OsVersion:     opt(f.OSVersion),
		ErrorLocation: optLike(f.ErrorLocation),
		Message:       optLike(f.Query),
		Tags:          json.RawMessage("{}"),
		PageLimit:     int32(clamp(f.Limit, 1, 200, 50)),
		PageOffset:    int32(max(f.Offset, 0)),
	}
	p.SinceID, p.UntilID = s.idRange(f.Range, f.HasRange)
	if len(f.Tags) > 0 {
		b, err := json.Marshal(f.Tags)
		if err != nil {
			return nil, err
		}
		p.Tags = b
	}
	if f.UserID != "" {
		// Events from the user's known devices may carry no user id.
		devs, err := s.q.ListUserDevices(ctx, f.UserID)
		if err != nil {
			return nil, err
		}
		p.UserDevices = devs
	}
	rows, err := s.q.ListEvents(ctx, p)
	if err != nil {
		return nil, err
	}
	return rows, nil
}

// GetEvent returns the full row including payload.
func (s *Store) GetEvent(ctx context.Context, id int64) (sqlc.GetEventRow, error) {
	ev, err := s.q.GetEvent(ctx, id)
	if errors.Is(err, pgx.ErrNoRows) {
		return ev, ErrNotFound
	}
	return ev, err
}

// ── stats ───────────────────────────────────────────────────

// LevelCount is a per-level total.
type LevelCount struct {
	Level string `json:"level"`
	Count int64  `json:"count"`
}

// Stats are the headline totals for a window.
type Stats struct {
	Fatal  int64        `json:"fatal"`
	Crash  int64        `json:"crash"`
	Error  int64        `json:"error"`
	Levels []LevelCount `json:"levels"`
}

// Stats sums hourly_stats over r.
func (s *Store) Stats(ctx context.Context, r timerange.Range) (Stats, error) {
	tot, err := s.q.StatsTotals(ctx, sqlc.StatsTotalsParams{Since: r.Since, Until: r.Until})
	if err != nil {
		return Stats{}, err
	}
	lv, err := s.q.StatsByLevel(ctx, sqlc.StatsByLevelParams{Since: r.Since, Until: r.Until})
	if err != nil {
		return Stats{}, err
	}
	out := Stats{Fatal: tot.Fatal, Crash: tot.Crash, Error: tot.Error, Levels: make([]LevelCount, 0, len(lv))}
	for _, l := range lv {
		out.Levels = append(out.Levels, LevelCount{Level: l.Level, Count: l.Count})
	}
	return out, nil
}

// Point is one bucket of a crash timeline.
type Point struct {
	Time  time.Time `json:"time"`
	Count int64     `json:"count"`
}

// VolumePoint is one bucket of the fatal/error volume series.
type VolumePoint struct {
	Time  time.Time `json:"time"`
	Fatal int64     `json:"fatal"`
	Error int64     `json:"error"`
}

// CrashTimeline returns zero-filled crash counts: 24 hourly buckets when
// hourly is set, else one per day of the range. Bucket times are UTC.
func (s *Store) CrashTimeline(ctx context.Context, r timerange.Range, hourly bool) ([]Point, error) {
	now := s.now().UTC()
	counts := map[time.Time]int64{}
	var slots []time.Time
	if hourly {
		rows, err := s.q.CrashesByHour(ctx, sqlc.CrashesByHourParams{Since: r.Since, Until: r.Until})
		if err != nil {
			return nil, err
		}
		for _, row := range rows {
			counts[row.Hour.UTC()] = row.Count
		}
		slots = r.HourSlots(now)
	} else {
		rows, err := s.q.CrashesByDay(ctx, sqlc.CrashesByDayParams{Since: r.Since, Until: r.Until})
		if err != nil {
			return nil, err
		}
		for _, row := range rows {
			counts[row.Day.UTC()] = row.Count
		}
		slots = r.DaySlots(now)
	}
	out := make([]Point, len(slots))
	for i, t := range slots {
		out[i] = Point{Time: t, Count: counts[t]}
	}
	return out, nil
}

// Volume returns zero-filled fatal + error counts per bucket.
func (s *Store) Volume(ctx context.Context, r timerange.Range, hourly bool) ([]VolumePoint, error) {
	now := s.now().UTC()
	byBucket := map[time.Time]*VolumePoint{}
	add := func(t time.Time, level string, n int64) {
		t = t.UTC()
		p := byBucket[t]
		if p == nil {
			p = &VolumePoint{Time: t}
			byBucket[t] = p
		}
		switch level {
		case "fatal":
			p.Fatal += n
		case "error":
			p.Error += n
		}
	}
	var slots []time.Time
	if hourly {
		rows, err := s.q.VolumeByHour(ctx, sqlc.VolumeByHourParams{Since: r.Since, Until: r.Until})
		if err != nil {
			return nil, err
		}
		for _, row := range rows {
			add(row.Hour, row.Level, row.Count)
		}
		slots = r.HourSlots(now)
	} else {
		rows, err := s.q.VolumeByDay(ctx, sqlc.VolumeByDayParams{Since: r.Since, Until: r.Until})
		if err != nil {
			return nil, err
		}
		for _, row := range rows {
			add(row.Day, row.Level, row.Count)
		}
		slots = r.DaySlots(now)
	}
	out := make([]VolumePoint, len(slots))
	for i, t := range slots {
		out[i] = VolumePoint{Time: t}
		if p := byBucket[t]; p != nil {
			out[i].Fatal, out[i].Error = p.Fatal, p.Error
		}
	}
	return out, nil
}

// ReleaseVersions lists versions active in r (newest first, max 20).
func (s *Store) ReleaseVersions(ctx context.Context, r timerange.Range) ([]string, error) {
	return s.q.ListReleaseVersions(ctx, sqlc.ListReleaseVersionsParams{Since: r.Since, Until: r.Until})
}

// Releases lists the 50 most recently seen releases with counters.
func (s *Store) Releases(ctx context.Context) ([]sqlc.Release, error) {
	return s.q.ListReleases(ctx)
}

// ReleaseHealth is the crash-free summary of one release.
type ReleaseHealth struct {
	Release         string  `json:"release"`
	TotalSessions   int64   `json:"total_sessions"`
	CrashedSessions int64   `json:"crashed_sessions"`
	ErroredSessions int64   `json:"errored_sessions"`
	CrashFreeRate   float64 `json:"crash_free_rate"` // percent, 100 when no sessions
}

// ReleaseHealth aggregates sessions per release over the days in r.
func (s *Store) ReleaseHealth(ctx context.Context, r timerange.Range) ([]ReleaseHealth, error) {
	p := sqlc.ReleaseHealthSummaryParams{SinceDay: startOfDay(r.Since)}
	if r.Until != nil {
		u := startOfDay(*r.Until)
		if u.Equal(*r.Until) {
			// exclusive bound on a day boundary excludes that day entirely
		} else {
			u = u.AddDate(0, 0, 1)
		}
		p.UntilDay = &u
	}
	rows, err := s.q.ReleaseHealthSummary(ctx, p)
	if err != nil {
		return nil, err
	}
	out := make([]ReleaseHealth, 0, len(rows))
	for _, row := range rows {
		h := ReleaseHealth{Release: row.Release, TotalSessions: row.TotalSessions, CrashedSessions: row.CrashedSessions, ErroredSessions: row.ErroredSessions, CrashFreeRate: 100}
		if h.TotalSessions > 0 {
			h.CrashFreeRate = (1 - float64(h.CrashedSessions)/float64(h.TotalSessions)) * 100
		}
		out = append(out, h)
	}
	return out, nil
}

// ── issues ──────────────────────────────────────────────────

// IssueFilter selects issues last seen in the window.
type IssueFilter struct {
	Range       timerange.Range
	ErrorType   string
	Status      string
	Release     string
	UserID      string
	DeviceID    string
	DeviceModel string
	OSVersion   string
	Limit       int
}

// ValidIssueStatuses is the lifecycle vocabulary.
var ValidIssueStatuses = []string{"unresolved", "triaged", "resolved", "ignored", "regression"}

// ListIssues returns issues newest-last-seen first. Event-scoped filters
// (release/user/device/…) resolve to fingerprints with one PK range scan
// over events in the window, then issues are fetched by key.
func (s *Store) ListIssues(ctx context.Context, f IssueFilter) ([]sqlc.Issue, error) {
	p := sqlc.ListIssuesParams{
		Since:        f.Range.Since,
		Until:        f.Range.Until,
		ErrorType:    opt(f.ErrorType),
		Status:       opt(f.Status),
		Fingerprints: []string{},
		PageLimit:    int32(clamp(f.Limit, 1, 100, 50)),
	}
	if f.Release != "" || f.UserID != "" || f.DeviceID != "" || f.DeviceModel != "" || f.OSVersion != "" {
		sinceID, untilID := s.idRange(f.Range, true)
		fps, err := s.q.FingerprintsInRange(ctx, sqlc.FingerprintsInRangeParams{
			SinceID: sinceID, UntilID: untilID,
			Release: opt(f.Release), UserID: opt(f.UserID), DeviceID: opt(f.DeviceID),
			DeviceModel: opt(f.DeviceModel), OsVersion: opt(f.OSVersion),
		})
		if err != nil {
			return nil, err
		}
		p.ByFingerprint = true
		for _, fp := range fps {
			if fp != nil {
				p.Fingerprints = append(p.Fingerprints, *fp)
			}
		}
		if len(p.Fingerprints) == 0 {
			return []sqlc.Issue{}, nil
		}
	}
	return s.q.ListIssues(ctx, p)
}

// GetIssue returns one issue.
func (s *Store) GetIssue(ctx context.Context, fp string) (sqlc.Issue, error) {
	is, err := s.q.GetIssue(ctx, fp)
	if errors.Is(err, pgx.ErrNoRows) {
		return is, ErrNotFound
	}
	return is, err
}

// UpdateIssueStatus sets the lifecycle status.
func (s *Store) UpdateIssueStatus(ctx context.Context, fp, status string) error {
	ok := false
	for _, v := range ValidIssueStatuses {
		ok = ok || v == status
	}
	if !ok {
		return fmt.Errorf("invalid status %q", status)
	}
	n, err := s.q.UpdateIssueStatus(ctx, sqlc.UpdateIssueStatusParams{Fingerprint: fp, Status: status})
	if err != nil {
		return err
	}
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

// ── alerts ──────────────────────────────────────────────────

// ValidAlertTypes are the built-in detectors.
var ValidAlertTypes = []string{"crash_spike", "new_error", "regression"}

// ListAlertTypes returns the detectors with their enabled state.
func (s *Store) ListAlertTypes(ctx context.Context) ([]sqlc.AlertType, error) {
	return s.q.ListAlertTypes(ctx)
}

// SetAlertEnabled toggles a detector.
func (s *Store) SetAlertEnabled(ctx context.Context, typ string, enabled bool) error {
	n, err := s.q.SetAlertEnabled(ctx, sqlc.SetAlertEnabledParams{Type: typ, Enabled: enabled})
	if err != nil {
		return err
	}
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

// Queries exposes the raw sqlc layer for the cron jobs.
func (s *Store) Queries() *sqlc.Queries { return s.q }

// ── helpers ─────────────────────────────────────────────────

func opt(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

func optLike(s string) *string {
	if s == "" {
		return nil
	}
	v := "%" + escapeLike(s) + "%"
	return &v
}

func escapeLike(s string) string {
	r := strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`)
	return r.Replace(s)
}

func nonNil(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}

func clamp(n, lo, hi, def int) int {
	if n == 0 {
		return def
	}
	return min(max(n, lo), hi)
}

func startOfDay(t time.Time) time.Time {
	t = t.UTC()
	return time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, time.UTC)
}
