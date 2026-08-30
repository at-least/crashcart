// Package web is the server-rendered viewer (templ + htmx).
//
//	GET  /                          portal: one card per project + create form
//	GET  /p/{slug}                  overview
//	GET  /p/{slug}/issues           issue list (tabs, filters, bulk triage)
//	GET  /p/{slug}/issues/{fp}      issue: stack, breakdown, timeline, events
//	GET  /p/{slug}/events           raw event list with filter toolbar
//	GET  /p/{slug}/events/{id}      event page (fragment when HX-Request)
//	GET  /p/{slug}/events/{id}/attachments/{n}  one attachment's bytes (a screenshot)
//	GET  /p/{slug}/releases[/{v}]   release health
//	GET  /p/{slug}/settings         DSN, sampling, alerts, channels, symbols
//	GET  /p/{slug}/stream           SSE: new issues / regressions counter
//	GET  /login, /setup, /account   sign in, first user, users + API keys (account.go)
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
	"net/url"
	"strings"

	"github.com/crashcartapp/crashcart/internal/metrics"

	"github.com/a-h/templ"
	"github.com/jackc/pgx/v5"

	"github.com/crashcartapp/crashcart/internal/auth"
	"github.com/crashcartapp/crashcart/internal/config"
	"github.com/crashcartapp/crashcart/internal/db/sqlc"
	"github.com/crashcartapp/crashcart/internal/store"
	"github.com/crashcartapp/crashcart/internal/symbolicate"
)

// Web holds the viewer's dependencies.
type Web struct {
	Store    *store.Store
	Cfg      config.Config
	Log      *slog.Logger
	Symbols  *symbolicate.Service
	Listener *store.Listener // wakes the SSE stream on issue notifications; nil = poll only
	Stopping <-chan struct{} // closed at shutdown: the SSE streams end so Shutdown can drain the rest; nil = never

	access *auth.Access
}

// Register mounts the HTML routes and /static on mux.
func (w *Web) Register(mux *http.ServeMux) {
	w.access = &auth.Access{Store: w.Store, TrustProxy: w.Cfg.TrustProxy}
	ip := auth.IPCredential(w.Cfg.TrustProxy)
	// The limiter runs outermost: a flood of unauthenticated requests must
	// not cost a session lookup each (the API does the same).
	limit := auth.RateLimit("web", w.Cfg.RateLimit, ip)
	page := func(h http.HandlerFunc) http.Handler { return auth.Chain(h, limit, w.access.Session) }
	public := func(h http.HandlerFunc) http.Handler { return auth.Chain(h, limit, w.access.Identify) }
	mutation := func(h http.HandlerFunc) http.Handler { return page(requireHX(h)) }
	// Password posts get their own, much smaller budget: each one is a
	// bcrypt verification, and the general limit is sized for page loads.
	signIn := func(h http.HandlerFunc) http.Handler {
		return auth.Chain(sameOrigin(h), auth.RateLimit("login", LoginRateLimit, ip), w.access.Identify)
	}

	mux.Handle("GET /login", public(w.loginPage))
	mux.Handle("POST /login", signIn(w.login))
	mux.Handle("POST /logout", mutation(w.logout))
	mux.Handle("GET /setup", public(w.setupPage))
	mux.Handle("POST /setup", signIn(w.setup))
	mux.Handle("GET /account", page(w.account))
	mux.Handle("POST /account/users", mutation(w.accountUserAdd))
	mux.Handle("DELETE /account/users/{id}", mutation(w.accountUserDelete))
	mux.Handle("POST /account/keys", mutation(w.accountKeyCreate))
	mux.Handle("DELETE /account/keys/{id}", mutation(w.accountKeyRevoke))

	mux.Handle("GET /{$}", page(w.portal))
	mux.Handle("POST /projects", mutation(w.createProject))
	mux.Handle("GET /p/{slug}", page(w.overview))
	mux.Handle("GET /p/{slug}/issues", page(w.issues))
	mux.Handle("POST /p/{slug}/issues/bulk", mutation(w.issuesBulk))
	mux.Handle("GET /p/{slug}/issues/{fingerprint}", page(w.issue))
	mux.Handle("PATCH /p/{slug}/issues/{fingerprint}/status", mutation(w.issueStatus))
	mux.Handle("GET /p/{slug}/events", page(w.events))
	mux.Handle("GET /p/{slug}/events/{id}", page(w.event))
	mux.Handle("GET /p/{slug}/events/{id}/attachments/{n}", page(w.attachment))
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

// CSRFRejected counts mutations refused for missing the HX-Request header
// or for coming from another origin.
var CSRFRejected = metrics.NewCounter("crashcart_web_csrf_rejected_total", "HTML mutations refused as cross-site (no HX-Request header, or a foreign Origin).")

// sameOrigin guards the two public form posts (sign-in, first-user
// setup) that cannot carry the HX-Request header: a browser announces a
// cross-site post in Sec-Fetch-Site and Origin, and those are refused.
// Requests without either header (curl, old browsers) pass.
func sameOrigin(next http.HandlerFunc) http.HandlerFunc {
	return func(rw http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Sec-Fetch-Site") == "cross-site" {
			refuseCrossSite(rw, "cross-site request refused")
			return
		}
		if o := r.Header.Get("Origin"); o != "" && o != "null" {
			if u, err := url.Parse(o); err != nil || !strings.EqualFold(u.Host, r.Host) {
				refuseCrossSite(rw, "cross-site request refused")
				return
			}
		}
		next(rw, r)
	}
}

// refuseCrossSite is the one 403 (and the one metric) of the CSRF guards.
func refuseCrossSite(rw http.ResponseWriter, msg string) {
	CSRFRejected.Inc()
	http.Error(rw, msg, http.StatusForbidden)
}

// requireHX rejects mutations that do not come from htmx (CSRF guard:
// browsers never add custom headers to cross-site form posts).
func requireHX(next http.HandlerFunc) http.HandlerFunc {
	return func(rw http.ResponseWriter, r *http.Request) {
		if r.Header.Get("HX-Request") != "true" {
			refuseCrossSite(rw, "htmx request required")
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
	User        string        // signed-in user's email
}

// page completes pg (custom tags, current path) and renders body(pg)
// inside the layout + shell. body is a constructor so it sees the
// completed page, not the handler's bare copy.
func (w *Web) page(rw http.ResponseWriter, r *http.Request, pg Page, body func(Page) templ.Component) {
	pg.Tags = w.Cfg.CustomTags
	pg.User = auth.ActorFrom(r.Context()).Name
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
	return auth.BaseURL(r, w.Cfg.PublicURL, w.Cfg.TrustProxy)
}
