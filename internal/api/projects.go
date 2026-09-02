package api

import (
	"context"
	"errors"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/at-least/crashcart/internal/alerts"
	"github.com/at-least/crashcart/internal/auth"
	"github.com/at-least/crashcart/internal/config"
	"github.com/at-least/crashcart/internal/db/sqlc"
	"github.com/at-least/crashcart/internal/sentry"
)

// projectOut is the JSON shape of a project (public key exposed as the DSN).
type projectOut struct {
	ID              int64     `json:"id"`
	Slug            string    `json:"slug"`
	Name            string    `json:"name"`
	Platform        *string   `json:"platform"`
	SampleKeepFirst int32     `json:"sample_keep_first"`
	DailyQuota      int32     `json:"daily_quota"`
	SampleRate      float64   `json:"sample_rate"`
	CreatedAt       time.Time `json:"created_at"`
	DSN             string    `json:"dsn"`
}

func (h *Handler) projectOut(r *http.Request, p sqlc.Project) projectOut {
	return projectOut{
		ID: p.ID, Slug: p.Slug, Name: p.Name, Platform: p.Platform,
		SampleKeepFirst: p.SampleKeepFirst, SampleRate: p.SampleRate, DailyQuota: p.DailyQuota, CreatedAt: p.CreatedAt.UTC(),
		DSN: DSN(h.Cfg, r, p),
	}
}

// DSN renders the project's DSN on the externally visible origin
// (cfg.PublicURL, else derived from the request; r may be nil).
func DSN(cfg config.Config, r *http.Request, p sqlc.Project) string {
	base := cfg.PublicURL
	if base == "" && r != nil {
		base = auth.BaseURL(r, "", cfg.TrustProxy)
	}
	if base == "" {
		base = "http://localhost" + cfg.Addr
	}
	return auth.DSN(base, p.PublicKey, p.ID)
}

