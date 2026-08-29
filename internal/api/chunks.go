package api

import (
	"bytes"
	"crypto/sha1"
	"encoding/hex"
	"errors"
	"io"
	"net/http"
	"strings"

	"github.com/crashcartapp/crashcart/internal/config"
	"github.com/crashcartapp/crashcart/internal/db/sqlc"
	"github.com/crashcartapp/crashcart/internal/symbolicate"
)

// Chunked upload protocol (sentry-cli `debug-files upload`, `upload-proguard`):
//
//  1. GET  /api/0/organizations/{org}/chunk-upload/   → options (chunk size, hash, accepted kinds)
//  2. POST /api/0/organizations/{org}/chunk-upload/   multipart, one part per chunk, filename = sha1
//  3. POST /api/0/projects/{org}/{project}/files/difs/assemble/
//     {"<file sha1>": {"name", "chunks": [sha1…], "debug_id"?}} → state per file;
//     sentry-cli uploads whatever is reported missing and polls until "ok".
//
// Chunks live in upload_chunks (Postgres, so any replica can assemble) and
// are deleted on assembly or expired by the retention sweep.

const (
	chunkSize        = 8 << 20
	chunksPerRequest = 8
	maxRequestSize   = chunkSize*chunksPerRequest + 1<<20
)

func (h *Handler) sentryChunkUploadOptions(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"url":              baseURL(h.Cfg, r) + "/api/0/organizations/" + r.PathValue("org") + "/chunk-upload/",
		"chunkSize":        chunkSize,
		"chunksPerRequest": chunksPerRequest,
		"maxFileSize":      symbolicate.MaxUpload,
		"maxRequestSize":   maxRequestSize,
		"concurrency":      4,
		"hashAlgorithm":    "sha1",
		"compression":      []string{},
		// No "sources": source bundles are zips of source files, not symbols.
		"accept": []string{"debug_files", "proguard"},
	})
}

func (h *Handler) sentryChunkUploadPost(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, maxRequestSize)
	mr, err := r.MultipartReader()
	if err != nil {
		h.fail(w, badRequest("multipart body expected"))
		return
	}
	for {
		part, err := mr.NextPart()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			h.fail(w, badRequest("invalid multipart body"))
			return
		}
		if part.FormName() != "file" {
			continue
		}
		data, err := io.ReadAll(io.LimitReader(part, chunkSize+1))
		if err != nil || len(data) > chunkSize {
			h.fail(w, badRequest("chunk too large"))
			return
		}
		sum := sha1.Sum(data)
		if hex.EncodeToString(sum[:]) != strings.ToLower(part.FileName()) {
			h.fail(w, badRequest("chunk checksum mismatch: "+part.FileName()))
			return
		}
		if err := h.Store.PutUploadChunk(r.Context(), sqlc.PutUploadChunkParams{Sha1: strings.ToLower(part.FileName()), Data: data}); err != nil {
			h.fail(w, err)
			return
		}
	}
	w.WriteHeader(http.StatusOK)
}

type assembleRequest struct {
	Name    string   `json:"name"`
	Chunks  []string `json:"chunks"`
	DebugID string   `json:"debug_id"`
}

type assembleResponse struct {
	State         string           `json:"state"` // ok | created | not_found | error
	MissingChunks []string         `json:"missingChunks"`
	Detail        string           `json:"detail,omitempty"`
	Dif           *sentryDebugFile `json:"dif,omitempty"`
}

