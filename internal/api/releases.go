package api

import (
	"net/http"
	"time"

	"github.com/at-least/crashcart/internal/db/sqlc"
)

type sessionsOut struct {
	Total   int64 `json:"total"`
	Crashed int64 `json:"crashed"`
	Errored int64 `json:"errored"`
}

type releaseOut struct {
	Release       string      `json:"release"`
	Platforms     []string    `json:"platforms"`
	FirstSeen     time.Time   `json:"first_seen"`
	LastSeen      time.Time   `json:"last_seen"`
	Events        int64       `json:"events"`
	Unhandled     int64       `json:"unhandled"`
	Errors        int64       `json:"errors"`
	Sessions      sessionsOut `json:"sessions"`
	CrashFreeRate *float64    `json:"crash_free_rate"`
	NewIssues     int64       `json:"new_issues"`
}

func toReleaseOut(r sqlc.ReleaseStatsRow) *releaseOut {
	platforms := r.Platforms
	if platforms == nil {
		platforms = []string{}
	}
	return &releaseOut{Release: r.Release, Platforms: platforms, FirstSeen: r.FirstSeen.UTC(), LastSeen: r.LastSeen.UTC(),
		Events: r.Events, Unhandled: r.Unhandled, Errors: r.Errors}
}

func (h *Handler) listReleases(w http.ResponseWriter, r *http.Request) {
	p, ok := h.project(w, r)
	if !ok {
		return
	}
	from, to, err := ParseWindow(r.URL.Query())
	if err != nil {
		h.fail(w, err)
		return
	}
	ctx := r.Context()
	stats, err := h.Store.ReleaseStats(ctx, sqlc.ReleaseStatsParams{ProjectID: p.ID, FromAt: from.Truncate(time.Hour), ToAt: to})
	if err != nil {
		h.fail(w, err)
		return
	}
	health, err := h.Store.ReleaseHealth(ctx, sqlc.ReleaseHealthParams{ProjectID: p.ID, Bucket: from.Truncate(24 * time.Hour), Bucket_2: to})
	if err != nil {
		h.fail(w, err)
		return
	}
	fresh, err := h.Store.NewIssuesByRelease(ctx, sqlc.NewIssuesByReleaseParams{ProjectID: p.ID, FirstSeen: from, FirstSeen_2: to})
	if err != nil {
		h.fail(w, err)
		return
	}
	out := make([]*releaseOut, 0, len(stats))
	byRel := map[string]*releaseOut{}
	for _, st := range stats {
		o := toReleaseOut(st)
		out = append(out, o)
		byRel[o.Release] = o
	}
	for _, hr := range health {
		o := byRel[hr.Release]
		if o == nil {
			// Sessions without events (healthy release): still listed.
			o = &releaseOut{Release: hr.Release, Platforms: []string{}}
			byRel[hr.Release] = o
			out = append(out, o)
		}
		o.Sessions = sessionsOut{Total: hr.Total, Crashed: hr.Crashed, Errored: hr.Errored}
		o.CrashFreeRate = crashFreeRate(hr.Total, hr.Crashed)
	}
	for _, n := range fresh {
		if o := byRel[deref(n.Release)]; o != nil {
			o.NewIssues = n.N
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"releases": out})
}

type dailyHealth struct {
	Day           time.Time `json:"day"`
	Total         int64     `json:"total"`
	Crashed       int64     `json:"crashed"`
	Errored       int64     `json:"errored"`
	CrashFreeRate *float64  `json:"crash_free_rate"`
}

type releaseTimelinePoint struct {
	Bucket    time.Time `json:"bucket"`
	Events    int64     `json:"events"`
	Unhandled int64     `json:"unhandled"`
}

func (h *Handler) getRelease(w http.ResponseWriter, r *http.Request) {
	p, ok := h.project(w, r)
	if !ok {
		return
	}
	from, to, err := ParseWindow(r.URL.Query())
	if err != nil {
		h.fail(w, err)
		return
	}
	ctx := r.Context()
	version := r.PathValue("version")
	hi := to
	hlo, dlo := from.Truncate(time.Hour), from.Truncate(24*time.Hour)

	stats, err := h.Store.ReleaseStats(ctx, sqlc.ReleaseStatsParams{ProjectID: p.ID, FromAt: hlo, ToAt: hi})
	if err != nil {
		h.fail(w, err)
		return
	}
	var rel *releaseOut
	for _, st := range stats {
		if st.Release == version {
			rel = toReleaseOut(st)
		}
	}
	health, err := h.Store.ReleaseHealthDaily(ctx, sqlc.ReleaseHealthDailyParams{ProjectID: p.ID, Release: version, Bucket: dlo, Bucket_2: hi})
	if err != nil {
		h.fail(w, err)
		return
	}
	issues, err := h.Store.ListIssuesByRelease(ctx, sqlc.ListIssuesByReleaseParams{ProjectID: p.ID, FirstRelease: &version, Limit: 200})
	if err != nil {
		h.fail(w, err)
		return
	}
	if rel == nil && len(health) == 0 && len(issues) == 0 {
		writeErr(w, http.StatusNotFound, "not found")
		return
	}
	if rel == nil {
		rel = &releaseOut{Release: version, Platforms: []string{}}
	}
	daily := make([]dailyHealth, 0, len(health))
	for _, d := range health {
		daily = append(daily, dailyHealth{Day: d.Bucket.UTC(), Total: d.Total, Crashed: d.Crashed, Errored: d.Errored, CrashFreeRate: crashFreeRate(d.Total, d.Crashed)})
		rel.Sessions.Total += d.Total
		rel.Sessions.Crashed += d.Crashed
		rel.Sessions.Errored += d.Errored
	}
	rel.CrashFreeRate = crashFreeRate(rel.Sessions.Total, rel.Sessions.Crashed)

	tl, err := h.Store.ReleaseTimeline(ctx, sqlc.ReleaseTimelineParams{ProjectID: p.ID, Release: version, FromAt: hlo, ToAt: hi, Width: hourly})
	if err != nil {
		h.fail(w, err)
		return
	}
	timeline := make([]releaseTimelinePoint, 0, len(tl))
	for _, t := range tl {
		timeline = append(timeline, releaseTimelinePoint{Bucket: t.Bucket.UTC(), Events: t.Events, Unhandled: t.Unhandled})
	}
	introduced, present := []issueOut{}, []issueOut{}
	for _, i := range issues {
		if deref(i.FirstRelease) == version {
			introduced = append(introduced, toIssueOut(i))
			rel.NewIssues++
		}
		if deref(i.LastRelease) == version {
			present = append(present, toIssueOut(i))
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"release": rel, "from": from, "to": to, "daily_health": daily, "timeline": timeline,
		"issues_introduced": introduced, "issues_present": present,
	})
}
