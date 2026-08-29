package web

import (
	"context"
	"errors"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/a-h/templ"

	"github.com/jackc/pgx/v5"

	"github.com/newlix/crashcart/internal/db/sqlc"
	"github.com/newlix/crashcart/internal/pk"
	"github.com/newlix/crashcart/internal/sentry"
	"github.com/newlix/crashcart/internal/store"
)

// ── portal ──────────────────────────────────────────────────

// PortalCard is one project on the portal.
type PortalCard struct {
	P             sqlc.Project
	Received      []string // platform families seen in the last 7 days
	Mismatch      bool     // some of them are not what the project declares
	Crashes24h    int64
	LatestRelease string
	CrashFree     string
	OpenIssues    int64
}

func (w *Web) portal(rw http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	projects, err := w.Store.ListProjects(ctx)
	if err != nil {
		w.fail(rw, r, err)
		return
	}
	n := now()
	cards := make([]PortalCard, 0, len(projects))
	for _, p := range projects {
		c := PortalCard{P: p, CrashFree: "n/a"}
		if t, err := w.Store.Totals(ctx, sqlc.TotalsParams{ProjectID: p.ID, Bucket: pk.Lower(n.Add(-24 * time.Hour)), Bucket_2: pk.Upper(n)}); err == nil {
			c.Crashes24h = t.Crashes
		}
		c.LatestRelease, c.CrashFree = w.latestHealth(ctx, p.ID, n, 30)
		c.Received, c.Mismatch = w.receivedPlatforms(ctx, p, pk.Lower(n.Add(-7*24*time.Hour)), pk.Upper(n))
		counts, _ := w.Store.CountIssuesByStatus(ctx, p.ID)
		for _, row := range counts {
			if row.Status == "unresolved" || row.Status == "triaged" || row.Status == "regression" {
				c.OpenIssues += row.N
			}
		}
		cards = append(cards, c)
	}
	w.page(rw, r, Page{S: ViewState{Filters: map[string]string{}}, Section: "portal"}, func(Page) templ.Component { return Portal(cards) })
}

// receivedPlatforms lists the platform families that sent events in the
// window (most events first) and whether any is not what the project
// declares. The aggregate only has the raw platform, so the family is
// derived without the SDK name — close enough for a warning.
func (w *Web) receivedPlatforms(ctx context.Context, p sqlc.Project, from, to int64) ([]string, bool) {
	rows, err := w.Store.PlatformTotals(ctx, sqlc.PlatformTotalsParams{ProjectID: p.ID, Bucket: from, Bucket_2: to})
	if err != nil {
		return nil, false
	}
	expected := ""
	if p.Platform != nil {
		expected = *p.Platform
	}
	seen := map[string]bool{}
	var out []string
	mismatch := false
	for _, r := range rows {
		fam := sentry.Family(r.Platform, "")
		if expected == "android" && r.Platform == "java" {
			fam = "android" // the JVM/Android ambiguity resolves in the project's favour
		}
		if !seen[fam] {
			seen[fam] = true
			out = append(out, fam)
		}
		if !sentry.Accepts(expected, fam) {
			mismatch = true
		}
	}
	return out, mismatch
}

// latestHealth finds the release with the most recent activity in the last
// days and its crash-free session rate over the same span.
func (w *Web) latestHealth(ctx context.Context, projectID int64, n time.Time, days int) (release, rate string) {
	from := pk.Lower(n.Add(-time.Duration(days) * 24 * time.Hour))
	rel, err := w.Store.LatestRelease(ctx, sqlc.LatestReleaseParams{ProjectID: projectID, Bucket: from})
	if err != nil || rel == "" {
		return "", "n/a"
	}
	rate = "n/a"
	if health, err := w.Store.ReleaseHealth(ctx, sqlc.ReleaseHealthParams{ProjectID: projectID, Bucket: from, Bucket_2: pk.Upper(n)}); err == nil {
		for _, h := range health {
			if h.Release == rel {
				rate = crashFree(h.Total, h.Crashed)
			}
		}
	}
	return rel, rate
}

// ── overview ────────────────────────────────────────────────

// ChartData is a stacked bar chart ready to render.
type ChartData struct {
	Buckets     []Bucket
	Series      []Series
	First, Last string // axis end labels
	Empty       bool
}

