package web

import (
	"errors"
	"net/http"
	"regexp"
	"strconv"
	"strings"

	"github.com/crashcartapp/crashcart/internal/alerts"
	"github.com/crashcartapp/crashcart/internal/auth"

	"github.com/a-h/templ"

	"github.com/jackc/pgx/v5"

	"github.com/crashcartapp/crashcart/internal/db/sqlc"
	"github.com/crashcartapp/crashcart/internal/sentry"
	"github.com/crashcartapp/crashcart/internal/symbolicate"
)

// AlertTypes are the rule types in display order (alert_rules CHECK).
var AlertTypes = []string{"new_issue", "regression", "unhandled_spike", "escalating"}

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
	d := SettingsData{EnvelopeURL: base + "/api/" + strconv.FormatInt(p.ID, 10) + "/envelope/", DSN: auth.DSN(base, p.PublicKey, p.ID)}
	rules, err := w.Store.ListAlertRules(ctx, p.ID)
	if err != nil {
		w.fail(rw, r, err)
		return
	}
	// Show every type even before its row exists (defaults: on, 60 min).
	have := map[string]sqlc.AlertRule{}
	for _, ru := range rules {
		have[string(ru.Type)] = ru
	}
	for _, t := range AlertTypes {
		ru, ok := have[t]
		if !ok {
			ru = sqlc.AlertRule{ProjectID: p.ID, Type: sqlc.AlertType(t), Enabled: true, CooldownMinutes: 60}
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
	if platform != "" && !sentry.IsFamily(platform) {
		http.Error(rw, "platform must be one of "+strings.Join(sentry.Families, ", "), http.StatusBadRequest)
		return
	}
	var plat *string
	if platform != "" {
		plat = &platform
	}
	ctx := r.Context()
	p, err := w.Store.CreateProject(ctx, sqlc.CreateProjectParams{Slug: slug, Name: name, Platform: plat, PublicKey: auth.NewProjectKey()})
	if err != nil {
		if strings.Contains(err.Error(), "duplicate") {
			http.Error(rw, "slug already exists", http.StatusConflict)
			return
		}
		w.fail(rw, r, err)
		return
	}
	for _, t := range AlertTypes {
		if _, err := w.Store.UpsertAlertRule(ctx, sqlc.UpsertAlertRuleParams{ProjectID: p.ID, Type: sqlc.AlertType(t), Enabled: true, CooldownMinutes: 60}); err != nil {
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
	// 32-bit parses: the columns are INTEGER, and a wider value would wrap negative.
	keep, err1 := strconv.ParseInt(r.Form.Get("keep_first"), 10, 32)
	rate, err2 := strconv.ParseFloat(r.Form.Get("rate"), 64)
	quota, err3 := int64(p.DailyQuota), error(nil)
	if v := strings.TrimSpace(r.Form.Get("daily_quota")); v != "" {
		quota, err3 = strconv.ParseInt(v, 10, 32)
	}
	if err1 != nil || err2 != nil || err3 != nil || keep < 0 || rate < 0 || rate > 1 || quota < 0 {
		http.Error(rw, "keep_first >= 0, 0 <= rate <= 1 and daily_quota >= 0 required", http.StatusBadRequest)
		return
	}
	if _, err := w.Store.UpdateProject(r.Context(), sqlc.UpdateProjectParams{ID: p.ID, Name: p.Name, Platform: p.Platform, SampleKeepFirst: int32(keep), SampleRate: rate, DailyQuota: int32(quota)}); err != nil {
		w.fail(rw, r, err)
		return
	}
	redirect(rw, r, state(r).Href("/settings"))
}

func (w *Web) settingsPlatform(rw http.ResponseWriter, r *http.Request) {
	p, ok := w.project(rw, r)
	if !ok {
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(rw, "bad form", http.StatusBadRequest)
		return
	}
	platform := strings.TrimSpace(r.Form.Get("platform"))
	if platform != "" && !sentry.IsFamily(platform) {
		http.Error(rw, "platform must be one of "+strings.Join(sentry.Families, ", "), http.StatusBadRequest)
		return
	}
	var plat *string
	if platform != "" {
		plat = &platform
	}
	if _, err := w.Store.UpdateProject(r.Context(), sqlc.UpdateProjectParams{ID: p.ID, Name: p.Name, Platform: plat, SampleKeepFirst: p.SampleKeepFirst, SampleRate: p.SampleRate, DailyQuota: p.DailyQuota}); err != nil {
		w.fail(rw, r, err)
		return
	}
	redirect(rw, r, state(r).Href("/settings"))
}

func (w *Web) settingsRotateKey(rw http.ResponseWriter, r *http.Request) {
	p, ok := w.project(rw, r)
	if !ok {
		return
	}
	if _, err := w.Store.RotateProjectKey(r.Context(), sqlc.RotateProjectKeyParams{ID: p.ID, PublicKey: auth.NewProjectKey()}); err != nil {
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
	if _, err := w.Store.UpsertAlertRule(r.Context(), sqlc.UpsertAlertRuleParams{ProjectID: p.ID, Type: sqlc.AlertType(typ), Enabled: enabled, CooldownMinutes: int32(cooldown)}); err != nil {
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
	cfg, err := alerts.ChannelConfig(r.PostForm.Get("kind"), r.PostForm.Get, w.Cfg.WebhookAllowPrivate)
	if err != nil {
		http.Error(rw, err.Error(), http.StatusBadRequest)
		return
	}
	if _, err := w.Store.CreateAlertChannel(r.Context(), sqlc.CreateAlertChannelParams{ProjectID: p.ID, Kind: sqlc.ChannelKind(r.PostForm.Get("kind")), Config: cfg}); err != nil {
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
