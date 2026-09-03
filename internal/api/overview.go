package api

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/crashcartapp/crashcart/internal/store"
)

// topReleases is how many releases the overview timeline keeps apart;
// the rest are folded into "other".
const topReleases = 5

type totalsOut struct {
	Events    int64 `json:"events"`
	Unhandled int64 `json:"unhandled"`
	Errors    int64 `json:"errors"`
}

type crashFreeOut struct {
	Release  string  `json:"release"`
	Rate     float64 `json:"rate"`
	Sessions int64   `json:"sessions"`
}

type timelinePoint struct {
	Bucket    time.Time `json:"bucket"`
	Release   string    `json:"release"`
	Unhandled int64     `json:"unhandled"`
	Events    int64     `json:"events"`
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

	tot, err := store.Totals(ctx, h.Store.Pool, p.ID, hlo, hi)
	if err != nil {
		h.fail(w, err)
		return
	}
	out.Totals = totalsOut{Events: tot.Events, Unhandled: tot.Unhandled, Errors: tot.Errors}

	levels, err := store.LevelTotals(ctx, h.Store.Pool, p.ID, hlo, hi)
	if err != nil {
		h.fail(w, err)
		return
	}
	for _, l := range levels {
		out.Levels[string(l.Level)] = l.Events
	}
	if out.NewIssues, err = store.CountNewIssuesIn(ctx, h.Store.Pool, p.ID, lo, hi); err != nil {
		h.fail(w, err)
		return
	}
	if out.Regressions, err = store.CountRegressionsIn(ctx, h.Store.Pool, p.ID, lo, hi); err != nil {
		h.fail(w, err)
		return
	}

	tl, err := store.Timeline(ctx, h.Store.Pool, p.ID, hlo, hi, hourly, topReleases)
	if err != nil {
		h.fail(w, err)
		return
	}
	for _, t := range tl {
		out.Timeline = append(out.Timeline, timelinePoint{Bucket: t.Bucket.UTC(), Release: t.Release, Unhandled: t.Unhandled, Events: t.Events})
	}

	if out.CrashFree, err = h.crashFree(ctx, p.ID, hlo, hi); err != nil {
		h.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

// hourly is the stats bucket width of the API (seconds): its windows are
// caller-chosen RFC3339 times, aligned to the hour, never to the day.
const hourly = int64(time.Hour / time.Second)

// crashFree reports the most recently active release's crash-free rate;
// nil when it has no sessions in the window.
func (h *Handler) crashFree(ctx context.Context, projectID int64, hlo, hi time.Time) (*crashFreeOut, error) {
	lr, err := store.LatestReleaseHealth(ctx, h.Store.Pool, projectID, hlo, hlo.Truncate(24*time.Hour), hi)
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