// OverviewData feeds the overview page.
type OverviewData struct {
	Received      []string // platform families seen in the window
	Mismatch      bool
	Today         int64 // events received today (UTC)
	QuotaReached  bool
	LatestRelease string
	CrashFree     string
	Crashes       int64
	NewIssues     int64
	Chart         ChartData
	New           []sqlc.Issue
	Regressions   []sqlc.Issue
}

func (w *Web) overview(rw http.ResponseWriter, r *http.Request) {
	p, ok := w.project(rw, r)
	if !ok {
		return
	}
	ctx := r.Context()
	s := state(r)
	n := now()
	win := s.Window(n)
	var d OverviewData
	d.Received, d.Mismatch = w.receivedPlatforms(ctx, p, win.From, win.To)
	if p.DailyQuota > 0 {
		d.Today, _ = w.Store.EventsSince(ctx, sqlc.EventsSinceParams{ProjectID: p.ID, Bucket: pk.Lower(n.UTC().Truncate(24 * time.Hour))})
		d.QuotaReached = d.Today >= int64(p.DailyQuota)
	}
	d.LatestRelease, d.CrashFree = w.latestHealth(ctx, p.ID, n, win.Days)
	totals, err := w.Store.Totals(ctx, sqlc.TotalsParams{ProjectID: p.ID, Bucket: win.From, Bucket_2: win.To})
	if err != nil {
		w.fail(rw, r, err)
		return
	}
	d.Crashes = totals.Crashes
	if d.NewIssues, err = w.Store.CountNewIssues(ctx, sqlc.CountNewIssuesParams{ProjectID: p.ID, FirstSeen: win.From}); err != nil {
		w.fail(rw, r, err)
		return
	}
	rows, err := w.Store.Timeline(ctx, sqlc.TimelineParams{ProjectID: p.ID, Bucket: win.From, Bucket_2: win.To})
	if err != nil {
		w.fail(rw, r, err)
		return
	}
	d.Chart = crashChart(rows, win)
	if d.New, err = w.Store.ListNewIssues(ctx, sqlc.ListNewIssuesParams{ProjectID: p.ID, FirstSeen: pk.Lower(n.Add(-24 * time.Hour)), Limit: 5}); err != nil {
		w.fail(rw, r, err)
		return
	}
	if d.Regressions, err = w.Store.ListRegressions(ctx, sqlc.ListRegressionsParams{ProjectID: p.ID, Limit: 5}); err != nil {
		w.fail(rw, r, err)
		return
	}
	pg := Page{S: s, Project: &p, Section: "overview"}
	pg.Stream, pg.Regressions = w.streamInfo(ctx, p.ID, s, n)
	w.page(rw, r, pg, func(pg Page) templ.Component { return Overview(pg, d) })
}

// streamInfo builds the SSE URL (since = now) and the regression baseline.
func (w *Web) streamInfo(ctx context.Context, projectID int64, s ViewState, n time.Time) (string, int64) {
	reg, _ := w.Store.CountRegressions(ctx, sqlc.CountRegressionsParams{ProjectID: projectID})
	return s.Base() + "/stream?since=" + strconv.FormatInt(pk.Upper(n), 10), reg
}

// seriesColors is the palette for stacked-by-release charts.
var seriesTokens = []string{"series-1", "series-2", "series-3", "series-4", "series-5"}

