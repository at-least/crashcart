package api

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"mime/multipart"
	"net/http"
	"path"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/newlix/crashcart/internal/db/sqlc"
	"github.com/newlix/crashcart/internal/symbolicate"
)

// MaxSymbolUpload caps one symbol upload request (and one zip entry).
const MaxSymbolUpload = 50 << 20

var symbolKinds = map[string]bool{"proguard": true, "sourcemap": true, "dsym": true}

// uuidRe matches the `<uuid>.txt` names sentry-cli gives ProGuard mappings.
var uuidRe = regexp.MustCompile(`(?i)^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

// detectKind guesses a symbol file kind from its name and first bytes,
// defaulting to dsym for opaque binaries.
func detectKind(filename string, head []byte) string {
	if k := symbolicate.DetectKind(filename, head); k != "" {
		return k
	}
	return "dsym"
}

// debugIDFor derives a debug id when the filename carries one
// (sentry-cli names ProGuard mappings `<uuid>.txt`).
func debugIDFor(kind, filename string) *string {
	base := path.Base(filename)
	stem := strings.TrimSuffix(base, path.Ext(base))
	if kind == "proguard" && uuidRe.MatchString(stem) {
		s := strings.ToLower(stem)
		return &s
	}
	return nil
}

// storeSymbolFile stores one file (or every entry of a zip) and returns the
// rows written. kind == "" means detect per file.
func (h *Handler) storeSymbolFile(ctx context.Context, projectID int64, release, kind, filename string, data []byte) ([]sqlc.UpsertSymbolFileRow, error) {
	if len(data) == 0 {
		return nil, badRequest("empty file")
	}
	if kind != "" && !symbolKinds[kind] {
		return nil, badRequest("kind must be proguard, sourcemap or dsym")
	}
	var rows []sqlc.UpsertSymbolFileRow
	if isZip(filename, data) {
		zr, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
		if err != nil {
			return nil, badRequest("invalid zip: " + err.Error())
		}
		for _, f := range zr.File {
			name := path.Clean("/" + f.Name)[1:]
			if f.FileInfo().IsDir() || name == "" || strings.HasPrefix(name, "__MACOSX/") || strings.HasPrefix(path.Base(name), ".") {
				continue
			}
			if f.UncompressedSize64 > MaxSymbolUpload {
				return nil, badRequest("zip entry too large: " + name)
			}
			rc, err := f.Open()
			if err != nil {
				return nil, badRequest("invalid zip entry: " + name)
			}
			content, err := io.ReadAll(io.LimitReader(rc, MaxSymbolUpload+1))
			rc.Close()
			if err != nil {
				return nil, err
			}
			if len(content) > MaxSymbolUpload {
				return nil, badRequest("zip entry too large: " + name)
			}
			if len(content) == 0 {
				continue
			}
			row, err := h.upsertSymbol(ctx, projectID, release, kind, name, content)
			if err != nil {
				return nil, err
			}
			rows = append(rows, row)
		}
		if len(rows) == 0 {
			return nil, badRequest("zip contains no files")
		}
	} else {
		row, err := h.upsertSymbol(ctx, projectID, release, kind, path.Base(filename), data)
		if err != nil {
			return nil, err
		}
		rows = append(rows, row)
	}
	// Re-queue the release's unsymbolicated events and drop cached mappings.
	if release != "" {
		args, _ := json.Marshal(map[string]string{"release": release})
		if err := h.Store.EnqueueJob(ctx, sqlc.EnqueueJobParams{Kind: "resymbolicate", ProjectID: projectID, Args: args, RunAfter: time.Now()}); err != nil {
			return nil, err
		}
	}
	if h.Symbols != nil {
		h.Symbols.Invalidate(projectID, release)
	}
	return rows, nil
}

func (h *Handler) upsertSymbol(ctx context.Context, projectID int64, release, kind, name string, data []byte) (sqlc.UpsertSymbolFileRow, error) {
	if kind == "" {
		kind = detectKind(name, data[:min(len(data), 4096)])
	}
	return h.Store.UpsertSymbolFile(ctx, sqlc.UpsertSymbolFileParams{
		ProjectID: projectID, Kind: kind, Release: release, DebugID: debugIDFor(kind, name),
		Filename: name, Size: int64(len(data)), Data: data,
	})
}

func isZip(filename string, data []byte) bool {
	return bytes.HasPrefix(data, []byte("PK\x03\x04")) || strings.HasSuffix(strings.ToLower(filename), ".zip")
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
