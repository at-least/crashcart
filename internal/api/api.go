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
	"net/http"
	"strconv"

	"github.com/jackc/pgx/v5"

	"github.com/newlix/crashcart/internal/auth"
	"github.com/newlix/crashcart/internal/config"
	"github.com/newlix/crashcart/internal/db/sqlc"
	"github.com/newlix/crashcart/internal/store"
	"github.com/newlix/crashcart/internal/symbolicate"
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
	wrap := func(fn http.HandlerFunc) http.Handler {
		return auth.Chain(fn, auth.CORS(h.Cfg.CORSOrigin), auth.Bearer(h.Cfg.APIKeys),
			auth.RateLimit(h.Store, h.Cfg.RateLimit, auth.BearerCredential))
	}
	routes := map[string]http.HandlerFunc{
		// projects
		"GET /api/projects":                 h.listProjects,
		"POST /api/projects":                h.createProject,
		"GET /api/projects/{slug}":          h.getProject,
		"PATCH /api/projects/{slug}":        h.updateProject,
		"DELETE /api/projects/{slug}":       h.deleteProject,
		"GET /api/projects/{slug}/overview": h.overview,
		// issues
		"GET /api/projects/{slug}/issues":                 h.listIssues,
		"POST /api/projects/{slug}/issues/bulk":           h.bulkIssues,
		"GET /api/projects/{slug}/issues/{fingerprint}":   h.getIssue,
		"PATCH /api/projects/{slug}/issues/{fingerprint}": h.updateIssue,
		// events
		"GET /api/projects/{slug}/events":      h.listEvents,
		"GET /api/projects/{slug}/events/{id}": h.getEvent,
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
		"GET /api/0/projects/{org}/{project}/files/dsyms/":  h.sentryListDSYMs,
		"POST /api/0/projects/{org}/{project}/files/dsyms/": h.sentryUploadDSYMs,
		"POST /api/0/organizations/{org}/chunk-upload/":     h.sentryChunkUpload,
		"GET /api/0/organizations/{org}/chunk-upload/":      h.sentryChunkUpload,
		// CORS preflights
		"OPTIONS /api/projects":  h.preflight,
		"OPTIONS /api/projects/": h.preflight,
		"OPTIONS /api/0/":        h.preflight,
	}
	for pattern, fn := range routes {
		mux.Handle(pattern, wrap(fn))
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
