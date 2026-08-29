package api

import (
	"encoding/json"
	"github.com/crashcartapp/crashcart/internal/sentry"
	"net/http"
	"strings"
	"time"

	"github.com/crashcartapp/crashcart/internal/db/sqlc"
	"github.com/crashcartapp/crashcart/internal/store"
)

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
		c, ok := store.ParseCursor(b)
		if !ok {
			return f, badRequest("before must be a cursor from next_before")
		}
		f.Before = c
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
		f.From, f.To = from, to
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
	var next *string
	if more && len(rows) > 0 {
		c := store.CursorOf(rows[len(rows)-1]).String()
		next = &c
	}
	writeJSON(w, http.StatusOK, map[string]any{"events": rows, "more": more, "next_before": next})
}

func (h *Handler) getEvent(w http.ResponseWriter, r *http.Request) {
	p, ok := h.project(w, r)
	if !ok {
		return
	}
	ev, err := h.Store.GetEvent(r.Context(), sqlc.GetEventParams{ProjectID: p.ID, EventID: r.PathValue("id")})
	if err != nil {
		h.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, eventDetail{Event: ev, Payload: json.RawMessage(ev.Payload), Breadcrumbs: breadcrumbsOf(ev)})
}

// eventDetail is the JSON API's event: the row, with the raw payload
// embedded as JSON (the column holds its bytes) and the breadcrumbs
// (newest last, at most 20) read from it.
type eventDetail struct {
	sqlc.Event
	Payload     json.RawMessage     `json:"payload"`
	Breadcrumbs []sentry.Breadcrumb `json:"breadcrumbs"`
}

func breadcrumbsOf(ev sqlc.Event) []sentry.Breadcrumb {
	parsed := sentry.ParseEvent(ev.EventID, ev.OccurredAt, ev.Payload, time.Now().UTC())
	if parsed == nil || parsed.Breadcrumbs == nil {
		return []sentry.Breadcrumb{}
	}
	return parsed.Breadcrumbs
}
