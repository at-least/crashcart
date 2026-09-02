package web

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/a-h/templ"

	"github.com/jackc/pgx/v5"

	"github.com/at-least/crashcart/internal/api"
	"github.com/at-least/crashcart/internal/db/sqlc"
	"github.com/at-least/crashcart/internal/sentry"
	"github.com/at-least/crashcart/internal/store"
)

// ── portal ──────────────────────────────────────────────────

// PortalCard is one project on the portal.
type PortalCard struct {
	P             sqlc.Project
	Received      []string // platform families seen in the last 7 days
	Mismatch      bool     // some of them are not what the project declares
	Unhandled24h  int64
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
	// One query per statistic, one row per project: unhandled in the last
	// 24 h, platforms seen in 7 days, the latest active release (30 days)
	// and its session health, open issues.
	dayFrom := n.Add(-30 * day).Truncate(day)
	unhandled, err := w.Store.PortalUnhandled(ctx, sqlc.PortalUnhandledParams{FromAt: n.Add(-day).Truncate(time.Hour), ToAt: n})
	if err != nil {
		w.fail(rw, r, err)
		return
	}
	platforms, err := w.Store.PortalPlatforms(ctx, sqlc.PortalPlatformsParams{FromAt: n.Add(-7 * day).Truncate(time.Hour), ToAt: n})
	if err != nil {
		w.fail(rw, r, err)
		return
	}
	latest, err := w.Store.PortalLatestReleases(ctx, sqlc.PortalLatestReleasesParams{FromAt: dayFrom, ToAt: n})
	if err != nil {
		w.fail(rw, r, err)
		return
	}
	open, err := w.Store.PortalOpenIssues(ctx)
	if err != nil {
		w.fail(rw, r, err)
		return
	}
	crashes24h := map[int64]int64{}
	for _, c := range unhandled {
		crashes24h[c.ProjectID] = c.Unhandled
	}
	byPlatform := map[int64][]platformCount{}
	for _, pc := range platforms {
		byPlatform[pc.ProjectID] = append(byPlatform[pc.ProjectID], platformCount{pc.Platform, pc.Events})
	}
	newest := map[int64]string{}
	hp := sqlc.PortalReleaseHealthParams{FromAt: dayFrom, ToAt: n}
	for _, l := range latest {
		newest[l.ProjectID] = l.Release
		hp.ProjectIds = append(hp.ProjectIds, l.ProjectID)
		hp.Releases = append(hp.Releases, l.Release)
	}
	health := map[int64]sqlc.PortalReleaseHealthRow{}
	if len(latest) > 0 {
		rows, err := w.Store.PortalReleaseHealth(ctx, hp)
		if err != nil {
			w.fail(rw, r, err)
			return
		}
		for _, r := range rows {
			health[r.ProjectID] = r
		}
	}
	openIssues := map[int64]int64{}
	for _, o := range open {
		openIssues[o.ProjectID] = o.N
	}
	cards := make([]PortalCard, 0, len(projects))
	for _, p := range projects {
		c := PortalCard{P: p, CrashFree: "n/a", Unhandled24h: crashes24h[p.ID], OpenIssues: openIssues[p.ID], LatestRelease: newest[p.ID]}
		c.Received, c.Mismatch = platformFamilies(p, byPlatform[p.ID])
		if hr, ok := health[p.ID]; ok {
			c.CrashFree = crashFree(hr.Total, hr.Crashed)
		}
		cards = append(cards, c)
	}
	w.page(rw, r, Page{S: ViewState{Filters: map[string]string{}}, Section: "portal"}, func(Page) templ.Component { return Portal(cards) })
}

// platformCount is a raw SDK platform with its events in a window.
type platformCount struct {
	Platform string
	Events   int64
}

// receivedPlatforms lists the platform families that sent events in the
// window (most events first) and whether any is not what the project
// declares. The aggregate only has the raw platform, so the family is
// derived without the SDK name — close enough for a warning.
func (w *Web) receivedPlatforms(ctx context.Context, p sqlc.Project, win Window) ([]string, bool) {
	rows, err := w.Store.PlatformTotals(ctx, sqlc.PlatformTotalsParams{ProjectID: p.ID, FromAt: win.From, ToAt: win.To})
	if err != nil {
		return nil, false
	}
	pc := make([]platformCount, 0, len(rows))
	for _, r := range rows {
		pc = append(pc, platformCount{r.Platform, r.Events})
	}
	return platformFamilies(p, pc)
}

