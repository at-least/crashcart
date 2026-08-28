// Package api serves the JSON API under /api and the Sentry ingest endpoint.
package api

import (
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/newlix/crashcart/internal/config"
	"github.com/newlix/crashcart/internal/ingest"
	"github.com/newlix/crashcart/internal/store"
	"github.com/newlix/crashcart/internal/symbolicate"
	"github.com/newlix/crashcart/internal/timerange"
)

// MaxEnvelopeBytes caps an ingest body.
const MaxEnvelopeBytes = 2 << 20

// Handler holds the dependencies of every API route.
type Handler struct {
	Store    *store.Store
	Ingester *ingest.Ingester
	Config   config.Config
	DSYM     *symbolicate.DSYMClient
	Log      *slog.Logger
	Now      func() time.Time
}

// Register mounts routes on mux. `apiMW` wraps /api/* routes (auth + rate
// limit), `ingestMW` wraps the ingest routes.
func (h *Handler) Register(mux *http.ServeMux, apiMW, ingestMW func(http.Handler) http.Handler) {
	api := func(pattern string, fn http.HandlerFunc) {
		mux.Handle(pattern, apiMW(fn))
	}
	// Sentry SDKs post to {DSN origin}/api/{project}/envelope/ — both forms work.
	mux.Handle("POST /ingest", ingestMW(http.HandlerFunc(h.Ingest)))
	mux.Handle("POST /api/{project}/envelope/", ingestMW(http.HandlerFunc(h.Ingest)))
	mux.Handle("POST /api/{project}/envelope", ingestMW(http.HandlerFunc(h.Ingest)))
	mux.Handle("POST /api/{project}/store/", ingestMW(http.HandlerFunc(h.Store_)))

	api("GET /api/events", h.ListEvents)
	api("GET /api/events/{id}", h.GetEvent)
	api("GET /api/stats", h.Stats)
	api("GET /api/stats/timeline", h.Timeline)
	api("GET /api/stats/volume", h.Volume)
	api("GET /api/stats/releases", h.Releases)
	api("GET /api/stats/release_versions", h.ReleaseVersions)
	api("GET /api/stats/release_health", h.ReleaseHealth)
	api("GET /api/issues", h.ListIssues)
	api("GET /api/issues/{fingerprint}", h.GetIssue)
	api("PATCH /api/issues/{fingerprint}", h.UpdateIssue)
	api("GET /api/alerts", h.ListAlerts)
	api("GET /api/alerts/channels", h.Channels)
	api("PATCH /api/alerts/{type}", h.ToggleAlert)
	api("GET /api/symbols", h.ListSymbols)
	api("POST /api/symbols", h.UploadSymbol)
	api("POST /api/symbolicate", h.Symbolicate)
	api("/api/", http.NotFound)
}

func (h *Handler) now() time.Time {
	if h.Now != nil {
		return h.Now()
	}
	return time.Now()
}

// ── helpers ─────────────────────────────────────────────────

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}

func (h *Handler) fail(w http.ResponseWriter, r *http.Request, err error) {
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, "not found")
		return
	}
	h.Log.Error("request failed", "method", r.Method, "path", r.URL.Path, "err", err)
	writeError(w, http.StatusInternalServerError, "internal server error")
}

func readJSON(r *http.Request, v any) error {
	dec := json.NewDecoder(io.LimitReader(r.Body, 1<<20))
	dec.DisallowUnknownFields()
	return dec.Decode(v)
}

// ── ingest ──────────────────────────────────────────────────

// Ingest accepts a Sentry envelope.
func (h *Handler) Ingest(w http.ResponseWriter, r *http.Request) {
	if r.ContentLength > MaxEnvelopeBytes {
		http.Error(w, "payload too large", http.StatusRequestEntityTooLarge)
		return
	}
	body, err := readBody(r)
	if err != nil {
		if errors.Is(err, errTooLarge) {
			http.Error(w, "payload too large", http.StatusRequestEntityTooLarge)
		} else {
			http.Error(w, "unreadable body", http.StatusBadRequest)
		}
		return
	}
	res, err := h.Ingester.Ingest(r.Context(), body)
	switch {
	case errors.Is(err, ingest.ErrEmpty):
		http.Error(w, "no events in envelope", http.StatusBadRequest)
	case errors.Is(err, ingest.ErrTooManyEvents):
		http.Error(w, "too many events in envelope", http.StatusRequestEntityTooLarge)
	case err != nil:
		h.Log.Error("ingest failed", "err", err)
		http.Error(w, "storage failed", http.StatusInternalServerError)
	default:
		writeJSON(w, http.StatusOK, map[string]int{"events": res.Events, "sessions": res.Sessions, "dropped": res.Dropped})
	}
}

