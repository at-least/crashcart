// Package api serves the JSON API under /api/projects/… and the sentry-cli
// compatible debug-file upload under /api/0/….
//
// All responses are JSON with snake_case keys, RFC3339 UTC timestamps and
// integer ids. Errors are {"error": "…"} with a matching status code.
package api

import (
	"context"
	"errors"
	"log/slog"
	"maps"
	"net/http"
	"slices"
	"strconv"

	"github.com/jackc/pgx/v5"

	"github.com/at-least/crashcart/internal/auth"
	"github.com/at-least/crashcart/internal/config"
	"github.com/at-least/crashcart/internal/db/sqlc"
	"github.com/at-least/crashcart/internal/store"
	"github.com/at-least/crashcart/internal/symbolicate"
)

// Handler holds the dependencies of the API routes.
type Handler struct {
	Store   *store.Store
	Cfg     config.Config
	Symbols *symbolicate.Service
	Log     *slog.Logger
}

// Register mounts the routes (with auth/CORS/rate limiting applied) on mux.
//
// Every route is registered with its own method + path pattern so the mux
// can route by method; the shared middleware chain (CORS → Bearer → rate
// limit) wraps each handler.
func (h *Handler) Register(mux *http.ServeMux) {
	// CORS on the JSON API only when API_CORS_ORIGIN is set: the SDK ingest
	// endpoints need it (browser SDKs), a key-protected API usually does not.
	access := &auth.Access{Store: h.Store}
	// Rate limit before the key check: a flood of bad keys must not cost a
	// database lookup per request.
	mws := []func(http.Handler) http.Handler{auth.RateLimit("api", h.Cfg.RateLimit, auth.BearerCredential), access.APIKey}
	if h.Cfg.APICORSOrigin != "" {
		mws = append([]func(http.Handler) http.Handler{auth.CORS(h.Cfg.APICORSOrigin)}, mws...)
	}
	wrap := func(fn http.HandlerFunc) http.Handler { return auth.Chain(fn, mws...) }
	for pattern, fn := range h.routes() {
		mux.Handle(pattern, wrap(fn))
	}
}

// RoutePatterns lists every pattern Register mounts, sorted; the API
// reference (cmd/gendocs) is checked against it.
func RoutePatterns() []string {
	return slices.Sorted(maps.Keys((*Handler)(nil).routes()))
}

// routes maps mux patterns to handlers.
func (h *Handler) routes() map[string]http.HandlerFunc {
	return map[string]http.HandlerFunc{
		// projects
		"GET /api/projects":                    h.listProjects,
		"POST /api/projects":                   h.createProject,
		"POST /api/projects/{slug}/rotate-key": h.rotateKey,
		"GET /api/projects/{slug}":             h.getProject,
		"PATCH /api/projects/{slug}":           h.updateProject,
		"DELETE /api/projects/{slug}":          h.deleteProject,
		"GET /api/projects/{slug}/overview":    h.overview,
		// issues
		"GET /api/projects/{slug}/issues":                 h.listIssues,
		"POST /api/projects/{slug}/issues/bulk":           h.bulkIssues,
		"GET /api/projects/{slug}/issues/{fingerprint}":   h.getIssue,
		"PATCH /api/projects/{slug}/issues/{fingerprint}": h.updateIssue,
		// events
		"GET /api/projects/{slug}/events":                      h.listEvents,
		"GET /api/projects/{slug}/events/{id}":                 h.getEvent,
		"GET /api/projects/{slug}/events/{id}/attachments/{n}": h.getAttachment,
		// user reports (Sentry's user feedback)
		"GET /api/projects/{slug}/user_reports": h.listUserReports,
		// client reports (SDK-side discarded event counts)
		"GET /api/projects/{slug}/client_reports": h.listClientReports,
		// releases
		"GET /api/projects/{slug}/releases":           h.listReleases,
		"GET /api/projects/{slug}/releases/{version}": h.getRelease,
		// alerts
		"GET /api/projects/{slug}/alerts":                  h.getAlerts,
		"PATCH /api/projects/{slug}/alerts/{type}":         h.updateAlertRule,
		"POST /api/projects/{slug}/alerts/channels":        h.createAlertChannel,
		"DELETE /api/projects/{slug}/alerts/channels/{id}": h.deleteAlertChannel,
		// symbols
		"GET /api/projects/{slug}/symbols":         h.listSymbols,
		"POST /api/projects/{slug}/symbols":        h.uploadSymbols,
		"DELETE /api/projects/{slug}/symbols/{id}": h.deleteSymbol,
		// sentry-cli compatibility
		"GET /api/0/projects/{org}/{project}/files/dsyms/":                      h.sentryListDSYMs,
		"POST /api/0/projects/{org}/{project}/files/dsyms/":                     h.sentryUploadDSYMs,
		"POST /api/0/organizations/{org}/chunk-upload/":                         h.sentryChunkUploadPost,
		"GET /api/0/organizations/{org}/chunk-upload/":                          h.sentryChunkUploadOptions,
		"POST /api/0/projects/{org}/{project}/files/difs/assemble/":             h.sentryAssemble,
		"POST /api/0/projects/{org}/{project}/files/dsyms/associate/":           h.sentryAssociate,
		"POST /api/0/projects/{org}/{project}/files/proguard-artifact-releases": h.sentryProguardArtifactRelease,
		// CORS preflights
		"OPTIONS /api/projects":  h.preflight,
		"OPTIONS /api/projects/": h.preflight,
		"OPTIONS /api/0/":        h.preflight,
	}
}

// preflight is only reached when CORS did not answer (never in practice).
func (h *Handler) preflight(w http.ResponseWriter, _ *http.Request) {
	w.WriteHeader(http.StatusNoContent)
}

// project resolves the {slug} path value; a 404 has been written when ok is false.
func (h *Handler) project(w http.ResponseWriter, r *http.Request) (sqlc.Project, bool) {
	return h.projectBySlugOrID(w, r, r.PathValue("slug"))
}

// projectBySlugOrID resolves a project by slug, falling back to numeric id
// (sentry-cli sends whichever it was configured with).
func (h *Handler) projectBySlugOrID(w http.ResponseWriter, r *http.Request, ref string) (sqlc.Project, bool) {
	p, err := h.lookupProject(r.Context(), ref)
	if err != nil {
		h.fail(w, err)
		return sqlc.Project{}, false
	}
	return p, true
}

func (h *Handler) lookupProject(ctx context.Context, ref string) (sqlc.Project, error) {
	if ref == "" {
		return sqlc.Project{}, errNotFound
	}
	p, err := h.Store.GetProject(ctx, ref)
	if errors.Is(err, pgx.ErrNoRows) {
		if id, perr := strconv.ParseInt(ref, 10, 64); perr == nil {
			return h.Store.GetProjectByID(ctx, id)
		}
	}
	return p, err
}
