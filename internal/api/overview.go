package api

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/crashcartapp/crashcart/internal/db/sqlc"
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
	lo, hi := from, to
	hlo := lo.Truncate(time.Hour) // include the bucket containing `from`
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
		out.Levels[string(l.Level)] = l.Events
	}
	if out.NewIssues, err = h.Store.CountNewIssues(ctx, sqlc.CountNewIssuesParams{ProjectID: p.ID, FirstSeen: lo}); err != nil {
		h.fail(w, err)
		return
	}
	if out.Regressions, err = h.Store.CountRegressions(ctx, sqlc.CountRegressionsParams{ProjectID: p.ID, LastSeen: lo}); err != nil {
		h.fail(w, err)
		return
	}

	tl, err := h.Store.Timeline(ctx, sqlc.TimelineParams{ProjectID: p.ID, FromAt: hlo, ToAt: hi, Width: 3600, Top: topReleases})
	if err != nil {
		h.fail(w, err)
		return
	}
	for _, t := range tl {
		out.Timeline = append(out.Timeline, timelinePoint{Bucket: t.Bucket.UTC(), Release: t.Release, Crashes: t.Crashes, Events: t.Events})
	}

	if out.CrashFree, err = h.crashFree(ctx, p.ID, hlo, hi); err != nil {
		h.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

// crashFree reports the most recently active release's crash-free rate;
// nil when it has no sessions in the window.
func (h *Handler) crashFree(ctx context.Context, projectID int64, hlo, hi time.Time) (*crashFreeOut, error) {
	lr, err := h.Store.LatestReleaseHealth(ctx, sqlc.LatestReleaseHealthParams{ProjectID: projectID, HourFrom: hlo, DayFrom: hlo.Truncate(24 * time.Hour), ToAt: hi})
	if errors.Is(err, pgx.ErrNoRows) || (err == nil && lr.Total == 0) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &crashFreeOut{Release: lr.Release, Sessions: lr.Total, Rate: 1 - float64(lr.Crashed)/float64(lr.Total)}, nil
}

// crashFreeRate returns 1 - crashed/total, or nil when there are no sessions.
func crashFreeRate(total, crashed int64) *float64 {
	if total <= 0 {
		return nil
	}
	v := 1 - float64(crashed)/float64(total)
	return &v
}
