package api

import (
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"github.com/crashcartapp/crashcart/internal/db/sqlc"
	"github.com/crashcartapp/crashcart/internal/sentry"
	"github.com/crashcartapp/crashcart/internal/store"
)

// EventFilterFromQuery builds the event filter from query parameters:
// exact-match columns, `q` (message search), `handled=true|false`, `before` cursor,
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
		DeviceModel: get("device_model"), OSVersion: get("os_version"), Transaction: get("transaction"),
		Location: get("culprit"), Query: get("q"),
	}
	switch h := strings.ToLower(get("handled")); h {
	case "", "true", "false":
		f.Handled = h
	case "yes", "no": // Sentry's handled tag values
		f.Handled = map[string]string{"yes": "true", "no": "false"}[h]
	default:
		return f, badRequest("handled must be true or false")
	}
	if v := get("fingerprint"); v != "" {
		fp, ok := sentry.ParseID(v)
		if !ok {
			return f, badRequest("fingerprint must be a 32-hex id")
		}
		f.Fingerprint = fp
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
	id, ok := sentry.ParseID(r.PathValue("id"))
	if !ok {
		writeErr(w, http.StatusNotFound, "not found")
		return
	}
	ev, err := h.Store.GetEvent(r.Context(), sqlc.GetEventParams{ProjectID: p.ID, EventID: id})
	if err != nil {
		h.fail(w, err)
		return
	}
	ev.OccurredAt = ev.OccurredAt.UTC()
	payload, err := store.Payload(ev)
	if err != nil {
		h.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, eventDetail{Event: ev, Payload: json.RawMessage(payload), Breadcrumbs: breadcrumbsOf(ev, payload)})
}

// eventDetail is the JSON API's event: the row, with the raw payload
// embedded as JSON (null when it has none) and the breadcrumbs (newest
// last, at most 20) read from it.
type eventDetail struct {
	sqlc.Event
	Payload     json.RawMessage     `json:"payload"`
	Breadcrumbs []sentry.Breadcrumb `json:"breadcrumbs"`
}

func breadcrumbsOf(ev sqlc.Event, payload []byte) []sentry.Breadcrumb {
	if len(payload) == 0 {
		return []sentry.Breadcrumb{}
	}
	parsed := sentry.ParseEvent(string(ev.EventID), ev.OccurredAt, payload, time.Now().UTC())
	if parsed == nil || parsed.Breadcrumbs == nil {
		return []sentry.Breadcrumb{}
	}
	return parsed.Breadcrumbs
}
