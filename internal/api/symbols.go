package api

import (
	"errors"
	"io"
	"net/http"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/newlix/crashcart/internal/db/sqlc"
	"github.com/newlix/crashcart/internal/symbolicate"
)

// MaxSymbolBytes caps one symbol upload (dSYMs are large, not unbounded).
const MaxSymbolBytes = 50 << 20

// UploadSymbol is POST /api/symbols?platform=&release=&file= with the raw
// file as body. Re-uploading the same (platform, release, file) replaces it.
func (h *Handler) UploadSymbol(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	platform, release, file := q.Get("platform"), q.Get("release"), q.Get("file")
	if platform == "" || release == "" || file == "" || strings.ContainsAny(file, "/\\") {
		writeError(w, http.StatusBadRequest, "missing platform, release, or file param")
		return
	}
	if r.ContentLength > MaxSymbolBytes {
		writeError(w, http.StatusRequestEntityTooLarge, "payload too large")
		return
	}
	data, err := io.ReadAll(io.LimitReader(r.Body, MaxSymbolBytes+1))
	if err != nil {
		writeError(w, http.StatusBadRequest, "unreadable body")
		return
	}
	if len(data) > MaxSymbolBytes {
		writeError(w, http.StatusRequestEntityTooLarge, "payload too large")
		return
	}
	if len(data) == 0 {
		writeError(w, http.StatusBadRequest, "empty body")
		return
	}
	row, err := h.Store.Queries().UpsertSymbolFile(r.Context(), sqlc.UpsertSymbolFileParams{
		Platform: platform, Release: release, Filename: file, Size: int64(len(data)), Data: data,
	})
	if err != nil {
		h.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, row)
}

// ListSymbols is GET /api/symbols.
func (h *Handler) ListSymbols(w http.ResponseWriter, r *http.Request) {
	rows, err := h.Store.Queries().ListSymbolFiles(r.Context())
	if err != nil {
		h.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, rows)
}

// Symbolicate is POST /api/symbolicate {platform, release, frames}.
// ProGuard and source maps resolve in-process; dSYMs go to the container.
func (h *Handler) Symbolicate(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Platform string              `json:"platform"`
		Release  string              `json:"release"`
		Frames   []symbolicate.Frame `json:"frames"`
	}
	if err := readJSON(r, &body); err != nil || body.Platform == "" || body.Release == "" || body.Frames == nil {
		writeError(w, http.StatusBadRequest, "missing platform, release, or frames")
		return
	}
	respond := func(frames []symbolicate.Frame, ok bool) {
		writeJSON(w, http.StatusOK, map[string]any{"frames": frames, "symbolicated": ok})
	}
	q := h.Store.Queries()
	meta, err := q.LatestSymbolFile(r.Context(), sqlc.LatestSymbolFileParams{Platform: body.Platform, Release: body.Release})
	if errors.Is(err, pgx.ErrNoRows) {
		respond(body.Frames, false)
		return
	}
	if err != nil {
		h.fail(w, r, err)
		return
	}
	data, err := q.GetSymbolFileData(r.Context(), sqlc.GetSymbolFileDataParams{Platform: meta.Platform, Release: meta.Release, Filename: meta.Filename})
	if err != nil {
		h.fail(w, r, err)
		return
	}
	name := strings.ToLower(meta.Filename)
	switch {
	case strings.HasSuffix(name, ".txt"):
		respond(symbolicate.ParseProGuard(string(data)).ResolveAll(body.Frames), true)
	case strings.HasSuffix(name, ".map") || body.Platform == "javascript" || body.Platform == "node":
		sm, err := symbolicate.ParseSourceMap(data)
		if err != nil {
			h.Log.Warn("symbolicate: bad source map", "file", meta.Filename, "err", err)
			respond(body.Frames, false)
			return
		}
		respond(sm.ResolveAll(body.Frames), true)
	case strings.Contains(name, ".dsym") || body.Platform == "ios" || body.Platform == "apple-ios" || body.Platform == "cocoa":
		if !h.DSYM.Enabled() {
			respond(body.Frames, false)
			return
		}
		frames, err := h.DSYM.Resolve(r.Context(), data, body.Frames)
		if err != nil {
			h.Log.Error("symbolicate: container call failed", "err", err)
			respond(body.Frames, false)
			return
		}
		respond(frames, true)
	default:
		respond(body.Frames, false)
	}
}
