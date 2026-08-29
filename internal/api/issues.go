package api

import (
	"net/http"
	"strconv"
	"time"

	"github.com/crashcartapp/crashcart/internal/db/sqlc"
	"github.com/crashcartapp/crashcart/internal/store"
)

// sparklineHours is the length of the per-issue sparkline (7 days, hourly).
const sparklineHours = 7 * 24

// issueStatuses is the lifecycle allowlist.
var issueStatuses = map[string]bool{"unresolved": true, "triaged": true, "resolved": true, "ignored": true, "regression": true}

// issueOut is the JSON shape of an issue.
type issueOut struct {
	Fingerprint     string    `json:"fingerprint"`
	Title           string    `json:"title"`
	Level           string    `json:"level"`
	ErrorType       *string   `json:"error_type"`
	Screen          *string   `json:"screen"`
	Platform        *string   `json:"platform"`
	Status          string    `json:"status"`
	EventCount      int64     `json:"event_count"`
	StoredCount     int64     `json:"stored_count"`
	FirstSeen       time.Time `json:"first_seen"`
	LastSeen        time.Time `json:"last_seen"`
	FirstRelease    *string   `json:"first_release"`
	LastRelease     *string   `json:"last_release"`
	ResolvedRelease *string   `json:"resolved_release"`
	CreatedAt       time.Time `json:"created_at"`
	UpdatedAt       time.Time `json:"updated_at"`
	Users           int64     `json:"users"`
	Sparkline       []int64   `json:"sparkline,omitempty"`
}

func toIssueOut(i sqlc.Issue) issueOut {
	return issueOut{
		Fingerprint: i.Fingerprint, Title: i.Title, Level: i.Level, ErrorType: i.ErrorType, Screen: i.Screen,
		Platform: i.Platform, Status: i.Status, EventCount: i.EventCount, StoredCount: i.StoredCount,
		FirstSeen: i.FirstSeen.UTC(), LastSeen: i.LastSeen.UTC(),
		FirstRelease: i.FirstRelease, LastRelease: i.LastRelease, ResolvedRelease: i.ResolvedRelease,
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
	issues, total, err := h.Store.ListIssues(r.Context(), f)
	if err != nil {
		h.fail(w, err)
		return
	}
	out := make([]issueOut, 0, len(issues))
	fps := make([]string, 0, len(issues))
	for _, i := range issues {
		out = append(out, toIssueOut(i))
		fps = append(fps, i.Fingerprint)
	}
	if len(fps) > 0 {
		users, err := h.Store.IssueUsers(r.Context(), sqlc.IssueUsersParams{ProjectID: p.ID, Column2: fps, OccurredAt: from, OccurredAt_2: to})
		if err != nil {
			h.fail(w, err)
			return
		}
		byFP := map[string]int64{}
		for _, u := range users {
			byFP[deref(u.Fingerprint)] = u.Users
		}
		start := to.Truncate(time.Hour).Add(-(sparklineHours - 1) * time.Hour)
		sp, err := h.Store.IssueSparklines(r.Context(), sqlc.IssueSparklinesParams{ProjectID: p.ID, Column2: fps, Bucket: start})
		if err != nil {
			h.fail(w, err)
			return
		}
		spark := map[string][]int64{}
		for _, s := range sp {
			idx := int(s.Bucket.Sub(start) / time.Hour)
			if idx < 0 || idx >= sparklineHours {
				continue
			}
			if spark[s.Fingerprint] == nil {
				spark[s.Fingerprint] = make([]int64, sparklineHours)
			}
			spark[s.Fingerprint][idx] += s.Events
		}
		for i := range out {
			out[i].Users = byFP[out[i].Fingerprint]
			if s := spark[out[i].Fingerprint]; s != nil {
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
	fp := r.PathValue("fingerprint")
	issue, err := h.Store.GetIssue(ctx, sqlc.GetIssueParams{ProjectID: p.ID, Fingerprint: fp})
	if err != nil {
		h.fail(w, err)
		return
	}
	out := issueDetailOut{issueOut: toIssueOut(issue), Breakdown: map[string][]store.Breakdown{}}

	users, err := h.Store.IssueUsers(ctx, sqlc.IssueUsersParams{ProjectID: p.ID, Column2: []string{fp}, OccurredAt: from, OccurredAt_2: to})
	if err != nil {
		h.fail(w, err)
		return
	}
	if len(users) > 0 {
		out.Users = users[0].Users
	}
	hlo := from.Truncate(time.Hour)
	tl, err := h.Store.IssueTimeline(ctx, sqlc.IssueTimelineParams{ProjectID: p.ID, Fingerprint: fp, Bucket: hlo, Bucket_2: to})
	if err != nil {
		h.fail(w, err)
		return
	}
	byBucket := map[int64]int64{}
	for _, t := range tl {
		byBucket[t.Bucket.Unix()] += t.Events
	}
	out.Timeline = []timelineBucket{}
	for b := hlo; b.Before(to); b = b.Add(time.Hour) {
		out.Timeline = append(out.Timeline, timelineBucket{Bucket: b, Events: byBucket[b.Unix()]})
	}
	ef := store.EventFilter{ProjectID: p.ID, Fingerprint: fp, From: from, To: to}
	for _, col := range breakdownColumns {
		bd, err := h.Store.Breakdown(ctx, ef, col, 5)
		if err != nil {
			h.fail(w, err)
			return
		}
		out.Breakdown[col] = bd
	}
	rng, err := h.Store.IssueEventRange(ctx, sqlc.IssueEventRangeParams{ProjectID: p.ID, Fingerprint: fp})
	if err != nil {
		h.fail(w, err)
		return
	}
	out.LatestEventID, out.OldestEventID = rng.Latest, rng.Oldest
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
	if !issueStatuses[in.Status] {
		writeErr(w, http.StatusBadRequest, "status must be one of unresolved, triaged, resolved, ignored, regression")
		return
	}
	issue, err := h.Store.SetIssueStatus(r.Context(), sqlc.SetIssueStatusParams{ProjectID: p.ID, Fingerprint: r.PathValue("fingerprint"), Status: in.Status})
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
	if !issueStatuses[in.Status] {
		writeErr(w, http.StatusBadRequest, "status must be one of unresolved, triaged, resolved, ignored, regression")
		return
	}
	if len(in.Fingerprints) == 0 {
		writeErr(w, http.StatusBadRequest, "fingerprints must not be empty")
		return
	}
	n, err := h.Store.SetIssuesStatus(r.Context(), sqlc.SetIssuesStatusParams{ProjectID: p.ID, Column2: in.Fingerprints, Status: in.Status})
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
