package web

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"time"

	"github.com/newlix/crashcart/internal/db/sqlc"
	"github.com/newlix/crashcart/internal/store"
	"github.com/newlix/crashcart/internal/timerange"
)

// PageSize is the log table page length.
const PageSize = 50

// ChartData is what app.js renders on a canvas.
type ChartData struct {
	Labels []string      `json:"labels"`
	Series []ChartSeries `json:"series"`
}

// ChartSeries is one line; Token is the CSS color token (no leading --).
type ChartSeries struct {
	Name  string  `json:"name"`
	Token string  `json:"token"`
	Data  []int64 `json:"data"`
}

// Empty reports whether there's nothing to draw.
func (c ChartData) Empty() bool { return len(c.Labels) == 0 }

// DashboardData feeds the dashboard template.
type DashboardData struct {
	Stats    store.Stats
	Issues   []sqlc.Issue
	Timeline ChartData
	Volume   ChartData
	Events   []store.Event
	Versions []string
}

// TopIssues sorts by event count (desc) and keeps the first n.
func TopIssues(issues []sqlc.Issue, n int) []sqlc.Issue {
	out := append([]sqlc.Issue(nil), issues...)
	sort.SliceStable(out, func(i, j int) bool { return out[i].EventCount > out[j].EventCount })
	if len(out) > n {
		out = out[:n]
	}
	return out
}

// bucketLabel formats a UTC bucket time in the viewer's zone: "08-22" for
// days, "14:00" for hours.
func bucketLabel(t time.Time, hourly bool, tzHours int) string {
	if hourly {
		return t.UTC().Add(time.Duration(tzHours) * time.Hour).Format("15:00")
	}
	return t.UTC().Format("01-02")
}

func timelineChart(points []store.Point, hourly bool, tz int) ChartData {
	c := ChartData{Series: []ChartSeries{{Name: "crashes", Token: "chart-fatal"}}}
	for _, p := range points {
		c.Labels = append(c.Labels, bucketLabel(p.Time, hourly, tz))
		c.Series[0].Data = append(c.Series[0].Data, p.Count)
	}
	return c
}

func volumeChart(points []store.VolumePoint, hourly bool, tz int) ChartData {
	c := ChartData{Series: []ChartSeries{{Name: "fatal", Token: "chart-fatal"}, {Name: "error", Token: "chart-error"}}}
	for _, p := range points {
		c.Labels = append(c.Labels, bucketLabel(p.Time, hourly, tz))
		c.Series[0].Data = append(c.Series[0].Data, p.Fatal)
		c.Series[1].Data = append(c.Series[1].Data, p.Error)
	}
	return c
}

func (h *Handler) loadDashboard(ctx context.Context, s ViewState, w Window) (DashboardData, error) {
	var d DashboardData
	var err error
	if d.Stats, err = h.Store.Stats(ctx, w.Range); err != nil {
		return d, err
	}
	if d.Issues, err = h.Store.ListIssues(ctx, store.IssueFilter{Range: w.Range}); err != nil {
		return d, err
	}
	tl, err := h.Store.CrashTimeline(ctx, w.Range, w.Hourly)
	if err != nil {
		return d, err
	}
	d.Timeline = timelineChart(tl, w.Hourly, w.TZ)
	vol, err := h.Store.Volume(ctx, w.Range, w.Hourly)
	if err != nil {
		return d, err
	}
	d.Volume = volumeChart(vol, w.Hourly, w.TZ)

	f := store.EventFilter{
		Range: w.Range, HasRange: true,
		Release:     s.Release,
		CrashesOnly: s.Crash,
		Limit:       PageSize,
		Offset:      s.Page * PageSize,
	}
	for k, v := range s.Filters {
		switch k {
		case "q":
			f.Query = v
		case "level":
			f.Levels = []string{v}
		case "error_type":
			f.ErrorType = v
		case "user_id":
			f.UserID = v
		case "device_id":
			f.DeviceID = v
		case "device_model":
			f.DeviceModel = v
		case "os_version":
			f.OSVersion = v
		case "error_location":
			f.ErrorLocation = v
		case "fingerprint":
			f.Fingerprint = v
		default:
			if f.Tags == nil {
				f.Tags = map[string]string{}
			}
			f.Tags[k] = v
		}
	}
	if s.Err {
		f.Levels = []string{"fatal", "error"} // error toggle overrides any level filter
	}
	if d.Events, err = h.Store.ListEvents(ctx, f); err != nil {
		return d, err
	}
	if d.Versions, err = h.Store.ReleaseVersions(ctx, w.Range); err != nil {
		return d, err
	}
	return d, nil
}