var slugRe = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,63}$`)

func (h *Handler) listProjects(w http.ResponseWriter, r *http.Request) {
	ps, err := h.Store.ListProjects(r.Context())
	if err != nil {
		h.fail(w, err)
		return
	}
	out := make([]projectOut, 0, len(ps))
	for _, p := range ps {
		out = append(out, h.projectOut(r, p))
	}
	writeJSON(w, http.StatusOK, out)
}

func (h *Handler) createProject(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Slug     string `json:"slug"`
		Name     string `json:"name"`
		Platform string `json:"platform"`
	}
	if err := readJSON(w, r, &in); err != nil {
		h.fail(w, err)
		return
	}
	in.Slug = strings.TrimSpace(in.Slug)
	in.Name = strings.TrimSpace(in.Name)
	if !slugRe.MatchString(in.Slug) {
		writeErr(w, http.StatusBadRequest, "slug must match ^[a-z0-9][a-z0-9._-]{0,63}$")
		return
	}
	if in.Slug == "bulk" || isNumeric(in.Slug) {
		writeErr(w, http.StatusBadRequest, "slug is reserved")
		return
	}
	if in.Name == "" {
		in.Name = in.Slug
	}
	if in.Platform != "" && !sentry.IsFamily(in.Platform) {
		writeErr(w, http.StatusBadRequest, "platform must be one of "+strings.Join(sentry.Families, ", "))
		return
	}
	p, err := h.Store.CreateProject(r.Context(), sqlc.CreateProjectParams{
		Slug: in.Slug, Name: in.Name, Platform: nilIfEmpty(in.Platform), PublicKey: auth.NewProjectKey(),
	})
	if err != nil {
		var pg *pgconn.PgError
		if errors.As(err, &pg) && pg.Code == "23505" {
			writeErr(w, http.StatusConflict, "slug already exists")
			return
		}
		h.fail(w, err)
		return
	}
	if err := alerts.EnsureRules(r.Context(), h.Store, p.ID); err != nil {
		h.fail(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, h.projectOut(r, p))
}

func (h *Handler) getProject(w http.ResponseWriter, r *http.Request) {
	p, ok := h.project(w, r)
	if !ok {
		return
	}
	writeJSON(w, http.StatusOK, h.projectOut(r, p))
}

func (h *Handler) updateProject(w http.ResponseWriter, r *http.Request) {
	p, ok := h.project(w, r)
	if !ok {
		return
	}
	var in struct {
		Name            *string  `json:"name"`
		Platform        *string  `json:"platform"`
		SampleKeepFirst *int32   `json:"sample_keep_first"`
		SampleRate      *float64 `json:"sample_rate"`
		DailyQuota      *int32   `json:"daily_quota"`
	}
	if err := readJSON(w, r, &in); err != nil {
		h.fail(w, err)
		return
	}
	upd := sqlc.UpdateProjectParams{ID: p.ID, Name: p.Name, Platform: p.Platform, SampleKeepFirst: p.SampleKeepFirst, SampleRate: p.SampleRate, DailyQuota: p.DailyQuota}
	if in.Name != nil {
		if strings.TrimSpace(*in.Name) == "" {
			writeErr(w, http.StatusBadRequest, "name must not be empty")
			return
		}
		upd.Name = strings.TrimSpace(*in.Name)
	}
	if in.Platform != nil {
		if *in.Platform != "" && !sentry.IsFamily(*in.Platform) {
			writeErr(w, http.StatusBadRequest, "platform must be one of "+strings.Join(sentry.Families, ", "))
			return
		}
		upd.Platform = nilIfEmpty(*in.Platform)
	}
	if in.SampleKeepFirst != nil {
		if *in.SampleKeepFirst < 0 {
			writeErr(w, http.StatusBadRequest, "sample_keep_first must be >= 0")
			return
		}
		upd.SampleKeepFirst = *in.SampleKeepFirst
	}
	if in.SampleRate != nil {
		if *in.SampleRate < 0 || *in.SampleRate > 1 {
			writeErr(w, http.StatusBadRequest, "sample_rate must be between 0 and 1")
			return
		}
		upd.SampleRate = *in.SampleRate
	}
	if in.DailyQuota != nil {
		if *in.DailyQuota < 0 {
			writeErr(w, http.StatusBadRequest, "daily_quota must be >= 0 (0 = unlimited)")
			return
		}
		upd.DailyQuota = *in.DailyQuota
	}
	np, err := h.Store.UpdateProject(r.Context(), upd)
	if err != nil {
		h.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, h.projectOut(r, np))
}

func (h *Handler) deleteProject(w http.ResponseWriter, r *http.Request) {
	p, ok := h.project(w, r)
	if !ok {
		return
	}
	// The rows go with the project (ON DELETE CASCADE); the objects its
	// symbol files point at do not, so their keys are read first and
	// deleted after.
	keys, err := h.Store.SymbolFileBlobKeys(r.Context(), p.ID)
	if err != nil {
		h.fail(w, err)
		return
	}
	packs, err := h.Store.ProjectPacks(r.Context(), p.ID)
	if err != nil {
		h.fail(w, err)
		return
	}
	if err := h.Store.DeleteProject(r.Context(), p.ID); err != nil {
		h.fail(w, err)
		return
	}
	if err := h.Symbols.DeleteBlobs(context.WithoutCancel(r.Context()), keys); err != nil {
		h.Log.Warn("project delete: symbol blobs left behind", "project", p.Slug, "err", err)
	}
	if err := h.Store.DeleteProjectPacks(context.WithoutCancel(r.Context()), p.ID, packs); err != nil {
		h.Log.Warn("project delete: payload packs left behind", "project", p.Slug, "err", err)
	}
	w.WriteHeader(http.StatusNoContent)
}

func nilIfEmpty(s string) *string {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil
	}
	return &s
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

func isNumeric(s string) bool {
	if s == "" {
		return false
	}
	for _, c := range s {
		if c < '0' || c > '9' {
			return false
		}
	}
	return true
}

// rotateKey issues a new current DSN key; the old one keeps authenticating
// (listed under GET .../keys) until explicitly deleted.
func (h *Handler) rotateKey(w http.ResponseWriter, r *http.Request) {
	p, ok := h.project(w, r)
	if !ok {
		return
	}
	np, err := h.Store.RotateProjectKey(r.Context(), p.ID, auth.NewProjectKey())
	if err != nil {
		h.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, h.projectOut(r, np))
}

// projectKeyOut is a retired-but-still-valid DSN key.
type projectKeyOut struct {
	ID         int64      `json:"id"`
	DSN        string     `json:"dsn"`
	RetiredAt  time.Time  `json:"retired_at"`
	LastUsedAt *time.Time `json:"last_used_at"`
}

func (h *Handler) listProjectKeys(w http.ResponseWriter, r *http.Request) {
	p, ok := h.project(w, r)
	if !ok {
		return
	}
	keys, err := h.Store.ListProjectKeys(r.Context(), p.ID)
	if err != nil {
		h.fail(w, err)
		return
	}
	out := make([]projectKeyOut, 0, len(keys))
	for _, k := range keys {
		out = append(out, projectKeyOut{ID: k.ID, DSN: DSN(h.Cfg, r, sqlc.Project{ID: p.ID, PublicKey: k.PublicKey}), RetiredAt: k.RetiredAt.UTC(), LastUsedAt: k.LastUsedAt})
	}
	writeJSON(w, http.StatusOK, map[string]any{"keys": out})
}

// deleteProjectKey deletes a retired key; it stops authenticating within
// the ingest cache TTL, not instantly.
func (h *Handler) deleteProjectKey(w http.ResponseWriter, r *http.Request) {
	p, ok := h.project(w, r)
	if !ok {
		return
	}
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "invalid key id")
		return
	}
	n, err := h.Store.DeleteProjectKey(r.Context(), sqlc.DeleteProjectKeyParams{ProjectID: p.ID, ID: id})
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
