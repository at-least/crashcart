package web

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"regexp"
	"strconv"
	"strings"

	"github.com/a-h/templ"

	"github.com/jackc/pgx/v5"

	"github.com/newlix/crashcart/internal/db/sqlc"
	"github.com/newlix/crashcart/internal/symbolicate"
)

// AlertTypes are the rule types in display order (alert_rules CHECK).
var AlertTypes = []string{"new_issue", "regression", "crash_spike"}

// SettingsData feeds the settings page.
type SettingsData struct {
	DSN         string
	EnvelopeURL string
	Rules       []sqlc.AlertRule
	Channels    []sqlc.AlertChannel
	Symbols     []sqlc.ListSymbolFilesRow
}

func (w *Web) settings(rw http.ResponseWriter, r *http.Request) {
	p, ok := w.project(rw, r)
	if !ok {
		return
	}
	ctx := r.Context()
	base := w.baseURL(r)
	d := SettingsData{EnvelopeURL: base + "/api/" + strconv.FormatInt(p.ID, 10) + "/envelope/"}
	if i := strings.Index(base, "://"); i >= 0 {
		d.DSN = base[:i+3] + p.PublicKey + "@" + base[i+3:] + "/" + strconv.FormatInt(p.ID, 10)
	}
	rules, err := w.Store.ListAlertRules(ctx, p.ID)
	if err != nil {
		w.fail(rw, r, err)
		return
	}
	// Show every type even before its row exists (defaults: on, 60 min).
	have := map[string]sqlc.AlertRule{}
	for _, ru := range rules {
		have[ru.Type] = ru
	}
	for _, t := range AlertTypes {
		ru, ok := have[t]
		if !ok {
			ru = sqlc.AlertRule{ProjectID: p.ID, Type: t, Enabled: true, CooldownMinutes: 60}
		}
		d.Rules = append(d.Rules, ru)
	}
	if d.Channels, err = w.Store.ListAlertChannels(ctx, p.ID); err != nil {
		w.fail(rw, r, err)
		return
	}
	if d.Symbols, err = w.Store.ListSymbolFiles(ctx, p.ID); err != nil {
		w.fail(rw, r, err)
		return
	}
	pg := Page{S: state(r), Project: &p, Section: "settings"}
	w.page(rw, r, pg, func(pg Page) templ.Component { return Settings(pg, d) })
}