// ── portal (DEPLOYMENTS) ────────────────────────────────────

// PortalIssue is one row of a portal section's issue list.
type PortalIssue struct {
	Title string
	Count int64
	Href  string
}

// PortalSection is one deployment's card row.
type PortalSection struct {
	Name       string
	Href       string
	Reachable  bool
	Crash      int64
	Error      int64
	Timeline   ChartData
	Volume     ChartData
	Issues     []PortalIssue
	IssueTotal int
}

type remoteIssue struct {
	Title      string  `json:"title"`
	ErrorType  *string `json:"error_type"`
	EventCount int64   `json:"event_count"`
}

// portalSection gathers one deployment's data — in-process for this
// instance, over HTTP for the others.
func (h *Handler) portalSection(ctx context.Context, d Deployment, self bool, w Window) PortalSection {
	sec := PortalSection{Name: d.Name, Href: d.URL + "/" + d.Slug + "/dashboard"}
	var (
		stats  store.Stats
		tl     []store.Point
		vol    []store.VolumePoint
		issues []remoteIssue
		err    error
	)
	if self {
		if stats, err = h.Store.Stats(ctx, w.Range); err == nil {
			if tl, err = h.Store.CrashTimeline(ctx, w.Range, w.Hourly); err == nil {
				if vol, err = h.Store.Volume(ctx, w.Range, w.Hourly); err == nil {
					var rows []sqlc.Issue
					rows, err = h.Store.ListIssues(ctx, store.IssueFilter{Range: w.Range})
					for _, r := range rows {
						issues = append(issues, remoteIssue{Title: r.Title, ErrorType: r.ErrorType, EventCount: r.EventCount})
					}
				}
			}
		}
	} else {
		q := w.QueryValues()
		tq := url.Values{}
		for k, v := range q {
			tq[k] = v
		}
		if w.Hourly {
			tq.Set("hourly", "1")
		}
		err = firstErr(
			h.fetchJSON(ctx, d, "/api/stats?"+q.Encode(), &stats),
			h.fetchJSON(ctx, d, "/api/stats/timeline?"+tq.Encode(), &tl),
			h.fetchJSON(ctx, d, "/api/stats/volume?"+tq.Encode(), &vol),
			h.fetchJSON(ctx, d, "/api/issues?"+q.Encode(), &issues),
		)
	}
	if err != nil {
		h.Log.Warn("portal: deployment unreachable", "name", d.Name, "err", err)
		return sec
	}
	sec.Reachable = true
	sec.Crash, sec.Error = stats.Crash, stats.Error
	sec.Timeline = timelineChart(tl, w.Hourly, w.TZ)
	sec.Volume = volumeChart(vol, w.Hourly, w.TZ)
	sec.IssueTotal = len(issues)
	sort.SliceStable(issues, func(i, j int) bool { return issues[i].EventCount > issues[j].EventCount })
	if len(issues) > 10 {
		issues = issues[:10]
	}
	for _, is := range issues {
		href := sec.Href
		if is.ErrorType != nil && *is.ErrorType != "" {
			href += "?error_type=" + url.QueryEscape(*is.ErrorType)
		}
		sec.Issues = append(sec.Issues, PortalIssue{Title: is.Title, Count: is.EventCount, Href: href})
	}
	return sec
}

func firstErr(errs ...error) error {
	for _, e := range errs {
		if e != nil {
			return e
		}
	}
	return nil
}

func (h *Handler) fetchJSON(ctx context.Context, d Deployment, path string, out any) error {
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, d.URL+path, nil)
	if err != nil {
		return err
	}
	if d.Key != "" {
		req.Header.Set("Authorization", "Bearer "+d.Key)
	}
	resp, err := h.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("%s: %s", path, resp.Status)
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

// windowForPortal builds the shared portal window from ?win= and ?anchor=.
func windowForPortal(q url.Values, tz int, now time.Time) (ViewState, Window) {
	s := ViewState{Win: "7d", Filters: map[string]string{}}
	if winValues[q.Get("win")] {
		s.Win = q.Get("win")
	}
	if dateRe.MatchString(q.Get("anchor")) {
		s.Anchor = q.Get("anchor")
	}
	return s, s.WindowFor(tz, now)
}

var _ = timerange.Range{}
