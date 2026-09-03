package api

import (
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/crashcartapp/crashcart/internal/auth"
	"github.com/crashcartapp/crashcart/internal/sentry"
	"github.com/crashcartapp/crashcart/internal/store"
)

// sparklineHours is the length of the per-issue sparkline (7 days, hourly).
const sparklineHours = 7 * 24

// issueStatuses is the lifecycle allowlist for filters; writableStatuses
// is what a client may set — "regression" is ingest's verdict (an event
// on a release outside resolved_releases), never a request's.
var (
	issueStatuses    = map[string]bool{"unresolved": true, "resolved": true, "ignored": true, "regression": true}
	writableStatuses = map[string]bool{"unresolved": true, "resolved": true, "ignored": true}
)

// issueOut is the JSON shape of an issue.
type issueOut struct {
	Fingerprint      string    `json:"fingerprint"`
	Title            string    `json:"title"`
	Level            string    `json:"level"`
	ErrorType        *string   `json:"error_type"`
	Transaction      *string   `json:"transaction"`
	Platform         *string   `json:"platform"`
	Status           string    `json:"status"`
	StatusBy         *string   `json:"status_by"`
	EventCount       int64     `json:"event_count"`
	StoredCount      int64     `json:"stored_count"`
	FirstSeen        time.Time `json:"first_seen"`
	LastSeen         time.Time `json:"last_seen"`
	FirstRelease     *string   `json:"first_release"`
	LastRelease      *string   `json:"last_release"`
	Releases         []string  `json:"releases"`          // every release the issue was seen on ("" = events without one)
	ResolvedReleases []string  `json:"resolved_releases"` // the releases at resolve time; a later event outside them is a regression
	// Ignore conditions (status ignored): back to unresolved at ignore_until,
	// when event_count reaches ignore_until_count, or when the issue
	// escalates. All unset: ignored for good.
	IgnoreUntil           *time.Time `json:"ignore_until"`
	IgnoreUntilCount      *int64     `json:"ignore_until_count"`
	IgnoreUntilEscalating bool       `json:"ignore_until_escalating"`
	CreatedAt             time.Time  `json:"created_at"`
	UpdatedAt             time.Time  `json:"updated_at"`
	Users                 *int64     `json:"users,omitempty"` // single-issue response only: distinct users in the window
	Sparkline             []int64    `json:"sparkline,omitempty"`
}

func toIssueOut(i store.Issue) issueOut {
	return issueOut{
		Fingerprint: string(i.Fingerprint), Title: i.Title, Level: string(i.Level), ErrorType: i.ErrorType, Transaction: i.Transaction,
		Platform: i.Platform, Status: string(i.Status), StatusBy: i.StatusBy, EventCount: i.EventCount, StoredCount: i.StoredCount,
		FirstSeen: i.FirstSeen.UTC(), LastSeen: i.LastSeen.UTC(),
		FirstRelease: i.FirstRelease, LastRelease: i.LastRelease, Releases: i.Releases, ResolvedReleases: i.ResolvedReleases,
		IgnoreUntil: utcPtr(i.IgnoreUntil), IgnoreUntilCount: i.IgnoreUntilCount, IgnoreUntilEscalating: i.IgnoreUntilEscalating,
		CreatedAt: i.CreatedAt.UTC(), UpdatedAt: i.UpdatedAt.UTC(),
	}
}

func utcPtr(t *time.Time) *time.Time {
	if t == nil {
		return nil
	}
	u := t.UTC()
	return &u
}

// statusChange is the body of PATCH …/issues/{fingerprint} and the status
// part of POST …/issues/bulk. With status ignored, the optional conditions
// (any may be combined; none = ignored for good): ignore_minutes (back
// after this long), ignore_events (back after this many further events),
// ignore_until_escalating (back when the issue's events in an hour reach
// the spike rule against the 24 h before now).
type statusChange struct {
	Status                string `json:"status"`
	IgnoreMinutes         *int64 `json:"ignore_minutes"`
	IgnoreEvents          *int64 `json:"ignore_events"`
	IgnoreUntilEscalating bool   `json:"ignore_until_escalating"`
}

