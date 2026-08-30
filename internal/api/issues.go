package api

import (
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/crashcartapp/crashcart/internal/auth"
	"github.com/crashcartapp/crashcart/internal/db/sqlc"
	"github.com/crashcartapp/crashcart/internal/sentry"
	"github.com/crashcartapp/crashcart/internal/store"
)

// sparklineHours is the length of the per-issue sparkline (7 days, hourly).
const sparklineHours = 7 * 24

// issueStatuses is the lifecycle allowlist for filters; writableStatuses
// is what a client may set — "regression" is ingest's verdict (an event
// on a release outside resolved_releases), never a request's.
var (
	issueStatuses    = map[string]bool{"unresolved": true, "triaged": true, "resolved": true, "ignored": true, "regression": true}
	writableStatuses = map[string]bool{"unresolved": true, "triaged": true, "resolved": true, "ignored": true}
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
	CreatedAt        time.Time `json:"created_at"`
	UpdatedAt        time.Time `json:"updated_at"`
	Users            *int64    `json:"users,omitempty"` // single-issue response only: distinct users in the window
	Sparkline        []int64   `json:"sparkline,omitempty"`
}

func toIssueOut(i sqlc.Issue) issueOut {
	return issueOut{
		Fingerprint: string(i.Fingerprint), Title: i.Title, Level: string(i.Level), ErrorType: i.ErrorType, Transaction: i.Transaction,
		Platform: i.Platform, Status: string(i.Status), StatusBy: i.StatusBy, EventCount: i.EventCount, StoredCount: i.StoredCount,
		FirstSeen: i.FirstSeen.UTC(), LastSeen: i.LastSeen.UTC(),
		FirstRelease: i.FirstRelease, LastRelease: i.LastRelease, Releases: i.Releases, ResolvedReleases: i.ResolvedReleases,
		CreatedAt: i.CreatedAt.UTC(), UpdatedAt: i.UpdatedAt.UTC(),
	}
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
		sp, err := h.Store.IssueSparklines(r.Context(), sqlc.IssueSparklinesParams{
			ProjectID: p.ID, Fingerprints: fps, FromAt: end.Add(-sparklineHours * time.Hour), ToAt: end, Width: hourly,
		})
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
	issue, err := h.Store.GetIssue(ctx, sqlc.GetIssueParams{ProjectID: p.ID, Fingerprint: fp})
	if err != nil {
		h.fail(w, err)
		return
	}
	out := issueDetailOut{issueOut: toIssueOut(issue), Breakdown: map[string][]store.Breakdown{}}

	users, err := h.Store.IssueUsers(ctx, sqlc.IssueUsersParams{ProjectID: p.ID, Column2: []sentry.ID{fp}, OccurredAt: from, OccurredAt_2: to})
	if err != nil {
		h.fail(w, err)
		return
	}
	if len(users) > 0 {
		out.Users = &users[0].Users
	}
	hlo := from.Truncate(time.Hour)
	tl, err := h.Store.IssueTimeline(ctx, sqlc.IssueTimelineParams{ProjectID: p.ID, Fingerprint: fp, FromAt: hlo, ToAt: to, Width: hourly})
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
	rng, err := h.Store.IssueEventRange(ctx, sqlc.IssueEventRangeParams{ProjectID: p.ID, Fingerprint: fp, FromAt: issue.FirstSeen, ToAt: issue.LastSeen.Add(time.Second)})
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
	var in struct {
		Status string `json:"status"`
	}
	if err := readJSON(w, r, &in); err != nil {
		h.fail(w, err)
		return
	}
	if !writableStatuses[in.Status] {
		writeErr(w, http.StatusBadRequest, "status must be one of unresolved, triaged, resolved, ignored")
		return
	}
	fp, ok := sentry.ParseID(r.PathValue("fingerprint"))
	if !ok {
		writeErr(w, http.StatusNotFound, "not found")
		return
	}
	issue, err := h.Store.SetIssueStatus(r.Context(), sqlc.SetIssueStatusParams{ProjectID: p.ID, Fingerprint: fp, Status: sqlc.IssueStatus(in.Status), StatusBy: actorName(r)})
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
		Status       string   `json:"status"`
	}
	if err := readJSON(w, r, &in); err != nil {
		h.fail(w, err)
		return
	}
	if !writableStatuses[in.Status] {
		writeErr(w, http.StatusBadRequest, "status must be one of unresolved, triaged, resolved, ignored")
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
	n, err := h.Store.SetIssuesStatus(r.Context(), sqlc.SetIssuesStatusParams{ProjectID: p.ID, Column2: fps, Status: sqlc.IssueStatus(in.Status), StatusBy: actorName(r)})
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
