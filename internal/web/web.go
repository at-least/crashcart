// Package web is the server-rendered viewer (templ + htmx).
//
//	GET  /                          portal: one card per project + create form
//	GET  /p/{slug}                  overview
//	GET  /p/{slug}/issues           issue list (tabs, filters, bulk triage)
//	GET  /p/{slug}/issues/{fp}      issue: stack, breakdown, timeline, events
//	GET  /p/{slug}/events           raw event list with filter toolbar
//	GET  /p/{slug}/events/{id}      event page (fragment when HX-Request)
//	GET  /p/{slug}/releases[/{v}]   release health
//	GET  /p/{slug}/settings         DSN, sampling, alerts, channels, symbols
//	GET  /p/{slug}/stream           SSE: new issues / regressions counter
//	GET  /static/{file}             embedded assets
//
// All state lives in the URL (state.go). Mutations are htmx-only: they
// require the HX-Request header (CSRF guard) and answer with a redirect
// the browser follows inside the XHR, so hx-select can pick the fragment.
package web

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strings"

	"github.com/a-h/templ"
	"github.com/jackc/pgx/v5"

	"github.com/at-least/crashcart/internal/auth"
	"github.com/at-least/crashcart/internal/config"
	"github.com/at-least/crashcart/internal/db/sqlc"
	"github.com/at-least/crashcart/internal/store"
	"github.com/at-least/crashcart/internal/symbolicate"
)

// Web holds the viewer's dependencies.
type Web struct {
	Store   *store.Store
	Cfg     config.Config
	Log     *slog.Logger
	Symbols *symbolicate.Service
}

// Register mounts the HTML routes and /static on mux.
func (w *Web) Register(mux *http.ServeMux) {
	page := func(h http.HandlerFunc) http.Handler {
		return auth.Chain(h, auth.Basic(w.Cfg.ViewerPassword), auth.RateLimit(w.Store, w.Cfg.RateLimit, auth.IPCredential))
	}
	mutation := func(h http.HandlerFunc) http.Handler { return page(requireHX(h)) }

	mux.Handle("GET /{$}", page(w.portal))
	mux.Handle("POST /projects", mutation(w.createProject))
	mux.Handle("GET /p/{slug}", page(w.overview))
	mux.Handle("GET /p/{slug}/issues", page(w.issues))
	mux.Handle("POST /p/{slug}/issues/bulk", mutation(w.issuesBulk))
	mux.Handle("GET /p/{slug}/issues/{fingerprint}", page(w.issue))
	mux.Handle("PATCH /p/{slug}/issues/{fingerprint}/status", mutation(w.issueStatus))
	mux.Handle("GET /p/{slug}/events", page(w.events))
	mux.Handle("GET /p/{slug}/events/{id}", page(w.event))
	mux.Handle("GET /p/{slug}/releases", page(w.releases))
	mux.Handle("GET /p/{slug}/releases/{version}", page(w.release))
	mux.Handle("GET /p/{slug}/settings", page(w.settings))
	mux.Handle("PATCH /p/{slug}/settings/sampling", mutation(w.settingsSampling))
	mux.Handle("PATCH /p/{slug}/settings/platform", mutation(w.settingsPlatform))
	mux.Handle("POST /p/{slug}/settings/rotate-key", mutation(w.settingsRotateKey))
	mux.Handle("PATCH /p/{slug}/settings/alerts/{type}", mutation(w.settingsAlert))
	mux.Handle("POST /p/{slug}/settings/channels", mutation(w.settingsChannelAdd))
	mux.Handle("DELETE /p/{slug}/settings/channels/{id}", mutation(w.settingsChannelDelete))
	mux.Handle("POST /p/{slug}/settings/symbols", mutation(w.settingsSymbolUpload))
	mux.Handle("DELETE /p/{slug}/settings/symbols/{id}", mutation(w.settingsSymbolDelete))
	mux.Handle("GET /p/{slug}/stream", page(w.stream))
	mux.HandleFunc("GET /static/{file}", serveAsset)
	mux.HandleFunc("GET /favicon.ico", func(rw http.ResponseWriter, _ *http.Request) { rw.WriteHeader(http.StatusNoContent) })
}

