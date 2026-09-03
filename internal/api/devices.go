package api

import (
	"net/http"
	"strconv"
	"strings"

	"github.com/crashcartapp/crashcart/internal/auth"
	"github.com/crashcartapp/crashcart/internal/store"
)

// registerDevice upserts the calling key's mobile device (by push token) —
// the companion apps' sign-in / token-refresh call.
func (h *Handler) registerDevice(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Token    string `json:"token"`
		Platform string `json:"platform"`
	}
	if err := readJSON(w, r, &in); err != nil {
		h.fail(w, err)
		return
	}
	in.Token = strings.TrimSpace(in.Token)
	if in.Token == "" {
		writeErr(w, http.StatusBadRequest, "token is required")
		return
	}
	if in.Platform != "ios" && in.Platform != "android" {
		writeErr(w, http.StatusBadRequest, "platform must be ios or android")
		return
	}
	d, err := store.UpsertPushDevice(r.Context(), h.Store.Pool, auth.ActorFrom(r.Context()).KeyID, in.Token, in.Platform)
	if err != nil {
		h.fail(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, d)
}

// deleteDevice unregisters a device (sign-out); its subscriptions go with
// it via cascade.
func (h *Handler) deleteDevice(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "invalid device id")
		return
	}
	n, err := store.DeletePushDevice(r.Context(), h.Store.Pool, auth.ActorFrom(r.Context()).KeyID, id)
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

// subscribeDevice turns on push notifications from this project for a
// device the calling key owns (all enabled alert types — v1 has no
// per-type push filter, same granularity as alert_rules).
func (h *Handler) subscribeDevice(w http.ResponseWriter, r *http.Request) {
	p, ok := h.project(w, r)
	if !ok {
		return
	}
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "invalid device id")
		return
	}
	found, err := store.SubscribePush(r.Context(), h.Store.Pool, auth.ActorFrom(r.Context()).KeyID, id, p.ID)
	if err != nil {
		h.fail(w, err)
		return
	}
	if !found {
		writeErr(w, http.StatusNotFound, "not found")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *Handler) unsubscribeDevice(w http.ResponseWriter, r *http.Request) {
	p, ok := h.project(w, r)
	if !ok {
		return
	}
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "invalid device id")
		return
	}
	n, err := store.UnsubscribePush(r.Context(), h.Store.Pool, auth.ActorFrom(r.Context()).KeyID, id, p.ID)
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