// crashChart stacks crashes per bucket by release: top 5 releases + other.
func crashChart(rows []sqlc.TimelineRow, win Window) ChartData {
	totals := map[string]int64{}
	perBucket := map[int64]map[string]int64{}
	var any bool
	for _, r := range rows {
		if r.Crashes == 0 {
			continue
		}
		any = true
		rel := r.Release
		if rel == "" {
			rel = "(no release)"
		}
		totals[rel] += r.Crashes
		b := pk.Bucket(r.Bucket, win.Width)
		if perBucket[b] == nil {
			perBucket[b] = map[string]int64{}
		}
		perBucket[b][rel] += r.Crashes
	}
	names := make([]string, 0, len(totals))
	for n := range totals {
		names = append(names, n)
	}
	sort.Slice(names, func(i, j int) bool {
		if totals[names[i]] != totals[names[j]] {
			return totals[names[i]] > totals[names[j]]
		}
		return names[i] < names[j]
	})
	top := names
	other := false
	if len(names) > 5 {
		top, other = names[:5], true
	}
	var series []Series
	for i, n := range top {
		series = append(series, Series{Name: n, Token: seriesTokens[i]})
	}
	if other {
		series = append(series, Series{Name: "other", Token: "series-other"})
	}
	if len(series) == 0 {
		series = []Series{{Name: "crashes", Token: "level-fatal"}}
	}
	idx := map[string]int{}
	for i, n := range top {
		idx[n] = i
	}
	c := ChartData{Series: series, Empty: !any}
	for _, b := range win.Buckets() {
		bk := Bucket{Label: win.Label(b), Values: make([]int64, len(series))}
		for rel, v := range perBucket[b] {
			if i, ok := idx[rel]; ok {
				bk.Values[i] += v
			} else if other {
				bk.Values[len(series)-1] += v
			}
		}
		c.Buckets = append(c.Buckets, bk)
	}
	if len(c.Buckets) > 0 {
		c.First, c.Last = c.Buckets[0].Label, c.Buckets[len(c.Buckets)-1].Label
	}
	return c
}

// singleChart folds (bucket, value) rows into one series over the window.
func singleChart(name, token string, points map[int64]int64, win Window) ChartData {
	c := ChartData{Series: []Series{{Name: name, Token: token}}, Empty: true}
	for _, b := range win.Buckets() {
		v := points[b]
		if v > 0 {
			c.Empty = false
		}
		c.Buckets = append(c.Buckets, Bucket{Label: win.Label(b), Values: []int64{v}})
	}
	if len(c.Buckets) > 0 {
		c.First, c.Last = c.Buckets[0].Label, c.Buckets[len(c.Buckets)-1].Label
	}
	return c
}

// ── issues ──────────────────────────────────────────────────

// IssuesData feeds the issue list.
type IssuesData struct {
	Issues   []sqlc.Issue
	Total    int64
	Counts   map[string]int64  // per status
	Users    map[string]int64  // per fingerprint, in window
	Sparks   map[string]string // per fingerprint: SVG
	Releases []string
}

const issuesPage = 50

func (w *Web) issues(rw http.ResponseWriter, r *http.Request) {
	p, ok := w.project(rw, r)
	if !ok {
		return
	}
	s := state(r)
	d, err := w.loadIssues(r.Context(), p, s)
	if err != nil {
		w.fail(rw, r, err)
		return
	}
	pg := Page{S: s, Project: &p, Section: "issues"}
	pg.Stream, pg.Regressions = w.streamInfo(r.Context(), p.ID, s, now())
	w.page(rw, r, pg, func(pg Page) templ.Component { return Issues(pg, d) })
}

func (w *Web) loadIssues(ctx context.Context, p sqlc.Project, s ViewState) (IssuesData, error) {
	n := now()
	win := s.Window(n)
	status := s.Status
	if status == "all" {
		status = ""
	}
	f := store.IssueFilter{ProjectID: p.ID, Status: status, Level: s.Filters["level"], Release: s.Filters["release"],
		Query: s.Filters["q"], Sort: s.Sort, From: win.From, To: win.To, Limit: issuesPage, Offset: s.Offset}
	issues, total, err := w.Store.ListIssues(ctx, f)
	if err != nil {
		return IssuesData{}, err
	}
	d := IssuesData{Issues: issues, Total: total, Counts: map[string]int64{}, Users: map[string]int64{}, Sparks: map[string]string{}}
	counts, err := w.Store.CountIssuesByStatus(ctx, p.ID)
	if err != nil {
		return d, err
	}
	for _, c := range counts {
		d.Counts[c.Status] = c.N
		d.Counts["all"] += c.N
	}
	fps := make([]string, 0, len(issues))
	for _, is := range issues {
		fps = append(fps, is.Fingerprint)
	}
	if len(fps) > 0 {
		users, err := w.Store.IssueUsers(ctx, sqlc.IssueUsersParams{ProjectID: p.ID, Column2: fps, ID: win.From, ID_2: win.To})
		if err != nil {
			return d, err
		}
		for _, u := range users {
			if u.Fingerprint != nil {
				d.Users[*u.Fingerprint] = u.Users
			}
		}
		sparks, err := w.sparklines(ctx, p.ID, fps, n)
		if err != nil {
			return d, err
		}
		d.Sparks = sparks
	}
	d.Releases, _ = w.Store.DistinctReleases(ctx, sqlc.DistinctReleasesParams{ProjectID: p.ID, Bucket: pk.Lower(n.Add(-90 * 24 * time.Hour))})
	return d, nil
}

