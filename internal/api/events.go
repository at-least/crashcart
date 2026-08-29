package api

import (
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/crashcartapp/crashcart/internal/db/sqlc"
	"github.com/crashcartapp/crashcart/internal/pk"
	"github.com/crashcartapp/crashcart/internal/store"
)

type eventListItem struct {
	store.EventRow
	Time time.Time `json:"time"`
}

type eventDetailOut struct {
	sqlc.Event
	Time time.Time `json:"time"`
}

// EventFilterFromQuery builds the event filter from query parameters:
// exact-match columns, `q` (message search), `crash=1`, `before` cursor,
// `limit`, and `tag.<key>=value`. The time window comes from ParseWindow
// unless neither days/from/to is given, in which case it is unbounded
// (cursor pagination over all retained events).
func EventFilterFromQuery(projectID int64, q map[string][]string) (store.EventFilter, error) {
	get := func(k string) string {
		if vs := q[k]; len(vs) > 0 {
			return strings.TrimSpace(vs[0])
		}
		return ""
	}
	f := store.EventFilter{
		ProjectID: projectID, Level: get("level"), Release: get("release"), Environment: get("environment"),
		Platform: get("platform"), ErrorType: get("error_type"), UserID: get("user_id"), DeviceID: get("device_id"),
		DeviceModel: get("device_model"), OSVersion: get("os_version"), Screen: get("screen"),
		Fingerprint: get("fingerprint"), Location: get("error_location"), Query: get("q"),
	}
	if c := get("crash"); c == "1" || c == "true" {
		f.Crash = true
	}
	if b := get("before"); b != "" {
		n, err := strconv.ParseInt(b, 10, 64)
		if err != nil || n <= 0 {
			return f, badRequest("before must be an event id")
		}
		f.Before = n
	}
	var err error
	if f.Limit, err = intParam(q, "limit", 50); err != nil {
		return f, err
	}
	if get("days") != "" || get("from") != "" || get("to") != "" {
		from, to, err := ParseWindow(q)
		if err != nil {
			return f, err
		}
		f.From, f.To = pk.Lower(from), pk.Upper(to)
	}
	for k, vs := range q {
		if key, ok := strings.CutPrefix(k, "tag."); ok && key != "" && len(vs) > 0 && vs[0] != "" {
			if f.Tags == nil {
				f.Tags = map[string]string{}
			}
			f.Tags[key] = vs[0]
		}
	}
	return f, nil
}

func (h *Handler) listEvents(w http.ResponseWriter, r *http.Request) {
	p, ok := h.project(w, r)
	if !ok {
		return
	}
	f, err := EventFilterFromQuery(p.ID, r.URL.Query())
	if err != nil {
		h.fail(w, err)
		return
	}
	rows, more, err := h.Store.ListEvents(r.Context(), f)
	if err != nil {
		h.fail(w, err)
		return
	}
	out := make([]eventListItem, 0, len(rows))
	for _, e := range rows {
		out = append(out, eventListItem{EventRow: e, Time: pk.Time(e.ID)})
	}
	var next *int64
	if more && len(rows) > 0 {
		id := rows[len(rows)-1].ID
		next = &id
	}
	writeJSON(w, http.StatusOK, map[string]any{"events": out, "more": more, "next_before": next})
}

func (h *Handler) getEvent(w http.ResponseWriter, r *http.Request) {
	p, ok := h.project(w, r)
	if !ok {
		return
	}
	ref := r.PathValue("id")
	var ev sqlc.Event
	var err error
	if id, perr := strconv.ParseInt(ref, 10, 64); perr == nil {
		ev, err = h.Store.GetEvent(r.Context(), sqlc.GetEventParams{ProjectID: p.ID, ID: id})
	} else {
		ev, err = h.Store.GetEventByEventID(r.Context(), sqlc.GetEventByEventIDParams{ProjectID: p.ID, EventID: ref})
	}
	if err != nil {
		h.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, eventDetailOut{Event: ev, Time: pk.Time(ev.ID)})
}
