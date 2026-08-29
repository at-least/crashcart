package api

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/newlix/crashcart/internal/alerts"
	"github.com/newlix/crashcart/internal/config"
	"github.com/newlix/crashcart/internal/db/sqlc"
	"github.com/newlix/crashcart/internal/sentry"
)

// projectOut is the JSON shape of a project (public key exposed as the DSN).
type projectOut struct {
	ID              int64     `json:"id"`
	Slug            string    `json:"slug"`
	Name            string    `json:"name"`
	Platform        *string   `json:"platform"`
	SampleKeepFirst int32     `json:"sample_keep_first"`
	SampleRate      float64   `json:"sample_rate"`
	CreatedAt       time.Time `json:"created_at"`
	DSN             string    `json:"dsn"`
}

func (h *Handler) projectOut(r *http.Request, p sqlc.Project) projectOut {
	return projectOut{
		ID: p.ID, Slug: p.Slug, Name: p.Name, Platform: p.Platform,
		SampleKeepFirst: p.SampleKeepFirst, SampleRate: p.SampleRate, CreatedAt: p.CreatedAt.UTC(),
		DSN: DSN(h.Cfg, r, p),
	}
}

// DSN renders `<scheme>://<public_key>@<host>/<id>` for p. The base is
// cfg.PublicURL when set, otherwise derived from the request
// (X-Forwarded-Proto or "http", and r.Host). r may be nil.
func DSN(cfg config.Config, r *http.Request, p sqlc.Project) string {
	scheme, host := "http", ""
	if cfg.PublicURL != "" {
		s, h, ok := strings.Cut(cfg.PublicURL, "://")
		if ok {
			scheme, host = s, h
		} else {
			host = cfg.PublicURL
		}
		host = strings.TrimSuffix(host, "/")
	} else if r != nil {
		if fp := r.Header.Get("X-Forwarded-Proto"); fp != "" {
			scheme = strings.TrimSpace(strings.Split(fp, ",")[0])
		} else if r.TLS != nil {
			scheme = "https"
		}
		host = r.Host
	}
	if host == "" {
		host = "localhost" + cfg.Addr
	}
	return fmt.Sprintf("%s://%s@%s/%d", scheme, p.PublicKey, host, p.ID)
}

// NewKey returns a random 32-hex DSN public key.
func NewKey() string {
	b := make([]byte, 16)
	rand.Read(b)
	return hex.EncodeToString(b)
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
		Slug: in.Slug, Name: in.Name, Platform: nilIfEmpty(in.Platform), PublicKey: NewKey(),
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
	}
	if err := readJSON(w, r, &in); err != nil {
		h.fail(w, err)
		return
	}
	upd := sqlc.UpdateProjectParams{ID: p.ID, Name: p.Name, Platform: p.Platform, SampleKeepFirst: p.SampleKeepFirst, SampleRate: p.SampleRate}
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
	if err := h.Store.DeleteProject(r.Context(), p.ID); err != nil {
		h.fail(w, err)
		return
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