// params validates the change; the error is the user-facing message.
func (c statusChange) params(at time.Time) (status store.IssueStatus, until *time.Time, events *int64, escalating bool, err error) {
	if !writableStatuses[c.Status] {
		return "", nil, nil, false, badRequest("status must be one of unresolved, resolved, ignored")
	}
	if c.Status != "ignored" {
		if c.IgnoreMinutes != nil || c.IgnoreEvents != nil || c.IgnoreUntilEscalating {
			return "", nil, nil, false, badRequest("ignore_* fields need status ignored")
		}
		return store.IssueStatus(c.Status), nil, nil, false, nil
	}
	if c.IgnoreMinutes != nil {
		if *c.IgnoreMinutes <= 0 || *c.IgnoreMinutes > 366*24*60*10 {
			return "", nil, nil, false, badRequest("ignore_minutes must be between 1 and ten years")
		}
		t := at.Add(time.Duration(*c.IgnoreMinutes) * time.Minute)
		until = &t
	}
	if c.IgnoreEvents != nil && (*c.IgnoreEvents <= 0 || *c.IgnoreEvents > 1_000_000_000) {
		return "", nil, nil, false, badRequest("ignore_events must be between 1 and 1000000000")
	}
	return store.IssueStatusIgnored, until, c.IgnoreEvents, c.IgnoreUntilEscalating, nil
}

type timelineBucket struct {
	Bucket time.Time `json:"bucket"`
	Events int64     `json:"events"`
}

func (h *Handler) listIssues(w http.ResponseWriter, r *http.Request) {
	p, ok := h.project(w, r)
	if !ok {
		return
	}
	q := r.URL.Query()
	from, to, err := ParseWindow(q)
	if err != nil {
		h.fail(w, err)
		return
	}
	f := store.IssueFilter{
		ProjectID: p.ID, Status: q.Get("status"), Level: q.Get("level"), Release: q.Get("release"),
		Query: q.Get("q"), Sort: q.Get("sort"), From: from, To: to,
	}
	if f.Status != "" && !issueStatuses[f.Status] {
		writeErr(w, http.StatusBadRequest, "invalid status")
		return
	}
	if f.Sort != "" && f.Sort != "last_seen" && f.Sort != "first_seen" && f.Sort != "events" {
		writeErr(w, http.StatusBadRequest, "sort must be last_seen, first_seen or events")
		return
	}
	if f.Limit, err = intParam(q, "limit", 50); err != nil {
		h.fail(w, err)
		return
	}
	if f.Offset, err = intParam(q, "offset", 0); err != nil {
		h.fail(w, err)
		return
	}
	if f.Offset > store.MaxOffset {
		h.fail(w, badRequest(fmt.Sprintf("offset must be at most %d (narrow the window instead)", store.MaxOffset)))
		return
	}
	issues, total, err := h.Store.ListIssues(r.Context(), f)
	if err != nil {
		h.fail(w, err)
		return
	}
	out := make([]issueOut, 0, len(issues))
	fps := make([]sentry.ID, 0, len(issues))
	for _, i := range issues {
		out = append(out, toIssueOut(i))
		fps = append(fps, i.Fingerprint)
	}
	if len(fps) > 0 {
		end := to.Truncate(time.Hour).Add(time.Hour)
		sp, err := store.IssueSparklines(r.Context(), h.Store.Pool, p.ID, fps, end.Add(-sparklineHours*time.Hour), end, hourly)
		if err != nil {
			h.fail(w, err)
			return
		}
		spark := map[sentry.ID][]int64{}
		for _, s := range sp {
			spark[s.Fingerprint] = s.Counts
		}
		for i := range out {
			if s := spark[sentry.ID(out[i].Fingerprint)]; s != nil {
				out[i].Sparkline = s
			} else {
				out[i].Sparkline = make([]int64, sparklineHours)
			}
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"issues": out, "total": total})
}

type issueDetailOut struct {
	issueOut
	Timeline      []timelineBucket             `json:"timeline"`
	Breakdown     map[string][]store.Breakdown `json:"breakdown"`
	LatestEventID string                       `json:"latest_event_id"`
	OldestEventID string                       `json:"oldest_event_id"`
}

// breakdownColumns are the dimensions the issue detail reports.
var breakdownColumns = []string{"release", "os_version", "device_model", "environment"}

