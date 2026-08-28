// Package api serves the JSON API under /api/projects/… and the sentry-cli
// compatible debug-file upload under /api/0/….
package api

import (
	"log/slog"
	"net/http"

	"github.com/newlix/crashcart/internal/config"
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
func (h *Handler) Register(mux *http.ServeMux) {
	// TODO(api)
}