// sparklines renders 7 days of hourly counts (folded to 4 h points) per issue.
func (w *Web) sparklines(ctx context.Context, projectID int64, fps []string, n time.Time) (map[string]string, error) {
	const width = 4 * pk.Hour
	since := pk.Bucket(pk.Lower(n.Add(-7*24*time.Hour)), width)
	rows, err := w.Store.IssueSparklines(ctx, sqlc.IssueSparklinesParams{ProjectID: projectID, Column2: fps, Bucket: since})
	if err != nil {
		return nil, err
	}
	points := 7 * 24 / 4
	per := map[string][]int64{}
	for _, fp := range fps {
		per[fp] = make([]int64, points)
	}
	for _, r := range rows {
		i := int((r.Bucket - since) / width)
		if i >= 0 && i < points {
			per[r.Fingerprint][i] += r.Events
		}
	}
	out := make(map[string]string, len(fps))
	for fp, v := range per {
		out[fp] = sparkline(v)
	}
	return out, nil
}

var bulkStatuses = map[string]bool{"resolved": true, "ignored": true, "triaged": true, "unresolved": true}

// issuesBulk sets the status of the selected issues and answers with the
// refreshed table fragment (the POST URL carries the list state).
func (w *Web) issuesBulk(rw http.ResponseWriter, r *http.Request) {
	p, ok := w.project(rw, r)
	if !ok {
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(rw, "bad form", http.StatusBadRequest)
		return
	}
	status := r.PostForm.Get("status")
	fps := r.PostForm["fp"]
	if !bulkStatuses[status] || len(fps) == 0 {
		http.Error(rw, "status and fp[] required", http.StatusBadRequest)
		return
	}
	if _, err := w.Store.SetIssuesStatus(r.Context(), sqlc.SetIssuesStatusParams{ProjectID: p.ID, Column2: fps, Status: status}); err != nil {
		w.fail(rw, r, err)
		return
	}
	s := state(r)
	d, err := w.loadIssues(r.Context(), p, s)
	if err != nil {
		w.fail(rw, r, err)
		return
	}
	pg := Page{S: s, Project: &p, Section: "issues", Tags: w.Cfg.CustomTags}
	w.render(rw, r, IssuesTable(pg, d))
}

// ── issue ───────────────────────────────────────────────────

// BreakdownList is one column's top values for an issue.
type BreakdownList struct {
	Column, Label string
	Items         []store.Breakdown
	Total         int64
}

// IssueData feeds the issue page.
type IssueData struct {
	Issue      sqlc.Issue
	Latest     *sqlc.Event
	Stacks     []Stack
	Users      int64
	Breakdowns []BreakdownList
	Chart      ChartData
	Events     []store.EventRow
	More       bool
	LatestID   int64
	OldestID   int64
}

var breakdownColumns = [][2]string{{"release", "Release"}, {"os_version", "OS version"}, {"device_model", "Device"}, {"environment", "Environment"}}

