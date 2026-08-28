// Package web is the server-rendered viewer: templ components + htmx.
//
//	/                     platform portal (DEPLOYMENTS set) or the dashboard
//	/dashboard            log explorer; state lives in the URL query
//	/:slug/dashboard      same, for one deployment of a multi-instance portal
//	/events/:id/detail    detail sheet fragment
//	/settings             alert toggles + channels
//	PATCH /settings/alerts/:type   toggle (htmx only — CSRF guard)
//	/static/*             embedded assets
//
// HTML routes are unauthenticated (rate-limited upstream); they read the
// store in-process, never through the JSON API.
package web

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/a-h/templ"

	"github.com/newlix/crashcart/internal/api"
	"github.com/newlix/crashcart/internal/config"
	"github.com/newlix/crashcart/internal/store"
)

// Handler serves the viewer.
type Handler struct {
	Store       *store.Store
	Config      config.Config
	API         *api.Handler // for ChannelsInfo
	Log         *slog.Logger
	HTTP        *http.Client
	Deployments []Deployment
	tagKeys     map[string]bool
}

// New builds a viewer handler.
func New(st *store.Store, cfg config.Config, apiH *api.Handler, log *slog.Logger) *Handler {
	h := &Handler{Store: st, Config: cfg, API: apiH, Log: log, HTTP: &http.Client{Timeout: 5 * time.Second},
		Deployments: ParseDeployments(cfg.Deployments), tagKeys: map[string]bool{}}
	for _, t := range cfg.CustomTags {
		h.tagKeys[t.Key] = true
	}
	return h
}

// Register mounts the viewer. Slug-prefixed routes are dispatched by hand
// from the "/" fallback so they can't conflict with /api and /static.
func (h *Handler) Register(mux *http.ServeMux) {
	mux.HandleFunc("GET /{$}", h.root)
	mux.HandleFunc("GET /dashboard", h.bareDashboard)
	mux.HandleFunc("GET /settings", h.bareSettings)
	mux.HandleFunc("GET /events/{id}/detail", func(w http.ResponseWriter, r *http.Request) { h.detail(w, r, r.PathValue("id")) })
	mux.HandleFunc("PATCH /settings/alerts/{type}", func(w http.ResponseWriter, r *http.Request) { h.toggle(w, r, "", r.PathValue("type")) })
	mux.HandleFunc("GET /static/{name}", serveAsset)
	mux.HandleFunc("GET /favicon.ico", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) })
	mux.HandleFunc("/", h.slugRoutes)
}

// ── request helpers ─────────────────────────────────────────

var tzCookieRe = regexp.MustCompile(`^-?\d+(\.\d+)?$`)

// tzHours reads the viewer's UTC offset from the cc-tz cookie (set by app.js).
func tzHours(r *http.Request) int {
	c, err := r.Cookie("cc-tz")
	if err != nil {
		return 0
	}
	v, _ := url.QueryUnescape(c.Value)
	if !tzCookieRe.MatchString(v) {
		return 0
	}
	f, err := strconv.ParseFloat(v, 64)
	if err != nil {
		return 0
	}
	return int(min(max(f, -23), 23))
}

// isHx: fragment (true) or full page. History restores also carry
// HX-Request but replace the whole <body>, so they need the full document.
func isHx(r *http.Request) bool {
	return r.Header.Get("HX-Request") == "true" && r.Header.Get("HX-History-Restore-Request") != "true"
}

func isPoll(r *http.Request) bool { return r.Header.Get("X-Poll") == "1" }

func scheme(r *http.Request) string {
	if p := r.Header.Get("X-Forwarded-Proto"); p != "" {
		return p
	}
	if r.TLS != nil {
		return "https"
	}
	return "http"
}

func (h *Handler) selfIndex(r *http.Request) int {
	return SelfIndex(h.Deployments, scheme(r), r.Host)
}

func (h *Handler) projectName(r *http.Request) string {
	if i := h.selfIndex(r); i >= 0 {
		return h.Deployments[i].Name
	}
	return ""
}

