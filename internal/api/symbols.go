package api

import (
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/at-least/crashcart/internal/db/sqlc"
	"github.com/at-least/crashcart/internal/symbolicate"
)

func (h *Handler) listSymbols(w http.ResponseWriter, r *http.Request) {
	p, ok := h.project(w, r)
	if !ok {
		return
	}
	files, err := h.Store.ListSymbolFiles(r.Context(), p.ID)
	if err != nil {
		h.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"symbols": files})
}

func (h *Handler) uploadSymbols(w http.ResponseWriter, r *http.Request) {
	p, ok := h.project(w, r)
	if !ok {
		return
	}
	name, data, err := symbolicate.ReadMultipartUpload(w, r)
	if err != nil {
		h.fail(w, err)
		return
	}
	release := strings.TrimSpace(r.FormValue("release"))
	rows, err := h.Symbols.Upload(r.Context(), p.ID, release, strings.TrimSpace(r.FormValue("kind")), name, data)
	if err != nil {
		h.fail(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"symbols": rows})
}

func (h *Handler) deleteSymbol(w http.ResponseWriter, r *http.Request) {
	p, ok := h.project(w, r)
	if !ok {
		return
	}
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "invalid symbol file id")
		return
	}
	n, err := h.Store.DeleteSymbolFile(r.Context(), sqlc.DeleteSymbolFileParams{ProjectID: p.ID, ID: id})
	if err != nil {
		h.fail(w, err)
		return
	}
	if n == 0 {
		writeErr(w, http.StatusNotFound, "not found")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// ── sentry-cli compatibility ────────────────────────────────────────────
//
// sentry-cli ≥ 3 only speaks the chunked upload protocol (chunks.go):
// GET chunk-upload/ for the options, POST chunk-upload/ for the chunks,
// POST files/difs/assemble/ to turn them into debug files. sentry-cli 2
// falls back to the legacy multipart upload below when it must. The org
// segment is ignored; the project may be the slug or the numeric id.

type sentryDebugFile struct {
	ID          string         `json:"id"`
	UUID        string         `json:"uuid"`
	DebugID     string         `json:"debugId"`
	Name        string         `json:"objectName"`
	CPUName     string         `json:"cpuName"`
	SHA1        string         `json:"sha1"`
	Size        int64          `json:"size"`
	DateCreated time.Time      `json:"dateCreated"`
	Headers     map[string]any `json:"headers"`
	SymbolType  string         `json:"symbolType"`
	Data        map[string]any `json:"data"`
	Release     string         `json:"release,omitempty"`
}

func sentryDebugFileFrom(f sqlc.ListSymbolFilesRow, sha string) sentryDebugFile {
	// symbolType is the file format; data.features what the file provides.
	// data.type (symbolic's object class) is optional for sentry-cli and its
	// accepted spellings vary between versions, so it is left out.
	typ := map[string]string{"proguard": "proguard", "sourcemap": "sourcebundle", "dsym": "macho"}[string(f.Kind)]
	features := map[string][]string{"proguard": {"mapping"}, "sourcemap": {"sources"}, "dsym": {"debug", "symtab"}}[string(f.Kind)]
	data := map[string]any{"features": features}
	return sentryDebugFile{
		ID: strconv.FormatInt(f.ID, 10), UUID: deref(f.DebugID), DebugID: deref(f.DebugID), Name: f.Filename,
		CPUName: "any", SHA1: sha, Size: f.Size, DateCreated: f.UploadedAt.UTC(), Headers: map[string]any{},
		SymbolType: typ, Data: data, Release: deref(f.Release),
	}
}

func (h *Handler) sentryUploadDSYMs(w http.ResponseWriter, r *http.Request) {
	p, ok := h.projectBySlugOrID(w, r, r.PathValue("project"))
	if !ok {
		return
	}
	name, data, err := symbolicate.ReadMultipartUpload(w, r)
	if err != nil {
		h.fail(w, err)
		return
	}
	release := strings.TrimSpace(r.FormValue("release"))
	rows, err := h.Symbols.Upload(r.Context(), p.ID, release, "", name, data)
	if err != nil {
		h.fail(w, err)
		return
	}
	out := make([]sentryDebugFile, 0, len(rows))
	for _, row := range rows {
		out = append(out, sentryDebugFileFrom(sqlc.ListSymbolFilesRow(row), ""))
	}
	writeJSON(w, http.StatusCreated, out)
}

func (h *Handler) sentryListDSYMs(w http.ResponseWriter, r *http.Request) {
	p, ok := h.projectBySlugOrID(w, r, r.PathValue("project"))
	if !ok {
		return
	}
	files, err := h.Store.ListSymbolFiles(r.Context(), p.ID)
	if err != nil {
		h.fail(w, err)
		return
	}
	want := map[string]bool{}
	for _, id := range r.URL.Query()["debug_id"] {
		want[strings.ToLower(id)] = true
	}
	out := []sentryDebugFile{}
	for _, f := range files {
		if len(want) > 0 && !want[strings.ToLower(deref(f.DebugID))] {
			continue
		}
		out = append(out, sentryDebugFileFrom(f, ""))
	}
	writeJSON(w, http.StatusOK, out)
}