func (h *Handler) sentryAssemble(w http.ResponseWriter, r *http.Request) {
	p, ok := h.projectBySlugOrID(w, r, r.PathValue("project"))
	if !ok {
		return
	}
	var req map[string]assembleRequest
	if err := readJSON(w, r, &req); err != nil {
		h.fail(w, err)
		return
	}
	ctx := r.Context()
	out := map[string]assembleResponse{}
	for checksum, f := range req {
		checksum = strings.ToLower(checksum)
		chunks := make([]string, len(f.Chunks))
		for i, c := range f.Chunks {
			chunks[i] = strings.ToLower(c)
		}
		present, err := h.Store.UploadChunksPresent(ctx, chunks)
		if err != nil {
			h.fail(w, err)
			return
		}
		have := map[string]bool{}
		for _, c := range present {
			have[c] = true
		}
		missing := []string{}
		for _, c := range chunks {
			if !have[c] {
				missing = append(missing, c)
			}
		}
		if len(missing) > 0 || len(chunks) == 0 {
			out[checksum] = assembleResponse{State: "not_found", MissingChunks: missing}
			continue
		}
		var buf bytes.Buffer
		for _, c := range chunks {
			data, err := h.Store.GetUploadChunk(ctx, c)
			if err != nil {
				h.fail(w, err)
				return
			}
			buf.Write(data)
			if buf.Len() > symbolicate.MaxUpload {
				break
			}
		}
		sum := sha1.Sum(buf.Bytes())
		if hex.EncodeToString(sum[:]) != checksum {
			out[checksum] = assembleResponse{State: "error", MissingChunks: []string{}, Detail: "assembled file does not match its checksum"}
			continue
		}
		name := f.Name
		if name == "" {
			name = checksum
		}
		rows, err := h.Symbols.UploadWithDebugID(ctx, p.ID, "", "", name, buf.Bytes(), f.DebugID)
		var ue symbolicate.UploadError
		if errors.As(err, &ue) {
			out[checksum] = assembleResponse{State: "error", MissingChunks: []string{}, Detail: ue.Error()}
			continue
		}
		if err != nil {
			h.fail(w, err)
			return
		}
		h.Store.DeleteUploadChunks(ctx, chunks)
		dif := sentryDebugFileFrom(sqlc.ListSymbolFilesRow(rows[0]), checksum)
		out[checksum] = assembleResponse{State: "ok", MissingChunks: []string{}, Dif: &dif}
	}
	writeJSON(w, http.StatusOK, out)
}

// baseURL is the externally visible origin: cfg.PublicURL, else derived
// from the request like DSN does.
func baseURL(cfg config.Config, r *http.Request) string {
	if cfg.PublicURL != "" {
		return strings.TrimSuffix(cfg.PublicURL, "/")
	}
	scheme := "http"
	if fp := r.Header.Get("X-Forwarded-Proto"); fp != "" {
		scheme = strings.TrimSpace(strings.Split(fp, ",")[0])
	} else if r.TLS != nil {
		scheme = "https"
	}
	return scheme + "://" + r.Host
}

// sentryAssociate answers the follow-up `upload-proguard` makes after an
// upload (the Sentry Gradle plugin passes --app-id/--version): it ties
// mappings to an app release. CrashCart matches mappings by debug_id, so
// the release is only recorded on the recent uploads for the settings page.
func (h *Handler) sentryAssociate(w http.ResponseWriter, r *http.Request) {
	p, ok := h.projectBySlugOrID(w, r, r.PathValue("project"))
	if !ok {
		return
	}
	var req struct {
		AppID   string `json:"appId"`
		Version string `json:"version"`
		Build   string `json:"build"`
	}
	if err := readJSON(w, r, &req); err != nil {
		h.fail(w, err)
		return
	}
	release := strings.TrimSpace(req.Version)
	if req.AppID != "" && release != "" {
		release = req.AppID + "@" + release
		if req.Build != "" {
			release += "+" + req.Build
		}
	}
	if err := h.Symbols.Associate(r.Context(), p.ID, release, ""); err != nil {
		h.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"associatedDsymFiles": []any{}})
}

// sentryProguardArtifactRelease is what the Sentry Gradle plugin's bundled
// sentry-cli calls after a legacy ProGuard upload: {proguard_uuid,
// release_name}.
func (h *Handler) sentryProguardArtifactRelease(w http.ResponseWriter, r *http.Request) {
	p, ok := h.projectBySlugOrID(w, r, r.PathValue("project"))
	if !ok {
		return
	}
	var req struct {
		UUID    string `json:"proguard_uuid"`
		Release string `json:"release_name"`
	}
	if err := readJSON(w, r, &req); err != nil {
		h.fail(w, err)
		return
	}
	if req.UUID != "" {
		if err := h.Symbols.Associate(r.Context(), p.ID, req.Release, req.UUID); err != nil {
			h.fail(w, err)
			return
		}
	}
	writeJSON(w, http.StatusCreated, map[string]any{})
}
