package api

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"

	"github.com/at-least/crashcart/internal/alerts"
	"github.com/jackc/pgx/v5"

	"github.com/at-least/crashcart/internal/store"
)

var alertTypes = map[string]bool{
	"new_issue": true, "regression": true, "unhandled_spike": true, "escalating": true,
	"monitor_failed": true, "monitor_recovered": true,
}

func (h *Handler) getAlerts(w http.ResponseWriter, r *http.Request) {
	p, ok := h.project(w, r)
	if !ok {
		return
	}
	rules, err := store.ListAlertRules(r.Context(), h.Store.Pool, p.ID)
	if err != nil {
		h.fail(w, err)
		return
	}
	channels, err := store.ListAlertChannels(r.Context(), h.Store.Pool, p.ID)
	if err != nil {
		h.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"rules": rules, "channels": channels})
}

func (h *Handler) updateAlertRule(w http.ResponseWriter, r *http.Request) {
	p, ok := h.project(w, r)
	if !ok {
		return
	}
	typ := r.PathValue("type")
	if !alertTypes[typ] {
		writeErr(w, http.StatusNotFound, "unknown alert type")
		return
	}
	var in struct {
		Enabled         *bool  `json:"enabled"`
		CooldownMinutes *int32 `json:"cooldown_minutes"`
	}
	if err := readJSON(w, r, &in); err != nil {
		h.fail(w, err)
		return
	}
	cur, err := store.GetAlertRule(r.Context(), h.Store.Pool, p.ID, store.AlertType(typ))
	if errors.Is(err, pgx.ErrNoRows) {
		cur = store.AlertRule{Enabled: true, CooldownMinutes: 60}
	} else if err != nil { // a read error must not re-enable a disabled rule with defaults
		h.fail(w, err)
		return
	}
	if in.Enabled != nil {
		cur.Enabled = *in.Enabled
	}
	if in.CooldownMinutes != nil {
		if *in.CooldownMinutes < 0 {
			writeErr(w, http.StatusBadRequest, "cooldown_minutes must be >= 0")
			return
		}
		cur.CooldownMinutes = *in.CooldownMinutes
	}
	rule, err := store.UpsertAlertRule(r.Context(), h.Store.Pool, p.ID, store.AlertType(typ), cur.Enabled, cur.CooldownMinutes)
	if err != nil {
		h.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, rule)
}

func (h *Handler) createAlertChannel(w http.ResponseWriter, r *http.Request) {
	p, ok := h.project(w, r)
	if !ok {
		return
	}
	var in struct {
		Kind   string          `json:"kind"`
		Config json.RawMessage `json:"config"`
	}
	if err := readJSON(w, r, &in); err != nil {
		h.fail(w, err)
		return
	}
	var cfg map[string]any
	if len(in.Config) > 0 {
		if err := json.Unmarshal(in.Config, &cfg); err != nil {
			writeErr(w, http.StatusBadRequest, "config must be an object")
			return
		}
	}
	str := func(k string) string {
		switch v := cfg[k].(type) {
		case string:
			return strings.TrimSpace(v)
		case float64:
			return strconv.FormatInt(int64(v), 10)
		}
		return ""
	}
	config, err := alerts.ChannelConfig(in.Kind, str, h.Cfg.WebhookAllowPrivate)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	in.Config = config
	ch, err := store.CreateAlertChannel(r.Context(), h.Store.Pool, p.ID, store.ChannelKind(in.Kind), in.Config)
	if err != nil {
		h.fail(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, ch)
}

func (h *Handler) deleteAlertChannel(w http.ResponseWriter, r *http.Request) {
	p, ok := h.project(w, r)
	if !ok {
		return
	}
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "invalid channel id")
		return
	}
	n, err := store.DeleteAlertChannel(r.Context(), h.Store.Pool, p.ID, id)
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