func (h *Handler) render(w http.ResponseWriter, r *http.Request, c templ.Component) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := c.Render(r.Context(), w); err != nil {
		h.Log.Error("render failed", "path", r.URL.Path, "err", err)
	}
}

// fullPage wraps a shell in the Layout with the net-error banner.
func (h *Handler) fullPage(w http.ResponseWriter, r *http.Request, shell templ.Component) {
	h.render(w, r, withChildren(Layout(), templ.Join(NetError(), shell)))
}

// withChildren renders parent with child as its `{ children... }`.
func withChildren(parent, child templ.Component) templ.Component {
	return templ.ComponentFunc(func(ctx context.Context, w io.Writer) error {
		return parent.Render(templ.WithChildren(ctx, child), w)
	})
}

// bannerLines: config warnings computed per render (best effort).
func (h *Handler) bannerLines(ctx context.Context) []string {
	var lines []string
	if err := h.Store.Ping(ctx); err != nil {
		lines = append(lines, "Server health check failed — check server logs.")
	}
	alerts, err := h.Store.ListAlertTypes(ctx)
	if err != nil {
		return lines
	}
	enabled := 0
	for _, a := range alerts {
		if a.Enabled {
			enabled++
		}
	}
	ch := h.API.ChannelsInfo()
	hasChannels := (ch.TelegramConfigured && len(ch.Telegram) > 0) || len(ch.Webhooks) > 0 || (ch.EmailConfigured && len(ch.Emails) > 0)
	if enabled > 0 && !hasChannels {
		lines = append(lines, strconv.Itoa(enabled)+" alert type(s) enabled but no notification channels configured. Alerts will fire silently. Go to Settings → Notification Channels.")
	}
	return lines
}