// platformFamilies folds raw platforms (most events first) into families
// and reports whether any is not what the project declares.
func platformFamilies(p sqlc.Project, rows []platformCount) ([]string, bool) {
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
	win := Window{From: n.Add(-time.Duration(days) * day).Truncate(day), To: n, Width: day}
	lr, err := w.Store.LatestReleaseHealth(ctx, sqlc.LatestReleaseHealthParams{ProjectID: projectID, HourFrom: win.From, DayFrom: win.From, ToAt: n})
	if err != nil {
		return "", "n/a"
	}
	return lr.Release, crashFree(lr.Total, lr.Crashed)
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
	Unhandled     int64
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
	d.Received, d.Mismatch = w.receivedPlatforms(ctx, p, win)
	if p.DailyQuota > 0 {
		d.Today, _ = w.Store.ProjectUsage(ctx, sqlc.ProjectUsageParams{ProjectID: p.ID, Day: n.UTC().Truncate(day)})
		d.QuotaReached = d.Today >= int64(p.DailyQuota)
	}
	d.LatestRelease, d.CrashFree = w.latestHealth(ctx, p.ID, n, win.Days)
	totals, err := w.Store.Totals(ctx, sqlc.TotalsParams{ProjectID: p.ID, FromAt: win.From, ToAt: win.To})
	if err != nil {
		w.fail(rw, r, err)
		return
	}
	d.Unhandled = totals.Unhandled
	if d.NewIssues, err = w.Store.CountNewIssues(ctx, sqlc.CountNewIssuesParams{ProjectID: p.ID, FirstSeen: win.From}); err != nil {
		w.fail(rw, r, err)
		return
	}
	rows, err := w.Store.Timeline(ctx, sqlc.TimelineParams{ProjectID: p.ID, FromAt: win.From, ToAt: win.To, Width: win.Seconds(), Top: 5})
	if err != nil {
		w.fail(rw, r, err)
		return
	}
	d.Chart = unhandledChart(rows, win)
	if d.New, err = w.Store.ListNewIssues(ctx, sqlc.ListNewIssuesParams{ProjectID: p.ID, FirstSeen: n.Add(-day), Limit: 5}); err != nil {
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
	win := s.Window(n)
	reg, _ := w.Store.CountRegressions(ctx, sqlc.CountRegressionsParams{ProjectID: projectID, LastSeen: win.From})
	// The stream re-counts with the same window, so the banner's delta is
	// against this baseline.
	return s.Base() + "/stream?since=" + url.QueryEscape(n.UTC().Format(time.RFC3339Nano)) + "&win=" + url.QueryEscape(s.Win), reg
}

// seriesColors is the palette for stacked-by-release charts.
var seriesTokens = []string{"series-1", "series-2", "series-3", "series-4", "series-5"}

// unhandledChart stacks unhandled per bucket by release. rows come from
// Timeline: gap-filled, ordered by bucket then series rank (top 5
// releases, then "other"); series without a crash in the window are
// dropped here.
func unhandledChart(rows []sqlc.TimelineRow, win Window) ChartData {
	totals := map[string]int64{}
	var order []string
	for _, r := range rows {
		if _, seen := totals[r.Release]; !seen {
			order = append(order, r.Release)
		}
		totals[r.Release] += r.Unhandled
	}
	var series []Series
	idx := map[string]int{}
	for _, rel := range order {
		if totals[rel] == 0 {
			continue
		}
		s := Series{Name: rel, Token: seriesTokens[len(series)%len(seriesTokens)]}
		switch rel {
		case "other":
			s.Token = "series-other"
		case "":
			s.Name = "(no release)"
		}
		idx[rel] = len(series)
		series = append(series, s)
	}
	c := ChartData{Series: series, Empty: len(series) == 0}
	if c.Empty {
		c.Series = []Series{{Name: "unhandled", Token: "level-fatal"}}
	}
	perBucket := map[int64][]int64{}
	for _, r := range rows {
		i, ok := idx[r.Release]
		if !ok {
			continue
		}
		b := r.Bucket.Unix()
		if perBucket[b] == nil {
			perBucket[b] = make([]int64, len(c.Series))
		}
		perBucket[b][i] += r.Unhandled
	}
	for _, b := range win.Buckets() {
		vals := perBucket[b.Unix()]
		if vals == nil {
			vals = make([]int64, len(c.Series))
		}
		c.Buckets = append(c.Buckets, Bucket{Label: win.Label(b), Values: vals})
	}
	if len(c.Buckets) > 0 {
		c.First, c.Last = c.Buckets[0].Label, c.Buckets[len(c.Buckets)-1].Label
	}
	return c
}

// singleChart folds (bucket start unix seconds → value) into one series
// over the window.
func singleChart(name, token string, points map[int64]int64, win Window) ChartData {
	c := ChartData{Series: []Series{{Name: name, Token: token}}, Empty: true}
	for _, b := range win.Buckets() {
		v := points[b.Unix()]
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
	Counts   map[string]int64     // per status
	Sparks   map[sentry.ID]string // per fingerprint: SVG
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
	d := IssuesData{Issues: issues, Total: total, Counts: map[string]int64{}, Sparks: map[sentry.ID]string{}}
	counts, err := w.Store.CountIssuesByStatus(ctx, p.ID)
	if err != nil {
		return d, err
	}
	for _, c := range counts {
		d.Counts[string(c.Status)] = c.N
		d.Counts["all"] += c.N
	}
	fps := make([]sentry.ID, 0, len(issues))
	for _, is := range issues {
		fps = append(fps, is.Fingerprint)
	}
	// No per-issue user count here: count(DISTINCT user_id) over every
	// event of 50 issues in the window is unbounded work for the default
	// page; the issue page computes it for one issue.
	if len(fps) > 0 {
		sparks, err := w.sparklines(ctx, p.ID, fps, n)
		if err != nil {
			return d, err
		}
		d.Sparks = sparks
	}
	d.Releases = w.releaseNames(ctx, p.ID)
	return d, nil
}

// sparklines renders 7 days of hourly counts (folded to 4 h points) per issue.
func (w *Web) sparklines(ctx context.Context, projectID int64, fps []sentry.ID, n time.Time) (map[sentry.ID]string, error) {
	const width = 4 * time.Hour
	since := n.UTC().Add(-7 * day).Truncate(width)
	rows, err := w.Store.IssueSparklines(ctx, sqlc.IssueSparklinesParams{
		ProjectID: projectID, Fingerprints: fps, FromAt: since, ToAt: since.Add(7 * day), Width: int64(width / time.Second),
	})
	if err != nil {
		return nil, err
	}
	out := make(map[sentry.ID]string, len(fps))
	for _, r := range rows {
		out[r.Fingerprint] = sparkline(r.Counts)
	}
	return out, nil
}

// statusForm reads a status change from the form: `status` (unresolved,
// resolved, or an ignoreOptions value), with a separate `ignore` field
// (the bulk bar's select: escalating, 7d, 30d, 100, forever) folded in
// for status=ignored. "regression" is ingest's verdict, never a form's.
func statusForm(form url.Values, at time.Time) (status string, ig ignore, ok bool) {
	v := form.Get("status")
	if cond := form.Get("ignore"); v == "ignored" && cond != "" {
		v = "ignored:" + cond
	}
	return parseStatus(v, at)
}

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
	status, ig, okStatus := statusForm(r.PostForm, now())
	var fps []sentry.ID
	for _, s := range r.PostForm["fp"] {
		if fp, ok := sentry.ParseID(s); ok {
			fps = append(fps, fp)
		}
	}
	if !okStatus || len(fps) == 0 {
		http.Error(rw, "status and fp[] required", http.StatusBadRequest)
		return
	}
	if _, err := w.Store.SetIssuesStatus(r.Context(), sqlc.SetIssuesStatusParams{ProjectID: p.ID, Column2: fps, Status: sqlc.IssueStatus(status), StatusBy: actorName(r),
		IgnoreUntil: ig.Until, IgnoreEvents: ig.Events, IgnoreEscalating: ig.Escalating}); err != nil {
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
	Images     []sqlc.ListAttachmentsRow // the latest event's image attachments (a crash screenshot)
	Stacks     []Stack
	Users      int64
	Breakdowns []BreakdownList
	Chart      ChartData
	Events     []store.EventRow
	More       bool
	LatestID   string // event_id of the newest / oldest stored event ("" when none)
	OldestID   string
}

var breakdownColumns = [][2]string{{"release", "Release"}, {"os_version", "OS version"}, {"device_model", "Device"}, {"environment", "Environment"}}

func (w *Web) issue(rw http.ResponseWriter, r *http.Request) {
	p, ok := w.project(rw, r)
	if !ok {
		return
	}
	ctx := r.Context()
	fp, ok := sentry.ParseID(r.PathValue("fingerprint"))
	if !ok {
		http.NotFound(rw, r)
		return
	}
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
	// The issue row is the exact time range of its events: lookups stay
	// within it (one or two partitions, not all of them).
	seenFrom, seenTo := is.FirstSeen, is.LastSeen.Add(time.Second)
	if ev, err := w.Store.LatestIssueEvent(ctx, sqlc.LatestIssueEventParams{ProjectID: p.ID, Fingerprint: &fp, FromAt: seenFrom, ToAt: seenTo}); err == nil {
		d.Latest = &ev
		d.LatestID = string(ev.EventID)
		d.Stacks = stacksOf(ev, parsePayload(ev, w.payload(ev)))
		if atts, err := w.Store.ListAttachments(ctx, sqlc.ListAttachmentsParams{ProjectID: p.ID, EventID: ev.EventID, OccurredAt: ev.OccurredAt}); err == nil {
			for _, a := range atts {
				if isImage(a.ContentType) {
					d.Images = append(d.Images, a)
				}
			}
		}
	} else if !errors.Is(err, pgx.ErrNoRows) {
		w.fail(rw, r, err)
		return
	}
	if nb, err := w.Store.IssueEventRange(ctx, sqlc.IssueEventRangeParams{ProjectID: p.ID, Fingerprint: fp, FromAt: seenFrom, ToAt: seenTo}); err == nil {
		d.OldestID = string(nb.Oldest)
	}
	if users, err := w.Store.IssueUsers(ctx, sqlc.IssueUsersParams{ProjectID: p.ID, Column2: []sentry.ID{fp}, OccurredAt: win.From, OccurredAt_2: win.To}); err == nil && len(users) > 0 {
		d.Users = users[0].Users
	}
	bf := store.EventFilter{ProjectID: p.ID, Fingerprint: fp, From: win.From, To: win.To}
	cols := make([]string, len(breakdownColumns))
	for i, c := range breakdownColumns {
		cols[i] = c[0]
	}
	bds, err := w.Store.Breakdowns(ctx, bf, cols, 5)
	if err != nil {
		w.fail(rw, r, err)
		return
	}
	for _, c := range breakdownColumns {
		bl := BreakdownList{Column: c[0], Label: c[1], Items: bds[c[0]]}
		for _, it := range bl.Items {
			bl.Total += it.Count
		}
		d.Breakdowns = append(d.Breakdowns, bl)
	}
	rows, err := w.Store.IssueTimeline(ctx, sqlc.IssueTimelineParams{ProjectID: p.ID, Fingerprint: fp, FromAt: win.From, ToAt: win.To, Width: win.Seconds()})
	if err != nil {
		w.fail(rw, r, err)
		return
	}
	points := map[int64]int64{}
	for _, r := range rows {
		points[r.Bucket.Unix()] = r.Events
	}
	token := "level-error"
	if is.Level == "fatal" {
		token = "level-fatal"
	}
	d.Chart = singleChart("events", token, points, win)
	// The list covers the page's window, like the chart and the breakdowns.
	d.Events, d.More, err = w.Store.ListEvents(ctx, store.EventFilter{ProjectID: p.ID, Fingerprint: fp, From: win.From, To: win.To, Before: s.Cursor(), Limit: 25})
	if err != nil {
		w.fail(rw, r, err)
		return
	}
	w.page(rw, r, Page{S: s, Project: &p, Section: "issues"}, func(pg Page) templ.Component { return IssuePage(pg, d) })
}

func (w *Web) issueStatus(rw http.ResponseWriter, r *http.Request) {
	p, ok := w.project(rw, r)
	if !ok {
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(rw, "bad form", http.StatusBadRequest)
		return
	}
	status, ig, okStatus := statusForm(r.Form, now())
	if !okStatus {
		http.Error(rw, "bad status", http.StatusBadRequest)
		return
	}
	fp, ok := sentry.ParseID(r.PathValue("fingerprint"))
	if !ok {
		http.NotFound(rw, r)
		return
	}
	if _, err := w.Store.SetIssueStatus(r.Context(), sqlc.SetIssueStatusParams{ProjectID: p.ID, Fingerprint: fp, Status: sqlc.IssueStatus(status), StatusBy: actorName(r),
		IgnoreUntil: ig.Until, IgnoreEvents: ig.Events, IgnoreEscalating: ig.Escalating}); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			http.NotFound(rw, r)
			return
		}
		w.fail(rw, r, err)
		return
	}
	redirect(rw, r, state(r).Href("/issues/"+string(fp)))
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
	f := store.EventFilter{ProjectID: p.ID, From: win.From, To: win.To, Before: s.Cursor(), Limit: eventsPage,
		Level: s.Filters["level"], Release: s.Filters["release"], Environment: s.Filters["environment"], Platform: s.Filters["platform"],
		ErrorType: s.Filters["error_type"], UserID: s.Filters["user_id"], DeviceID: s.Filters["device_id"], DeviceModel: s.Filters["device_model"],
		OSVersion: s.Filters["os_version"], Transaction: s.Filters["transaction"], Location: s.Filters["culprit"],
		Query: s.Filters["q"], Handled: s.Filters["handled"]}
	if fp, ok := sentry.ParseID(s.Filters["fingerprint"]); ok {
		f.Fingerprint = fp
	}
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
	d.Releases = w.releaseNames(ctx, p.ID)
	pg := Page{S: s, Project: &p, Section: "events"}
	w.page(rw, r, pg, func(pg Page) templ.Component { return Events(pg, d) })
}