func (w *Web) issue(rw http.ResponseWriter, r *http.Request) {
	p, ok := w.project(rw, r)
	if !ok {
		return
	}
	ctx := r.Context()
	fp := r.PathValue("fingerprint")
	is, err := w.Store.GetIssue(ctx, sqlc.GetIssueParams{ProjectID: p.ID, Fingerprint: fp})
	if errors.Is(err, pgx.ErrNoRows) {
		http.NotFound(rw, r)
		return
	}
	if err != nil {
		w.fail(rw, r, err)
		return
	}
	s := state(r)
	n := now()
	win := s.Window(n)
	d := IssueData{Issue: is}
	if ev, err := w.Store.LatestIssueEvent(ctx, sqlc.LatestIssueEventParams{ProjectID: p.ID, Fingerprint: &fp}); err == nil {
		d.Latest = &ev
		d.Stacks = stacksOf(ev)
	} else if !errors.Is(err, pgx.ErrNoRows) {
		w.fail(rw, r, err)
		return
	}
	if nb, err := w.Store.IssueNeighbors(ctx, sqlc.IssueNeighborsParams{ProjectID: p.ID, Fingerprint: &fp}); err == nil {
		d.LatestID, d.OldestID = nb.Latest, nb.Oldest
	}
	if users, err := w.Store.IssueUsers(ctx, sqlc.IssueUsersParams{ProjectID: p.ID, Column2: []string{fp}, ID: win.From, ID_2: win.To}); err == nil && len(users) > 0 {
		d.Users = users[0].Users
	}
	bf := store.EventFilter{ProjectID: p.ID, Fingerprint: fp, From: win.From, To: win.To}
	for _, c := range breakdownColumns {
		items, err := w.Store.Breakdown(ctx, bf, c[0], 5)
		if err != nil {
			w.fail(rw, r, err)
			return
		}
		bl := BreakdownList{Column: c[0], Label: c[1], Items: items}
		for _, it := range items {
			bl.Total += it.Count
		}
		d.Breakdowns = append(d.Breakdowns, bl)
	}
	rows, err := w.Store.IssueTimeline(ctx, sqlc.IssueTimelineParams{ProjectID: p.ID, Fingerprint: fp, Bucket: win.From, Bucket_2: win.To})
	if err != nil {
		w.fail(rw, r, err)
		return
	}
	points := map[int64]int64{}
	for _, r := range rows {
		points[pk.Bucket(r.Bucket, win.Width)] += r.Events
	}
	token := "level-error"
	if is.Level == "fatal" {
		token = "level-fatal"
	}
	d.Chart = singleChart("events", token, points, win)
	d.Events, d.More, err = w.Store.ListEvents(ctx, store.EventFilter{ProjectID: p.ID, Fingerprint: fp, Before: s.Before, Limit: 25})
	if err != nil {
		w.fail(rw, r, err)
		return
	}
	w.page(rw, r, Page{S: s, Project: &p, Section: "issues"}, func(pg Page) templ.Component { return IssuePage(pg, d) })
}

var issueStatuses = map[string]bool{"unresolved": true, "triaged": true, "resolved": true, "ignored": true}

func (w *Web) issueStatus(rw http.ResponseWriter, r *http.Request) {
	p, ok := w.project(rw, r)
	if !ok {
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(rw, "bad form", http.StatusBadRequest)
		return
	}
	status := r.Form.Get("status")
	if !issueStatuses[status] {
		http.Error(rw, "bad status", http.StatusBadRequest)
		return
	}
	fp := r.PathValue("fingerprint")
	if _, err := w.Store.SetIssueStatus(r.Context(), sqlc.SetIssueStatusParams{ProjectID: p.ID, Fingerprint: fp, Status: status}); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			http.NotFound(rw, r)
			return
		}
		w.fail(rw, r, err)
		return
	}
	redirect(rw, r, state(r).Href("/issues/"+fp))
}

// ── events ──────────────────────────────────────────────────

// EventsData feeds the event list.
type EventsData struct {
	Events   []store.EventRow
	More     bool
	Releases []string
}

const eventsPage = 50

// eventFilter maps the URL state onto the store filter.
func eventFilter(p sqlc.Project, s ViewState, win Window) store.EventFilter {
	f := store.EventFilter{ProjectID: p.ID, From: win.From, To: win.To, Before: s.Before, Limit: eventsPage,
		Level: s.Filters["level"], Release: s.Filters["release"], Environment: s.Filters["environment"], Platform: s.Filters["platform"],
		ErrorType: s.Filters["error_type"], UserID: s.Filters["user_id"], DeviceID: s.Filters["device_id"], DeviceModel: s.Filters["device_model"],
		OSVersion: s.Filters["os_version"], Screen: s.Filters["screen"], Fingerprint: s.Filters["fingerprint"], Location: s.Filters["error_location"],
		Query: s.Filters["q"], Crash: s.Filters["crash"] == "1"}
	for k, v := range s.Filters {
		if strings.HasPrefix(k, "tag.") {
			if f.Tags == nil {
				f.Tags = map[string]string{}
			}
			f.Tags[strings.TrimPrefix(k, "tag.")] = v
		}
	}
	return f
}