func (h *Handler) getIssue(w http.ResponseWriter, r *http.Request) {
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
	fp, ok := sentry.ParseID(r.PathValue("fingerprint"))
	if !ok {
		writeErr(w, http.StatusNotFound, "not found")
		return
	}
	issue, err := store.GetIssue(ctx, h.Store.Pool, p.ID, fp)
	if err != nil {
		h.fail(w, err)
		return
	}
	out := issueDetailOut{issueOut: toIssueOut(issue), Breakdown: map[string][]store.Breakdown{}}

	users, err := store.IssueUsers(ctx, h.Store.Pool, p.ID, []sentry.ID{fp}, from, to)
	if err != nil {
		h.fail(w, err)
		return
	}
	if len(users) > 0 {
		out.Users = &users[0].Users
	}
	hlo := from.Truncate(time.Hour)
	tl, err := store.IssueTimeline(ctx, h.Store.Pool, p.ID, fp, hlo, to, hourly)
	if err != nil {
		h.fail(w, err)
		return
	}
	out.Timeline = make([]timelineBucket, 0, len(tl))
	for _, t := range tl {
		out.Timeline = append(out.Timeline, timelineBucket{Bucket: t.Bucket.UTC(), Events: t.Events})
	}
	ef := store.EventFilter{ProjectID: p.ID, Fingerprint: fp, From: from, To: to}
	if out.Breakdown, err = h.Store.Breakdowns(ctx, ef, breakdownColumns, 5); err != nil {
		h.fail(w, err)
		return
	}
	rng, err := store.IssueEventRange(ctx, h.Store.Pool, p.ID, fp, issue.FirstSeen, issue.LastSeen.Add(time.Second))
	if err != nil {
		h.fail(w, err)
		return
	}
	out.LatestEventID, out.OldestEventID = string(rng.Latest), string(rng.Oldest) // "" when none stored
	writeJSON(w, http.StatusOK, out)
}

func (h *Handler) updateIssue(w http.ResponseWriter, r *http.Request) {
	p, ok := h.project(w, r)
	if !ok {
		return
	}
	var in statusChange
	if err := readJSON(w, r, &in); err != nil {
		h.fail(w, err)
		return
	}
	status, until, events, escalating, err := in.params(time.Now().UTC())
	if err != nil {
		h.fail(w, err)
		return
	}
	fp, ok := sentry.ParseID(r.PathValue("fingerprint"))
	if !ok {
		writeErr(w, http.StatusNotFound, "not found")
		return
	}
	issue, err := store.SetIssueStatus(r.Context(), h.Store.Pool, store.SetIssueStatusParams{ProjectID: p.ID, Fingerprint: fp, Status: status, StatusBy: actorName(r),
		IgnoreUntil: until, IgnoreEvents: events, IgnoreEscalating: escalating})
	if err != nil {
		h.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, toIssueOut(issue))
}

func (h *Handler) bulkIssues(w http.ResponseWriter, r *http.Request) {
	p, ok := h.project(w, r)
	if !ok {
		return
	}
	var in struct {
		Fingerprints []string `json:"fingerprints"`
		statusChange
	}
	if err := readJSON(w, r, &in); err != nil {
		h.fail(w, err)
		return
	}
	status, until, events, escalating, err := in.params(time.Now().UTC())
	if err != nil {
		h.fail(w, err)
		return
	}
	if len(in.Fingerprints) == 0 {
		writeErr(w, http.StatusBadRequest, "fingerprints must not be empty")
		return
	}
	fps := make([]sentry.ID, 0, len(in.Fingerprints))
	for _, s := range in.Fingerprints {
		fp, ok := sentry.ParseID(s)
		if !ok {
			h.fail(w, badRequest("fingerprints must be 32-hex ids"))
			return
		}
		fps = append(fps, fp)
	}
	n, err := store.SetIssuesStatus(r.Context(), h.Store.Pool, p.ID, fps, store.SetIssueStatusParams{Status: status, StatusBy: actorName(r),
		IgnoreUntil: until, IgnoreEvents: events, IgnoreEscalating: escalating})
	if err != nil {
		h.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"updated": n, "status": in.Status})
}

// intParam reads a non-negative integer query parameter.
func intParam(q map[string][]string, key string, def int) (int, error) {
	v := ""
	if vs := q[key]; len(vs) > 0 {
		v = vs[0]
	}
	if v == "" {
		return def, nil
	}
	n, err := strconv.Atoi(v)
	if err != nil || n < 0 {
		return 0, badRequest(key + " must be a non-negative integer")
	}
	return n, nil
}

// actorName is who is acting (the API key's name), for issues.status_by.
func actorName(r *http.Request) *string {
	if a := auth.ActorFrom(r.Context()); a.Name != "" {
		return &a.Name
	}
	return nil
}