// EventData feeds the event page.
type EventData struct {
	E           sqlc.Event
	Payload     json.RawMessage // the raw event (events.payload, decoded); nil when the row has none
	Issue       *sqlc.Issue
	Stacks      []Stack
	Crumbs      []sentry.Breadcrumb
	Contexts    []ContextGroup
	User        []KV
	Tags        map[string]string
	Attachments []sqlc.ListAttachmentsRow
	UserReport  *sqlc.UserReport
}

// isImage: an attachment the page shows inline (the browser renders it;
// anything else is a download).
func isImage(contentType string) bool { return api.InlineImage(contentType) }

// attachment is GET /p/{slug}/events/{id}/attachments/{n}: the bytes.
func (w *Web) attachment(rw http.ResponseWriter, r *http.Request) {
	p, ok := w.project(rw, r)
	if !ok {
		return
	}
	id, ok := sentry.ParseID(r.PathValue("id"))
	n, err := strconv.Atoi(r.PathValue("n"))
	if !ok || err != nil || n < 0 {
		http.NotFound(rw, r)
		return
	}
	e, err := w.Store.GetEvent(r.Context(), sqlc.GetEventParams{ProjectID: p.ID, EventID: id})
	if err == nil {
		var a sqlc.Attachment
		a, err = w.Store.GetAttachment(r.Context(), sqlc.GetAttachmentParams{ProjectID: p.ID, EventID: id, OccurredAt: e.OccurredAt, N: int32(n)})
		if err == nil {
			api.ServeAttachment(rw, a)
			return
		}
	}
	if errors.Is(err, pgx.ErrNoRows) {
		http.NotFound(rw, r)
		return
	}
	w.fail(rw, r, err)
}

