package api

import (
	"context"
	"errors"
	"io"
	"mime/multipart"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/newlix/crashcart/internal/db/sqlc"
	"github.com/newlix/crashcart/internal/symbolicate"
)

// MaxSymbolUpload caps one symbol upload request (and one zip entry).
const MaxSymbolUpload = symbolicate.MaxUpload

// storeSymbolFile stores one file (or every entry of a zip) through the
// shared symbolicate.Service path; caller mistakes become 400s.
func (h *Handler) storeSymbolFile(ctx context.Context, projectID int64, release, kind, filename string, data []byte) ([]sqlc.UpsertSymbolFileRow, error) {
	rows, err := h.Symbols.Upload(ctx, projectID, release, kind, filename, data)
	var ue symbolicate.UploadError
	if errors.As(err, &ue) {
		return nil, badRequest(string(ue))
	}
	return rows, err
}

// readUpload parses a multipart body (bounded by MaxSymbolUpload) and
// returns the `file` part.
func readUpload(w http.ResponseWriter, r *http.Request) (*multipart.FileHeader, []byte, error) {
	r.Body = http.MaxBytesReader(w, r.Body, MaxSymbolUpload+1<<20)
	if err := r.ParseMultipartForm(8 << 20); err != nil {
		var mbe *http.MaxBytesError
		if errors.As(err, &mbe) {
			return nil, nil, badRequest("upload exceeds 50 MB")
		}
		return nil, nil, badRequest("multipart form expected: " + err.Error())
	}
	f, fh, err := r.FormFile("file")
	if err != nil {
		return nil, nil, badRequest(`multipart field "file" is required`)
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, MaxSymbolUpload+1))
	if err != nil {
		return nil, nil, err
	}
	if len(data) > MaxSymbolUpload {
		return nil, nil, badRequest("upload exceeds 50 MB")
	}
	return fh, data, nil
}

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
	fh, data, err := readUpload(w, r)
	if err != nil {
		h.fail(w, err)
		return
	}
	release := strings.TrimSpace(r.FormValue("release"))
	rows, err := h.storeSymbolFile(r.Context(), p.ID, release, strings.TrimSpace(r.FormValue("kind")), fh.Filename, data)
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
// `sentry-cli upload-dif` / `upload-proguard` first try the chunked upload
// protocol (GET /api/0/organizations/{org}/chunk-upload/); a 404 there
// makes them fall back to the legacy multipart upload below, which is the
// one CrashCart implements. The org segment is ignored; the project may be
// the slug or the numeric id.

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
	typ := map[string]string{"proguard": "proguard", "sourcemap": "sourcebundle", "dsym": "macho"}[f.Kind]
	return sentryDebugFile{
		ID: strconv.FormatInt(f.ID, 10), UUID: deref(f.DebugID), DebugID: deref(f.DebugID), Name: f.Filename,
		CPUName: "any", SHA1: sha, Size: f.Size, DateCreated: f.UploadedAt.UTC(), Headers: map[string]any{},
		SymbolType: typ, Data: map[string]any{"type": typ}, Release: f.Release,
	}
}

func (h *Handler) sentryChunkUpload(w http.ResponseWriter, _ *http.Request) {
	writeErr(w, http.StatusNotFound, "chunked upload is not supported; sentry-cli falls back to the legacy upload")
}

func (h *Handler) sentryUploadDSYMs(w http.ResponseWriter, r *http.Request) {
	p, ok := h.projectBySlugOrID(w, r, r.PathValue("project"))
	if !ok {
		return
	}
	fh, data, err := readUpload(w, r)
	if err != nil {
		h.fail(w, err)
		return
	}
	release := strings.TrimSpace(r.FormValue("release"))
	rows, err := h.storeSymbolFile(r.Context(), p.ID, release, "", fh.Filename, data)
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