func (w *Web) events(rw http.ResponseWriter, r *http.Request) {
	p, ok := w.project(rw, r)
	if !ok {
		return
	}
	ctx := r.Context()
	s := state(r)
	n := now()
	win := s.Window(n)
	var d EventsData
	var err error
	if d.Events, d.More, err = w.Store.ListEvents(ctx, eventFilter(p, s, win)); err != nil {
		w.fail(rw, r, err)
		return
	}
	d.Releases, _ = w.Store.DistinctReleases(ctx, sqlc.DistinctReleasesParams{ProjectID: p.ID, Bucket: win.From})
	pg := Page{S: s, Project: &p, Section: "events"}
	w.page(rw, r, pg, func(pg Page) templ.Component { return Events(pg, d) })
}

// EventData feeds the event page.
type EventData struct {
	E        sqlc.Event
	Issue    *sqlc.Issue
	Stacks   []Stack
	Crumbs   []sentry.Breadcrumb
	Contexts []ContextGroup
	User     []KV
	Tags     map[string]string
}

func (w *Web) event(rw http.ResponseWriter, r *http.Request) {
	p, ok := w.project(rw, r)
	if !ok {
		return
	}
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		http.NotFound(rw, r)
		return
	}
	ctx := r.Context()
	e, err := w.Store.GetEvent(ctx, sqlc.GetEventParams{ProjectID: p.ID, ID: id})
	if errors.Is(err, pgx.ErrNoRows) {
		http.NotFound(rw, r)
		return
	}
	if err != nil {
		w.fail(rw, r, err)
		return
	}
	d := EventData{E: e, Stacks: stacksOf(e), Crumbs: crumbsOf(e.Breadcrumbs), Tags: tagsMap(e.Tags)}
	d.Contexts, d.User = payloadContexts(e.Payload)
	if e.Fingerprint != nil {
		if is, err := w.Store.GetIssue(ctx, sqlc.GetIssueParams{ProjectID: p.ID, Fingerprint: *e.Fingerprint}); err == nil {
			d.Issue = &is
		}
	}
	pg := Page{S: state(r), Project: &p, Section: "events", Tags: w.Cfg.CustomTags}
	if isHx(r) {
		w.render(rw, r, EventBody(pg, d))
		return
	}
	w.page(rw, r, pg, func(pg Page) templ.Component { return EventPage(pg, d) })
}

// ── releases ────────────────────────────────────────────────

// ReleaseRow is one line of the releases table.
type ReleaseRow struct {
	Release, Platform   string
	Sessions, Crashed   int64
	TotalSessions       int64 // all releases in the window (adoption denominator)
	Crashes, Events     int64
	NewIssues           int64
	FirstSeen, LastSeen int64
}

func (r ReleaseRow) Adoption() string  { return percent(r.Sessions, r.TotalSessions) }
func (r ReleaseRow) CrashFree() string { return crashFree(r.Sessions, r.Crashed) }

func (w *Web) releaseRows(ctx context.Context, p sqlc.Project, win Window) ([]ReleaseRow, error) {
	stats, err := w.Store.ReleaseStats(ctx, sqlc.ReleaseStatsParams{ProjectID: p.ID, Bucket: win.From, Bucket_2: win.To})
	if err != nil {
		return nil, err
	}
	health, err := w.Store.ReleaseHealth(ctx, sqlc.ReleaseHealthParams{ProjectID: p.ID, Bucket: win.From, Bucket_2: win.To})
	if err != nil {
		return nil, err
	}
	introduced, err := w.Store.IssuesIntroducedPerRelease(ctx, p.ID)
	if err != nil {
		return nil, err
	}
	hm := map[string]sqlc.ReleaseHealthRow{}
	var total int64
	for _, h := range health {
		hm[h.Release] = h
		total += h.Total
	}
	im := map[string]int64{}
	for _, i := range introduced {
		if i.Release != nil {
			im[*i.Release] = i.N
		}
	}
	// ReleaseStats is per (release, platform); fold platforms per release.
	var rows []ReleaseRow
	idx := map[string]int{}
	for _, st := range stats {
		i, ok := idx[st.Release]
		if !ok {
			i = len(rows)
			idx[st.Release] = i
			h := hm[st.Release]
			rows = append(rows, ReleaseRow{Release: st.Release, Platform: st.Platform, Sessions: h.Total, Crashed: h.Crashed,
				TotalSessions: total, NewIssues: im[st.Release], FirstSeen: st.FirstSeen, LastSeen: st.LastSeen})
		}
		r := &rows[i]
		r.Crashes += st.Crashes
		r.Events += st.Events
		if st.Platform != "" && !strings.Contains(r.Platform, st.Platform) {
			r.Platform = strings.TrimPrefix(r.Platform+", "+st.Platform, ", ")
		}
		if st.FirstSeen < r.FirstSeen {
			r.FirstSeen = st.FirstSeen
		}
		if st.LastSeen > r.LastSeen {
			r.LastSeen = st.LastSeen
		}
	}
	// releases with sessions but no events still count
	for rel, h := range hm {
		if _, ok := idx[rel]; !ok {
			rows = append(rows, ReleaseRow{Release: rel, Sessions: h.Total, Crashed: h.Crashed, TotalSessions: total, NewIssues: im[rel]})
		}
	}
	sort.SliceStable(rows, func(i, j int) bool { return rows[i].LastSeen > rows[j].LastSeen })
	return rows, nil
}