func (w *Web) event(rw http.ResponseWriter, r *http.Request) {
	p, ok := w.project(rw, r)
	if !ok {
		return
	}
	ctx := r.Context()
	id, ok := sentry.ParseID(r.PathValue("id"))
	if !ok {
		http.NotFound(rw, r)
		return
	}
	e, err := w.Store.GetEvent(ctx, sqlc.GetEventParams{ProjectID: p.ID, EventID: id})
	if errors.Is(err, pgx.ErrNoRows) {
		http.NotFound(rw, r)
		return
	}
	if err != nil {
		w.fail(rw, r, err)
		return
	}
	payload := w.payload(e)
	ev := parsePayload(e, payload) // once: stacks, breadcrumbs and contexts all read it
	d := EventData{E: e, Payload: payload, Stacks: stacksOf(e, ev), Crumbs: crumbsOf(ev), Tags: tagsMap(e.Tags)}
	d.Contexts, d.User = payloadContexts(payload)
	if d.Attachments, err = w.Store.ListAttachments(ctx, sqlc.ListAttachmentsParams{ProjectID: p.ID, EventID: e.EventID, OccurredAt: e.OccurredAt}); err != nil {
		w.fail(rw, r, err)
		return
	}
	if ur, err := w.Store.GetUserReport(ctx, sqlc.GetUserReportParams{ProjectID: p.ID, EventID: e.EventID}); err == nil {
		d.UserReport = &ur
	} else if !errors.Is(err, pgx.ErrNoRows) {
		w.fail(rw, r, err)
		return
	}
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

// payload is an event's raw payload; nil when the row has none (imported
// without one) or it does not decode — the page still renders from the
// columns.
func (w *Web) payload(e sqlc.Event) []byte {
	b, err := store.Payload(e)
	if err != nil {
		w.Log.Error("event payload", "event", e.EventID, "err", err)
		return nil
	}
	return b
}

// ── releases ────────────────────────────────────────────────

// ReleaseRow is one line of the releases table.
type ReleaseRow struct {
	Release, Platform   string
	Sessions, Crashed   int64
	TotalSessions       int64 // all releases in the window (adoption denominator)
	Unhandled, Events   int64
	NewIssues           int64
	FirstSeen, LastSeen time.Time // zero when the release has sessions but no events
}

func (r ReleaseRow) Adoption() string  { return percent(r.Sessions, r.TotalSessions) }
func (r ReleaseRow) CrashFree() string { return crashFree(r.Sessions, r.Crashed) }

func (w *Web) releaseRows(ctx context.Context, p sqlc.Project, win Window) ([]ReleaseRow, error) {
	stats, err := w.Store.ReleaseStats(ctx, sqlc.ReleaseStatsParams{ProjectID: p.ID, FromAt: win.From, ToAt: win.To})
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
	rows := make([]ReleaseRow, 0, len(stats))
	idx := map[string]bool{}
	for _, st := range stats {
		idx[st.Release] = true
		h := hm[st.Release]
		rows = append(rows, ReleaseRow{Release: st.Release, Platform: strings.Join(st.Platforms, ", "), Sessions: h.Total, Crashed: h.Crashed,
			TotalSessions: total, NewIssues: im[st.Release], FirstSeen: st.FirstSeen, LastSeen: st.LastSeen, Unhandled: st.Unhandled, Events: st.Events})
	}
	// releases with sessions but no events still count
	for rel, h := range hm {
		if !idx[rel] {
			rows = append(rows, ReleaseRow{Release: rel, Sessions: h.Total, Crashed: h.Crashed, TotalSessions: total, NewIssues: im[rel]})
		}
	}
	sort.SliceStable(rows, func(i, j int) bool { return rows[i].LastSeen.After(rows[j].LastSeen) })
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
	dayFrom := win.From.Truncate(day)
	daily, err := w.Store.ReleaseHealthDaily(ctx, sqlc.ReleaseHealthDailyParams{ProjectID: p.ID, Release: version, Bucket: dayFrom, Bucket_2: win.To})
	if err != nil {
		w.fail(rw, r, err)
		return
	}
	dm := map[int64]sqlc.ReleaseHealthDailyRow{}
	for _, row := range daily {
		dm[row.Bucket.Unix()] = row
	}
	for b := dayFrom; b.Before(win.To); b = b.Add(day) {
		row := dm[b.Unix()]
		d.Health = append(d.Health, HealthPoint{Label: b.Format("Jan 2"), Total: row.Total, Crashed: row.Crashed})
	}
	tl, err := w.Store.ReleaseTimeline(ctx, sqlc.ReleaseTimelineParams{ProjectID: p.ID, Release: version, FromAt: win.From, ToAt: win.To, Width: win.Seconds()})
	if err != nil {
		w.fail(rw, r, err)
		return
	}
	points := map[int64]int64{}
	for _, row := range tl {
		points[row.Bucket.Unix()] = row.Unhandled
	}
	d.Chart = singleChart("unhandled", "level-fatal", points, win)
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

// releaseNames lists the project's releases for filter dropdowns, newest first.
func (w *Web) releaseNames(ctx context.Context, projectID int64) []string {
	rows, err := w.Store.ListReleases(ctx, sqlc.ListReleasesParams{ProjectID: projectID, Limit: 50})
	if err != nil {
		return nil
	}
	out := make([]string, 0, len(rows))
	for _, r := range rows {
		out = append(out, r.Release)
	}
	return out
}

// ── feedback (user reports) ────────────────────────────────────

// feedbackPageSize bounds the Feedback page: reports are low-volume by
// nature (one per event, user-submitted), so a single page with no
// offset control is enough.
const feedbackPageSize = 100

func (w *Web) feedback(rw http.ResponseWriter, r *http.Request) {
	p, ok := w.project(rw, r)
	if !ok {
		return
	}
	rows, err := w.Store.ListUserReports(r.Context(), sqlc.ListUserReportsParams{ProjectID: p.ID, Limit: feedbackPageSize})
	if err != nil {
		w.fail(rw, r, err)
		return
	}
	pg := Page{S: state(r), Project: &p, Section: "feedback"}
	w.page(rw, r, pg, func(pg Page) templ.Component { return Feedback(pg, rows) })
}

// ── monitors (Sentry's cron monitoring) ────────────────────────

// monitorCheckInsPageSize mirrors internal/api's: enough recent runs to
// see a pattern without paging.
const monitorCheckInsPageSize = 100

func (w *Web) monitors(rw http.ResponseWriter, r *http.Request) {
	p, ok := w.project(rw, r)
	if !ok {
		return
	}
	rows, err := w.Store.ListMonitors(r.Context(), p.ID)
	if err != nil {
		w.fail(rw, r, err)
		return
	}
	pg := Page{S: state(r), Project: &p, Section: "monitors"}
	w.page(rw, r, pg, func(pg Page) templ.Component { return Monitors(pg, rows) })
}

func (w *Web) monitor(rw http.ResponseWriter, r *http.Request) {
	p, ok := w.project(rw, r)
	if !ok {
		return
	}
	slug := r.PathValue("monitor")
	ctx := r.Context()
	m, err := w.Store.GetMonitor(ctx, sqlc.GetMonitorParams{ProjectID: p.ID, Slug: slug})
	if errors.Is(err, pgx.ErrNoRows) {
		http.NotFound(rw, r)
		return
	}
	if err != nil {
		w.fail(rw, r, err)
		return
	}
	checkIns, err := w.Store.ListCheckIns(ctx, sqlc.ListCheckInsParams{ProjectID: p.ID, MonitorSlug: slug, Limit: monitorCheckInsPageSize})
	if err != nil {
		w.fail(rw, r, err)
		return
	}
	pg := Page{S: state(r), Project: &p, Section: "monitors"}
	w.page(rw, r, pg, func(pg Page) templ.Component { return MonitorPage(pg, m, checkIns) })
}

func (w *Web) monitorDelete(rw http.ResponseWriter, r *http.Request) {
	p, ok := w.project(rw, r)
	if !ok {
		return
	}
	if _, err := w.Store.DeleteMonitor(r.Context(), sqlc.DeleteMonitorParams{ProjectID: p.ID, Slug: r.PathValue("monitor")}); err != nil {
		w.fail(rw, r, err)
		return
	}
	redirect(rw, r, state(r).Href("/monitors"))
}