var slugRe = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,62}$`)

// createProject handles the portal form: random DSN key, default alert
// rules, then HX-Redirect to the settings page (which shows the DSN).
func (w *Web) createProject(rw http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(rw, "bad form", http.StatusBadRequest)
		return
	}
	slug := strings.ToLower(strings.TrimSpace(r.PostForm.Get("slug")))
	name := strings.TrimSpace(r.PostForm.Get("name"))
	platform := strings.TrimSpace(r.PostForm.Get("platform"))
	if !slugRe.MatchString(slug) || name == "" {
		http.Error(rw, "slug (a-z, 0-9, -) and name are required", http.StatusBadRequest)
		return
	}
	key := make([]byte, 16)
	if _, err := rand.Read(key); err != nil {
		w.fail(rw, r, err)
		return
	}
	var plat *string
	if platform != "" {
		plat = &platform
	}
	ctx := r.Context()
	p, err := w.Store.CreateProject(ctx, sqlc.CreateProjectParams{Slug: slug, Name: name, Platform: plat, PublicKey: hex.EncodeToString(key)})
	if err != nil {
		if strings.Contains(err.Error(), "duplicate") {
			http.Error(rw, "slug already exists", http.StatusConflict)
			return
		}
		w.fail(rw, r, err)
		return
	}
	for _, t := range AlertTypes {
		if _, err := w.Store.UpsertAlertRule(ctx, sqlc.UpsertAlertRuleParams{ProjectID: p.ID, Type: t, Enabled: true, CooldownMinutes: 60}); err != nil {
			w.fail(rw, r, err)
			return
		}
	}
	rw.Header().Set("HX-Redirect", "/p/"+p.Slug+"/settings")
	rw.WriteHeader(http.StatusOK)
}

func (w *Web) settingsSampling(rw http.ResponseWriter, r *http.Request) {
	p, ok := w.project(rw, r)
	if !ok {
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(rw, "bad form", http.StatusBadRequest)
		return
	}
	keep, err1 := strconv.Atoi(r.Form.Get("keep_first"))
	rate, err2 := strconv.ParseFloat(r.Form.Get("rate"), 64)
	if err1 != nil || err2 != nil || keep < 0 || rate < 0 || rate > 1 {
		http.Error(rw, "keep_first >= 0 and 0 <= rate <= 1 required", http.StatusBadRequest)
		return
	}
	if _, err := w.Store.UpdateProject(r.Context(), sqlc.UpdateProjectParams{ID: p.ID, Name: p.Name, Platform: p.Platform, SampleKeepFirst: int32(keep), SampleRate: rate}); err != nil {
		w.fail(rw, r, err)
		return
	}
	redirect(rw, r, state(r).Href("/settings"))
}

func (w *Web) settingsAlert(rw http.ResponseWriter, r *http.Request) {
	p, ok := w.project(rw, r)
	if !ok {
		return
	}
	typ := r.PathValue("type")
	known := false
	for _, t := range AlertTypes {
		known = known || t == typ
	}
	if !known {
		http.NotFound(rw, r)
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(rw, "bad form", http.StatusBadRequest)
		return
	}
	en := r.Form.Get("enabled")
	enabled := en == "on" || en == "true" || en == "1"
	cooldown := 60
	if v := r.Form.Get("cooldown"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 0 || n > 100000 {
			http.Error(rw, "bad cooldown", http.StatusBadRequest)
			return
		}
		cooldown = n
	}
	if _, err := w.Store.UpsertAlertRule(r.Context(), sqlc.UpsertAlertRuleParams{ProjectID: p.ID, Type: typ, Enabled: enabled, CooldownMinutes: int32(cooldown)}); err != nil {
		w.fail(rw, r, err)
		return
	}
	redirect(rw, r, state(r).Href("/settings"))
}

func (w *Web) settingsChannelAdd(rw http.ResponseWriter, r *http.Request) {
	p, ok := w.project(rw, r)
	if !ok {
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(rw, "bad form", http.StatusBadRequest)
		return
	}
	var cfg []byte
	switch kind := r.PostForm.Get("kind"); kind {
	case "webhook":
		u := strings.TrimSpace(r.PostForm.Get("url"))
		if !strings.HasPrefix(u, "http://") && !strings.HasPrefix(u, "https://") {
			http.Error(rw, "webhook url must be http(s)", http.StatusBadRequest)
			return
		}
		cfg, _ = json.Marshal(map[string]string{"url": u})
	case "telegram":
		id := strings.TrimSpace(r.PostForm.Get("chat_id"))
		if id == "" {
			http.Error(rw, "chat_id required", http.StatusBadRequest)
			return
		}
		cfg, _ = json.Marshal(map[string]string{"chat_id": id})
	default:
		http.Error(rw, "kind must be webhook or telegram", http.StatusBadRequest)
		return
	}
	if _, err := w.Store.CreateAlertChannel(r.Context(), sqlc.CreateAlertChannelParams{ProjectID: p.ID, Kind: r.PostForm.Get("kind"), Config: cfg}); err != nil {
		w.fail(rw, r, err)
		return
	}
	redirect(rw, r, state(r).Href("/settings"))
}

func (w *Web) settingsChannelDelete(rw http.ResponseWriter, r *http.Request) {
	p, ok := w.project(rw, r)
	if !ok {
		return
	}
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		http.NotFound(rw, r)
		return
	}
	if _, err := w.Store.DeleteAlertChannel(r.Context(), sqlc.DeleteAlertChannelParams{ProjectID: p.ID, ID: id}); err != nil {
		w.fail(rw, r, err)
		return
	}
	redirect(rw, r, state(r).Href("/settings"))
}

func (w *Web) settingsSymbolUpload(rw http.ResponseWriter, r *http.Request) {
	p, ok := w.project(rw, r)
	if !ok {
		return
	}
	name, data, err := symbolicate.ReadMultipartUpload(rw, r)
	if err != nil {
		w.fail(rw, r, err)
		return
	}
	release := strings.TrimSpace(r.FormValue("release"))
	if _, err := w.Symbols.Upload(r.Context(), p.ID, release, "", name, data); err != nil {
		w.fail(rw, r, err)
		return
	}
	redirect(rw, r, state(r).Href("/settings"))
}

func (w *Web) settingsSymbolDelete(rw http.ResponseWriter, r *http.Request) {
	p, ok := w.project(rw, r)
	if !ok {
		return
	}
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		http.NotFound(rw, r)
		return
	}
	if _, err := w.Store.DeleteSymbolFile(r.Context(), sqlc.DeleteSymbolFileParams{ProjectID: p.ID, ID: id}); err != nil && !errors.Is(err, pgx.ErrNoRows) {
		w.fail(rw, r, err)
		return
	}
	redirect(rw, r, state(r).Href("/settings"))
}
