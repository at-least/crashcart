package api

import (
	"net/http"
	"sort"
	"time"

	"github.com/crashcartapp/crashcart/internal/db/sqlc"
	"github.com/crashcartapp/crashcart/internal/pk"
)

// topReleases is how many releases the overview timeline keeps apart;
// the rest are folded into "other".
const topReleases = 5

type totalsOut struct {
	Events  int64 `json:"events"`
	Crashes int64 `json:"crashes"`
	Errors  int64 `json:"errors"`
}

type crashFreeOut struct {
	Release  string  `json:"release"`
	Rate     float64 `json:"rate"`
	Sessions int64   `json:"sessions"`
}

type timelinePoint struct {
	Bucket  time.Time `json:"bucket"`
	Release string    `json:"release"`
	Crashes int64     `json:"crashes"`
	Events  int64     `json:"events"`
}

type overviewOut struct {
	From        time.Time        `json:"from"`
	To          time.Time        `json:"to"`
	Totals      totalsOut        `json:"totals"`
	Levels      map[string]int64 `json:"levels"`
	NewIssues   int64            `json:"new_issues"`
	Regressions int64            `json:"regressions"`
	CrashFree   *crashFreeOut    `json:"crash_free"`
	Timeline    []timelinePoint  `json:"timeline"`
}

func (h *Handler) overview(w http.ResponseWriter, r *http.Request) {
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
	hlo := pk.Bucket(lo, pk.Hour) // include the bucket containing `from`
	out := overviewOut{From: from, To: to, Levels: map[string]int64{}, Timeline: []timelinePoint{}}

	tot, err := h.Store.Totals(ctx, sqlc.TotalsParams{ProjectID: p.ID, Bucket: hlo, Bucket_2: hi})
	if err != nil {
		h.fail(w, err)
		return
	}
	out.Totals = totalsOut{Events: tot.Events, Crashes: tot.Crashes, Errors: tot.Errors}

	levels, err := h.Store.LevelTotals(ctx, sqlc.LevelTotalsParams{ProjectID: p.ID, Bucket: hlo, Bucket_2: hi})
	if err != nil {
		h.fail(w, err)
		return
	}
	for _, l := range levels {
		out.Levels[l.Level] = l.Events
	}
	if out.NewIssues, err = h.Store.CountNewIssues(ctx, sqlc.CountNewIssuesParams{ProjectID: p.ID, FirstSeen: lo}); err != nil {
		h.fail(w, err)
		return
	}
	if out.Regressions, err = h.Store.CountRegressions(ctx, sqlc.CountRegressionsParams{ProjectID: p.ID, LastSeen: lo}); err != nil {
		h.fail(w, err)
		return
	}

	tl, err := h.Store.Timeline(ctx, sqlc.TimelineParams{ProjectID: p.ID, Bucket: hlo, Bucket_2: hi})
	if err != nil {
		h.fail(w, err)
		return
	}
	out.Timeline = foldTimeline(tl, hlo, hi)

	if out.CrashFree, err = h.crashFree(r, p.ID, lo, hi, tl); err != nil {
		h.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

// foldTimeline keeps the top releases by events and folds the rest into
// "other"; every hourly bucket in [lo, hi) is emitted for each kept series.
func foldTimeline(rows []sqlc.TimelineRow, lo, hi int64) []timelinePoint {
	if len(rows) == 0 {
		return []timelinePoint{}
	}
	byRelease := map[string]int64{}
	for _, r := range rows {
		byRelease[r.Release] += r.Events
	}
	names := make([]string, 0, len(byRelease))
	for n := range byRelease {
		names = append(names, n)
	}
	sort.Slice(names, func(i, j int) bool {
		if byRelease[names[i]] != byRelease[names[j]] {
			return byRelease[names[i]] > byRelease[names[j]]
		}
		return names[i] < names[j]
	})
	keep := map[string]bool{}
	for i, n := range names {
		if i < topReleases {
			keep[n] = true
		}
	}
	series := names
	if len(names) > topReleases {
		series = append(append([]string{}, names[:topReleases]...), "other")
	}
	type key struct {
		bucket  int64
		release string
	}
	agg := map[key]*timelinePoint{}
	for _, r := range rows {
		rel := r.Release
		if !keep[rel] {
			rel = "other"
		}
		k := key{r.Bucket, rel}
		pt := agg[k]
		if pt == nil {
			pt = &timelinePoint{Bucket: pk.Time(r.Bucket), Release: rel}
			agg[k] = pt
		}
		pt.Events += r.Events
		pt.Crashes += r.Crashes
	}
	out := []timelinePoint{}
	for b := pk.Bucket(lo, pk.Hour); b < hi; b += pk.Hour {
		for _, rel := range series {
			if pt := agg[key{b, rel}]; pt != nil {
				out = append(out, *pt)
			} else {
				out = append(out, timelinePoint{Bucket: pk.Time(b), Release: rel})
			}
		}
	}
	return out
}

// crashFree picks the most recently active release that has sessions in
// the window and computes 1 - crashed/total.
func (h *Handler) crashFree(r *http.Request, projectID, lo, hi int64, tl []sqlc.TimelineRow) (*crashFreeOut, error) {
	health, err := h.Store.ReleaseHealthNN(r.Context(), sqlc.ReleaseHealthNNParams{ProjectID: projectID, Bucket: pk.Bucket(lo, pk.Day), Bucket_2: hi})
	if err != nil {
		return nil, err
	}
	if len(health) == 0 {
		return nil, nil
	}
	// Rank releases by their latest event bucket; releases without events
	// fall back to name order so the choice is still deterministic.
	lastSeen := map[string]int64{}
	for _, t := range tl {
		if t.Bucket > lastSeen[t.Release] {
			lastSeen[t.Release] = t.Bucket
		}
	}
	sort.Slice(health, func(i, j int) bool {
		a, b := health[i], health[j]
		if lastSeen[a.Release] != lastSeen[b.Release] {
			return lastSeen[a.Release] > lastSeen[b.Release]
		}
		return a.Release > b.Release
	})
	best := health[0]
	out := &crashFreeOut{Release: best.Release, Sessions: best.Total, Rate: 1}
	if best.Total > 0 {
		out.Rate = 1 - float64(best.Crashed)/float64(best.Total)
	}
	return out, nil
}

// crashFreeRate returns 1 - crashed/total, or nil when there are no sessions.
func crashFreeRate(total, crashed int64) *float64 {
	if total <= 0 {
		return nil
	}
	v := 1 - float64(crashed)/float64(total)
	return &v
}