func (w *Web) releases(rw http.ResponseWriter, r *http.Request) {
	p, ok := w.project(rw, r)
	if !ok {
		return
	}
	s := state(r)
	rows, err := w.releaseRows(r.Context(), p, s.Window(now()))
	if err != nil {
		w.fail(rw, r, err)
		return
	}
	pg := Page{S: s, Project: &p, Section: "releases"}
	w.page(rw, r, pg, func(pg Page) templ.Component { return Releases(pg, rows) })
}

// ReleaseData feeds the release page.
type ReleaseData struct {
	Row        ReleaseRow
	Health     []HealthPoint
	Chart      ChartData
	Introduced []sqlc.Issue
	Present    []sqlc.Issue
}

func (w *Web) release(rw http.ResponseWriter, r *http.Request) {
	p, ok := w.project(rw, r)
	if !ok {
		return
	}
	ctx := r.Context()
	s := state(r)
	win := s.Window(now())
	version := r.PathValue("version")
	rows, err := w.releaseRows(ctx, p, win)
	if err != nil {
		w.fail(rw, r, err)
		return
	}
	d := ReleaseData{Row: ReleaseRow{Release: version}}
	for _, row := range rows {
		if row.Release == version {
			d.Row = row
		}
	}
	dayFrom := pk.Bucket(win.From, pk.Day)
	daily, err := w.Store.ReleaseHealthDaily(ctx, sqlc.ReleaseHealthDailyParams{ProjectID: p.ID, Release: version, Bucket: dayFrom, Bucket_2: win.To})
	if err != nil {
		w.fail(rw, r, err)
		return
	}
	dm := map[int64]sqlc.ReleaseHealthDailyRow{}
	for _, row := range daily {
		dm[row.Bucket] = row
	}
	for b := dayFrom; b < win.To; b += pk.Day {
		row := dm[b]
		d.Health = append(d.Health, HealthPoint{Label: pk.Time(b).Format("Jan 2"), Total: row.Total, Crashed: row.Crashed})
	}
	tl, err := w.Store.ReleaseTimeline(ctx, sqlc.ReleaseTimelineParams{ProjectID: p.ID, Release: version, Bucket: win.From, Bucket_2: win.To})
	if err != nil {
		w.fail(rw, r, err)
		return
	}
	points := map[int64]int64{}
	for _, row := range tl {
		points[pk.Bucket(row.Bucket, win.Width)] += row.Crashes
	}
	d.Chart = singleChart("crashes", "level-fatal", points, win)
	if d.Introduced, err = w.Store.ListIssuesIntroducedIn(ctx, sqlc.ListIssuesIntroducedInParams{ProjectID: p.ID, FirstRelease: &version, Limit: 20}); err != nil {
		w.fail(rw, r, err)
		return
	}
	if d.Present, err = w.Store.ListIssuesPresentIn(ctx, sqlc.ListIssuesPresentInParams{ProjectID: p.ID, LastRelease: &version, Limit: 20}); err != nil {
		w.fail(rw, r, err)
		return
	}
	pg := Page{S: s, Project: &p, Section: "releases"}
	w.page(rw, r, pg, func(pg Page) templ.Component { return ReleasePage(pg, d) })
}