func (h *Handler) fail(w http.ResponseWriter, r *http.Request, err error) {
	if errors.Is(err, store.ErrNotFound) {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	h.Log.Error("viewer request failed", "path", r.URL.Path, "err", err)
	http.Error(w, "internal server error", http.StatusInternalServerError)
}

// ── routing ─────────────────────────────────────────────────

// root: portal when DEPLOYMENTS is set, else the dashboard.
func (h *Handler) root(w http.ResponseWriter, r *http.Request) {
	if len(h.Deployments) == 0 {
		h.dashboard(w, r, "")
		return
	}
	tz := tzHours(r)
	s, win := windowForPortal(r.URL.Query(), tz, time.Now())
	self := h.selfIndex(r)
	sections := make([]PortalSection, len(h.Deployments))
	done := make(chan struct{}, len(h.Deployments))
	for i, d := range h.Deployments {
		go func(i int, d Deployment) {
			sections[i] = h.portalSection(r.Context(), d, i == self, win)
			done <- struct{}{}
		}(i, d)
	}
	for range h.Deployments {
		<-done
	}
	settingsHref := ""
	if self >= 0 {
		settingsHref = "/" + h.Deployments[self].Slug + "/settings"
	}
	h.render(w, r, withChildren(Layout(), Landing(sections, s.Win, s.Anchor, settingsHref)))
}

// bareDashboard / bareSettings: with DEPLOYMENTS, redirect to the slug form.
func (h *Handler) bareDashboard(w http.ResponseWriter, r *http.Request) {
	if h.redirectToSelf(w, r) {
		return
	}
	h.dashboard(w, r, "")
}

func (h *Handler) bareSettings(w http.ResponseWriter, r *http.Request) {
	if h.redirectToSelf(w, r) {
		return
	}
	h.settings(w, r, "")
}

func (h *Handler) redirectToSelf(w http.ResponseWriter, r *http.Request) bool {
	if len(h.Deployments) == 0 {
		return false
	}
	self := h.selfIndex(r)
	if self < 0 {
		http.Redirect(w, r, "/", http.StatusFound)
		return true
	}
	http.Redirect(w, r, "/"+h.Deployments[self].Slug+r.URL.Path+queryString(r), http.StatusFound)
	return true
}

func queryString(r *http.Request) string {
	if r.URL.RawQuery != "" {
		return "?" + r.URL.RawQuery
	}
	return ""
}

// slugRoutes dispatches /:slug/{dashboard|settings|events/:id/detail|settings/alerts/:type}.
// A known slug served by another instance redirects there; unknown → /.
func (h *Handler) slugRoutes(w http.ResponseWriter, r *http.Request) {
	slug, rest, ok := strings.Cut(strings.TrimPrefix(r.URL.Path, "/"), "/")
	if !ok || len(h.Deployments) == 0 {
		http.NotFound(w, r)
		return
	}
	idx := -1
	for i, d := range h.Deployments {
		if d.Slug == slug {
			idx = i
		}
	}
	if idx < 0 {
		http.Redirect(w, r, "/", http.StatusFound)
		return
	}
	if h.selfIndex(r) != idx {
		http.Redirect(w, r, h.Deployments[idx].URL+"/"+slug+"/"+rest+queryString(r), http.StatusFound)
		return
	}
	base := "/" + slug
	parts := strings.Split(rest, "/")
	switch {
	case r.Method == http.MethodGet && rest == "dashboard":
		h.dashboard(w, r, base)
	case r.Method == http.MethodGet && rest == "settings":
		h.settings(w, r, base)
	case r.Method == http.MethodGet && len(parts) == 3 && parts[0] == "events" && parts[2] == "detail":
		h.detail(w, r, parts[1])
	case r.Method == http.MethodPatch && len(parts) == 3 && parts[0] == "settings" && parts[1] == "alerts":
		h.toggle(w, r, base, parts[2])
	default:
		http.NotFound(w, r)
	}
}

// ── views ───────────────────────────────────────────────────

func (h *Handler) dashboard(w http.ResponseWriter, r *http.Request, base string) {
	s := ParseViewState(r.URL.Query(), h.tagKeys, base)
	win := s.WindowFor(tzHours(r), time.Now())
	data, err := h.loadDashboard(r.Context(), s, win)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	shell := withChildren(Shell("dashboard", s, h.bannerLines(r.Context()), h.projectName(r)), Dashboard(data, s, h.Config.CustomTags))
	if isHx(r) {
		if !isPoll(r) {
			w.Header().Set("HX-Push-Url", s.Href())
		}
		h.render(w, r, shell)
		return
	}
	h.fullPage(w, r, shell)
}

func (h *Handler) detail(w http.ResponseWriter, r *http.Request, idStr string) {
	id, err := strconv.ParseInt(idStr, 10, 64)
	if err != nil {
		http.Error(w, "invalid id", http.StatusBadRequest)
		return
	}
	ev, err := h.Store.GetEvent(r.Context(), id)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	w.Header().Set("HX-Push-Url", "false")
	h.render(w, r, EventDetail(ev))
}

func (h *Handler) settings(w http.ResponseWriter, r *http.Request, base string) {
	s := ParseViewState(r.URL.Query(), h.tagKeys, base)
	alerts, err := h.Store.ListAlertTypes(r.Context())
	if err != nil {
		h.fail(w, r, err)
		return
	}
	shell := withChildren(Shell("settings", s, h.bannerLines(r.Context()), h.projectName(r)), SettingsPanel(alerts, h.API.ChannelsInfo(), base))
	if isHx(r) {
		w.Header().Set("HX-Push-Url", base+"/settings")
		h.render(w, r, shell)
		return
	}
	h.fullPage(w, r, shell)
}

// toggle flips an alert type. HTML routes are unauthenticated; requiring
// the htmx header (browsers only send it after a same-origin request —
// these routes have no CORS) blocks cross-site form PATCHes.
func (h *Handler) toggle(w http.ResponseWriter, r *http.Request, base, typ string) {
	if !isHx(r) {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}
	enabled := r.FormValue("enabled") == "true"
	if err := h.Store.SetAlertEnabled(r.Context(), typ, enabled); err != nil {
		h.fail(w, r, err)
		return
	}
	alerts, err := h.Store.ListAlertTypes(r.Context())
	if err != nil {
		h.fail(w, r, err)
		return
	}
	w.Header().Set("HX-Push-Url", "false")
	h.render(w, r, SettingsPanel(alerts, h.API.ChannelsInfo(), base))
}
