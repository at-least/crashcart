package api

import (
	"net/http"
	"sort"
	"time"

	"github.com/newlix/crashcart/internal/db/sqlc"
	"github.com/newlix/crashcart/internal/pk"
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
	Crashes       int64       `json:"crashes"`
	Errors        int64       `json:"errors"`
	Sessions      sessionsOut `json:"sessions"`
	CrashFreeRate *float64    `json:"crash_free_rate"`
	NewIssues     int64       `json:"new_issues"`
}

// mergeReleaseStats folds the per-(release, platform) rows into one entry
// per release, most recently active first.
func mergeReleaseStats(rows []sqlc.ReleaseStatsRow) []*releaseOut {
	byRel := map[string]*releaseOut{}
	var order []*releaseOut
	for _, r := range rows {
		o := byRel[r.Release]
		if o == nil {
			o = &releaseOut{Release: r.Release, Platforms: []string{}, FirstSeen: pk.Time(r.FirstSeen), LastSeen: pk.Time(r.LastSeen)}
			byRel[r.Release] = o
			order = append(order, o)
		}
		if r.Platform != "" {
			o.Platforms = append(o.Platforms, r.Platform)
		}
		if t := pk.Time(r.FirstSeen); t.Before(o.FirstSeen) {
			o.FirstSeen = t
		}
		if t := pk.Time(r.LastSeen); t.After(o.LastSeen) {
			o.LastSeen = t
		}
		o.Events += r.Events
		o.Crashes += r.Crashes
		o.Errors += r.Errors
	}
	for _, o := range order {
		sort.Strings(o.Platforms)
	}
	sort.SliceStable(order, func(i, j int) bool { return order[i].LastSeen.After(order[j].LastSeen) })
	return order
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
	lo, hi := pk.Lower(from), pk.Upper(to)
	stats, err := h.Store.ReleaseStats(ctx, sqlc.ReleaseStatsParams{ProjectID: p.ID, Bucket: pk.Bucket(lo, pk.Hour), Bucket_2: hi})
	if err != nil {
		h.fail(w, err)
		return
	}
	health, err := h.Store.ReleaseHealthNN(ctx, sqlc.ReleaseHealthNNParams{ProjectID: p.ID, Bucket: pk.Bucket(lo, pk.Day), Bucket_2: hi})
	if err != nil {
		h.fail(w, err)
		return
	}
	fresh, err := h.Store.NewIssuesByRelease(ctx, sqlc.NewIssuesByReleaseParams{ProjectID: p.ID, FirstSeen: lo, FirstSeen_2: hi})
	if err != nil {
		h.fail(w, err)
		return
	}
	out := mergeReleaseStats(stats)
	byRel := map[string]*releaseOut{}
	for _, o := range out {
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
	Bucket  time.Time `json:"bucket"`
	Events  int64     `json:"events"`
	Crashes int64     `json:"crashes"`
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
	lo, hi := pk.Lower(from), pk.Upper(to)
	hlo, dlo := pk.Bucket(lo, pk.Hour), pk.Bucket(lo, pk.Day)

	stats, err := h.Store.ReleaseStats(ctx, sqlc.ReleaseStatsParams{ProjectID: p.ID, Bucket: hlo, Bucket_2: hi})
	if err != nil {
		h.fail(w, err)
		return
	}
	var rel *releaseOut
	for _, o := range mergeReleaseStats(stats) {
		if o.Release == version {
			rel = o
		}
	}
	health, err := h.Store.ReleaseHealthDailyNN(ctx, sqlc.ReleaseHealthDailyNNParams{ProjectID: p.ID, Release: version, Bucket: dlo, Bucket_2: hi})
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
		daily = append(daily, dailyHealth{Day: pk.Time(d.Bucket), Total: d.Total, Crashed: d.Crashed, Errored: d.Errored, CrashFreeRate: crashFreeRate(d.Total, d.Crashed)})
		rel.Sessions.Total += d.Total
		rel.Sessions.Crashed += d.Crashed
		rel.Sessions.Errored += d.Errored
	}
	rel.CrashFreeRate = crashFreeRate(rel.Sessions.Total, rel.Sessions.Crashed)

	tl, err := h.Store.ReleaseTimeline(ctx, sqlc.ReleaseTimelineParams{ProjectID: p.ID, Release: version, Bucket: hlo, Bucket_2: hi})
	if err != nil {
		h.fail(w, err)
		return
	}
	byBucket := map[int64]sqlc.ReleaseTimelineRow{}
	for _, t := range tl {
		byBucket[t.Bucket] = t
	}
	timeline := make([]releaseTimelinePoint, 0, (hi-hlo)/pk.Hour+1)
	for b := hlo; b < hi; b += pk.Hour {
		t := byBucket[b]
		timeline = append(timeline, releaseTimelinePoint{Bucket: pk.Time(b), Events: t.Events, Crashes: t.Crashes})
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