// requireHX rejects mutations that do not come from htmx (CSRF guard:
// browsers never add custom headers to cross-site form posts).
func requireHX(next http.HandlerFunc) http.HandlerFunc {
	return func(rw http.ResponseWriter, r *http.Request) {
		if r.Header.Get("HX-Request") != "true" {
			http.Error(rw, "htmx request required", http.StatusForbidden)
			return
		}
		next(rw, r)
	}
}

// isHx: fragment (true) or full page. History restores also carry
// HX-Request but replace the whole <body>, so they need the full document.
func isHx(r *http.Request) bool {
	return r.Header.Get("HX-Request") == "true" && r.Header.Get("HX-History-Restore-Request") != "true"
}

// project resolves {slug}; writes 404 and returns false when unknown.
func (w *Web) project(rw http.ResponseWriter, r *http.Request) (sqlc.Project, bool) {
	p, err := w.Store.GetProject(r.Context(), r.PathValue("slug"))
	if errors.Is(err, pgx.ErrNoRows) {
		http.NotFound(rw, r)
		return p, false
	}
	if err != nil {
		w.fail(rw, r, err)
		return p, false
	}
	return p, true
}

func (w *Web) fail(rw http.ResponseWriter, r *http.Request, err error) {
	var ue symbolicate.UploadError
	if errors.As(err, &ue) {
		http.Error(rw, ue.Error(), http.StatusBadRequest)
		return
	}
	w.Log.Error("web", "path", r.URL.Path, "err", err)
	http.Error(rw, "internal error", http.StatusInternalServerError)
}

func (w *Web) render(rw http.ResponseWriter, r *http.Request, c templ.Component) {
	rw.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := c.Render(r.Context(), rw); err != nil {
		w.Log.Error("render", "path", r.URL.Path, "err", err)
	}
}

// Page is what the shell needs on every project page.
type Page struct {
	S           ViewState
	Project     *sqlc.Project // nil on the portal
	Section     string        // overview | issues | events | releases | settings
	Stream      string        // SSE URL when the page shows the "new issues" banner
	Regressions int64         // current regression count (baseline for the banner)
	Tags        []string      // custom tag keys shown as filters
	Path        string        // current path below the project base (window links)
}

// page completes pg (custom tags, current path) and renders body(pg)
// inside the layout + shell. body is a constructor so it sees the
// completed page, not the handler's bare copy.
func (w *Web) page(rw http.ResponseWriter, r *http.Request, pg Page, body func(Page) templ.Component) {
	pg.Tags = w.Cfg.CustomTags
	if pg.Project != nil {
		pg.Path = strings.TrimPrefix(r.URL.Path, pg.S.Base())
	}
	w.render(rw, r, withChildren(Layout(pageTitle(pg)), withChildren(Shell(pg), body(pg))))
}

func pageTitle(pg Page) string {
	if pg.Project == nil {
		return "CrashCart"
	}
	return pg.Project.Name + " · " + pg.Section + " · CrashCart"
}

// withChildren renders parent with child as its `{ children... }`.
func withChildren(parent, child templ.Component) templ.Component {
	return templ.ComponentFunc(func(ctx context.Context, wr io.Writer) error {
		return parent.Render(templ.WithChildren(ctx, child), wr)
	})
}

// state parses the view state for the request's project.
func state(r *http.Request) ViewState { return ParseViewState(r.PathValue("slug"), r.URL.Query()) }

// redirect answers a mutation: htmx follows the redirect inside the XHR
// (hx-select picks the fragment); plain clients navigate.
func redirect(rw http.ResponseWriter, r *http.Request, to string) {
	http.Redirect(rw, r, to, http.StatusSeeOther)
}

// baseURL is the externally visible origin for DSN display.
func (w *Web) baseURL(r *http.Request) string {
	if w.Cfg.PublicURL != "" {
		return w.Cfg.PublicURL
	}
	scheme := "http"
	if p := r.Header.Get("X-Forwarded-Proto"); p != "" {
		scheme = p
	} else if r.TLS != nil {
		scheme = "https"
	}
	return scheme + "://" + r.Host
}