// Store_ accepts the legacy /store/ endpoint (a bare event JSON) by wrapping
// it into a one-item envelope.
func (h *Handler) Store_(w http.ResponseWriter, r *http.Request) {
	body, err := readBody(r)
	if err != nil {
		http.Error(w, "unreadable body", http.StatusBadRequest)
		return
	}
	body = append([]byte("{}\n{\"type\":\"event\"}\n"), body...)
	r2 := r.Clone(r.Context())
	r2.Body = io.NopCloser(strings.NewReader(string(body)))
	r2.ContentLength = int64(len(body))
	h.Ingest(w, r2)
}

var errTooLarge = errors.New("body too large")

func readBody(r *http.Request) ([]byte, error) {
	var reader io.Reader = r.Body
	if enc := r.Header.Get("Content-Encoding"); enc != "" {
		dr, err := decompress(enc, r.Body)
		if err != nil {
			return nil, err
		}
		reader = dr
	}
	b, err := io.ReadAll(io.LimitReader(reader, MaxEnvelopeBytes+1))
	if err != nil {
		return nil, err
	}
	if len(b) > MaxEnvelopeBytes {
		return nil, errTooLarge
	}
	return b, nil
}

// ── events ──────────────────────────────────────────────────

func eventFilter(q url.Values, now time.Time) store.EventFilter {
	f := store.EventFilter{
		DeviceID:      q.Get("device_id"),
		UserID:        q.Get("user_id"),
		Platform:      q.Get("platform"),
		Release:       q.Get("release"),
		ErrorType:     q.Get("error_type"),
		Fingerprint:   q.Get("fingerprint"),
		CrashesOnly:   q.Get("crash") == "1" || q.Get("crash") == "true",
		DeviceModel:   q.Get("device_model"),
		OSVersion:     q.Get("os_version"),
		ErrorLocation: q.Get("error_location"),
		Query:         q.Get("q"),
		Limit:         timerange.ClampInt(q.Get("limit"), 1, 200, 50),
		Offset:        timerange.ClampInt(q.Get("offset"), 0, 1<<30, 0),
	}
	if lv := q.Get("level"); lv != "" {
		for _, l := range strings.Split(lv, ",") {
			if l = strings.TrimSpace(l); l != "" {
				f.Levels = append(f.Levels, l)
			}
		}
	}
	if q.Get("from") != "" || q.Get("to") != "" || q.Get("days") != "" {
		f.Range = timerange.Parse(q, 7, now)
		f.HasRange = true
	}
	for k, vs := range q {
		if key, ok := strings.CutPrefix(k, "tag."); ok && key != "" && len(vs) > 0 && vs[0] != "" {
			if f.Tags == nil {
				f.Tags = map[string]string{}
			}
			f.Tags[key] = vs[0]
		}
	}
	return f
}

// ListEvents is GET /api/events.
func (h *Handler) ListEvents(w http.ResponseWriter, r *http.Request) {
	rows, err := h.Store.ListEvents(r.Context(), eventFilter(r.URL.Query(), h.now()))
	if err != nil {
		h.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, rows)
}

// GetEvent is GET /api/events/{id}.
func (h *Handler) GetEvent(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid id")
		return
	}
	ev, err := h.Store.GetEvent(r.Context(), id)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, ev)
}

// ── stats ───────────────────────────────────────────────────

func (h *Handler) Stats(w http.ResponseWriter, r *http.Request) {
	st, err := h.Store.Stats(r.Context(), timerange.Parse(r.URL.Query(), 7, h.now()))
	if err != nil {
		h.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, st)
}

func hourly(q url.Values) bool { return q.Get("hourly") == "1" || q.Get("hourly") == "true" }

func (h *Handler) Timeline(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	pts, err := h.Store.CrashTimeline(r.Context(), timerange.Parse(q, 7, h.now()), hourly(q))
	if err != nil {
		h.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, pts)
}

func (h *Handler) Volume(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	pts, err := h.Store.Volume(r.Context(), timerange.Parse(q, 7, h.now()), hourly(q))
	if err != nil {
		h.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, pts)
}

func (h *Handler) Releases(w http.ResponseWriter, r *http.Request) {
	rows, err := h.Store.Releases(r.Context())
	if err != nil {
		h.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, rows)
}

