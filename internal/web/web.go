// Package web is the server-rendered viewer (templ + htmx).
package web

import (
	"log/slog"
	"net/http"

	"github.com/newlix/crashcart/internal/config"
	"github.com/newlix/crashcart/internal/store"
)

// Web holds the viewer's dependencies.
type Web struct {
	Store *store.Store
	Cfg   config.Config
	Log   *slog.Logger
}

// Register mounts the HTML routes and /static on mux.
func (w *Web) Register(mux *http.ServeMux) {
	// TODO(web)
}
