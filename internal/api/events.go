package api

import (
	"encoding/json"
	"mime"
	"net/http"
	"net/url"
	"strconv"
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
	atts, err := h.attachmentsOf(r, p.Slug, ev)
	if err != nil {
		h.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, eventDetail{Event: ev, Payload: json.RawMessage(payload), Breadcrumbs: breadcrumbsOf(ev, payload), Attachments: atts})
}

// eventDetail is the JSON API's event: the row, with the raw payload
// embedded as JSON (null when it has none), the breadcrumbs (newest
// last, at most 20) read from it, and its attachments (metadata; the
// bytes are at each one's url).
type eventDetail struct {
	sqlc.Event
	Payload     json.RawMessage     `json:"payload"`
	Breadcrumbs []sentry.Breadcrumb `json:"breadcrumbs"`
	Attachments []attachmentOut     `json:"attachments"`
}

type attachmentOut struct {
	N              int32  `json:"n"`
	Filename       string `json:"filename"`
	ContentType    string `json:"content_type"`
	AttachmentType string `json:"attachment_type"`
	Size           int64  `json:"size"`
	URL            string `json:"url"`
}

func (h *Handler) attachmentsOf(r *http.Request, slug string, ev sqlc.Event) ([]attachmentOut, error) {
	rows, err := h.Store.ListAttachments(r.Context(), sqlc.ListAttachmentsParams{ProjectID: ev.ProjectID, EventID: ev.EventID, OccurredAt: ev.OccurredAt})
	if err != nil {
		return nil, err
	}
	out := make([]attachmentOut, 0, len(rows))
	for _, a := range rows {
		out = append(out, attachmentOut{N: a.N, Filename: a.Filename, ContentType: a.ContentType, AttachmentType: a.AttachmentType, Size: a.Size,
			URL: "/api/projects/" + url.PathEscape(slug) + "/events/" + string(ev.EventID) + "/attachments/" + strconv.Itoa(int(a.N))})
	}
	return out, nil
}

// getAttachment is GET /api/projects/{slug}/events/{id}/attachments/{n}: the bytes.
func (h *Handler) getAttachment(w http.ResponseWriter, r *http.Request) {
	p, ok := h.project(w, r)
	if !ok {
		return
	}
	id, ok := sentry.ParseID(r.PathValue("id"))
	n, err := strconv.Atoi(r.PathValue("n"))
	if !ok || err != nil || n < 0 {
		writeErr(w, http.StatusNotFound, "not found")
		return
	}
	ev, err := h.Store.GetEvent(r.Context(), sqlc.GetEventParams{ProjectID: p.ID, EventID: id})
	if err != nil {
		h.fail(w, err)
		return
	}
	a, err := h.Store.GetAttachment(r.Context(), sqlc.GetAttachmentParams{ProjectID: p.ID, EventID: id, OccurredAt: ev.OccurredAt, N: int32(n)})
	if err != nil {
		h.fail(w, err)
		return
	}
	ServeAttachment(w, a)
}

// inlineImages are the content types a browser may render inline; a
// PNG / JPEG cannot carry script. Everything else is served as a download
// under application/octet-stream — an attachment is SDK-supplied bytes,
// and text/html or SVG shown inline on this origin would run in the
// viewer's session.
var inlineImages = map[string]bool{"image/png": true, "image/jpeg": true, "image/gif": true, "image/webp": true}

// InlineImage reports whether an attachment of this content type is shown
// inline (an <img>) rather than offered as a download.
func InlineImage(contentType string) bool { return inlineImages[strings.ToLower(contentType)] }

// ServeAttachment writes an attachment's bytes: images inline under their
// own type, anything else as an octet-stream download; never sniffed.
func ServeAttachment(w http.ResponseWriter, a sqlc.Attachment) {
	disposition, ctype := "attachment", "application/octet-stream"
	if InlineImage(a.ContentType) {
		disposition, ctype = "inline", strings.ToLower(a.ContentType)
	}
	w.Header().Set("Content-Type", ctype)
	w.Header().Set("Content-Disposition", mime.FormatMediaType(disposition, map[string]string{"filename": a.Filename}))
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Content-Security-Policy", "default-src 'none'; sandbox")
	w.Header().Set("Cache-Control", "private, max-age=3600")
	w.Header().Set("Content-Length", strconv.Itoa(len(a.Data)))
	w.Write(a.Data)
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