func (h *Handler) ReleaseVersions(w http.ResponseWriter, r *http.Request) {
	rows, err := h.Store.ReleaseVersions(r.Context(), timerange.Parse(r.URL.Query(), 30, h.now()))
	if err != nil {
		h.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, rows)
}

func (h *Handler) ReleaseHealth(w http.ResponseWriter, r *http.Request) {
	rows, err := h.Store.ReleaseHealth(r.Context(), timerange.Parse(r.URL.Query(), 7, h.now()))
	if err != nil {
		h.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, rows)
}

// ── issues ──────────────────────────────────────────────────

func (h *Handler) ListIssues(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	rows, err := h.Store.ListIssues(r.Context(), store.IssueFilter{
		Range:       timerange.Parse(q, 7, h.now()),
		ErrorType:   q.Get("error_type"),
		Status:      q.Get("status"),
		Release:     q.Get("release"),
		UserID:      q.Get("user_id"),
		DeviceID:    q.Get("device_id"),
		DeviceModel: q.Get("device_model"),
		OSVersion:   q.Get("os_version"),
		Limit:       timerange.ClampInt(q.Get("limit"), 1, 100, 50),
	})
	if err != nil {
		h.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, rows)
}

func (h *Handler) GetIssue(w http.ResponseWriter, r *http.Request) {
	is, err := h.Store.GetIssue(r.Context(), r.PathValue("fingerprint"))
	if err != nil {
		h.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, is)
}

func (h *Handler) UpdateIssue(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Status string `json:"status"`
	}
	if err := readJSON(r, &body); err != nil || body.Status == "" {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	valid := false
	for _, s := range store.ValidIssueStatuses {
		valid = valid || s == body.Status
	}
	if !valid {
		writeError(w, http.StatusBadRequest, "invalid status")
		return
	}
	fp := r.PathValue("fingerprint")
	if err := h.Store.UpdateIssueStatus(r.Context(), fp, body.Status); err != nil {
		h.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"fingerprint": fp, "status": body.Status})
}

// ── alerts ──────────────────────────────────────────────────

func (h *Handler) ListAlerts(w http.ResponseWriter, r *http.Request) {
	rows, err := h.Store.ListAlertTypes(r.Context())
	if err != nil {
		h.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, rows)
}

// ChannelsInfo describes the configured notification channels.
type ChannelsInfo struct {
	Telegram           []string `json:"telegram"`
	Webhooks           []string `json:"webhooks"`
	Emails             []string `json:"emails"`
	TelegramConfigured bool     `json:"telegram_configured"`
	EmailConfigured    bool     `json:"email_configured"`
}

// Channels reports which channels are configured (secrets masked).
func (h *Handler) Channels(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, h.ChannelsInfo())
}

// ChannelsInfo builds the channel summary from config.
func (h *Handler) ChannelsInfo() ChannelsInfo {
	c := h.Config
	info := ChannelsInfo{
		Telegram:           nonNil(c.TelegramChatIDs),
		Webhooks:           []string{},
		Emails:             nonNil(c.AlertEmails),
		TelegramConfigured: c.TelegramBotToken != "",
		EmailConfigured:    c.EmailFrom != "" && c.SMTPAddr != "",
	}
	for _, u := range c.AlertWebhooks {
		info.Webhooks = append(info.Webhooks, maskURL(u))
	}
	return info
}

func nonNil(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}

func maskURL(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		if len(raw) > 20 {
			return raw[:20] + "..."
		}
		return raw
	}
	p := u.Path
	if len(p) > 20 {
		p = p[:8] + "..." + p[len(p)-6:]
	}
	return u.Host + p
}

func (h *Handler) ToggleAlert(w http.ResponseWriter, r *http.Request) {
	typ := r.PathValue("type")
	valid := false
	for _, t := range store.ValidAlertTypes {
		valid = valid || t == typ
	}
	if !valid {
		writeError(w, http.StatusBadRequest, "invalid alert type")
		return
	}
	var body struct {
		Enabled *bool `json:"enabled"`
	}
	if err := readJSON(r, &body); err != nil || body.Enabled == nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if err := h.Store.SetAlertEnabled(r.Context(), typ, *body.Enabled); err != nil {
		h.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"type": typ, "enabled": *body.Enabled})
}

// ── health ──────────────────────────────────────────────────

// Health is GET /health.
func (h *Handler) Health(w http.ResponseWriter, r *http.Request) {
	if err := h.Store.Ping(r.Context()); err != nil {
		h.Log.Error("health check failed", "err", err)
		http.Error(w, "unhealthy", http.StatusServiceUnavailable)
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	_, _ = w.Write([]byte("ok"))
}
