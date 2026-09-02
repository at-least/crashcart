package api

import (
	"net/http"
	"time"

	"github.com/at-least/crashcart/internal/db/sqlc"
)

// monitorCheckInsPageSize bounds getMonitor's check-in history: enough to
// see recent runs without paging (a monitor's own detail page, not the
// project-wide event list).
const monitorCheckInsPageSize = 100

// monitorOut is sqlc.Monitor with last_status flattened to a plain
// nullable string — the generated NullCheckinStatus has no MarshalJSON
// and would otherwise serialize as {"checkin_status":"ok","valid":true}.
type monitorOut struct {
	Slug                 string     `json:"slug"`
	ScheduleType         string     `json:"schedule_type"`
	ScheduleValue        string     `json:"schedule_value"`
	ScheduleUnit         *string    `json:"schedule_unit,omitempty"`
	Timezone             string     `json:"timezone"`
	CheckinMarginMin     int32      `json:"checkin_margin_min"`
	MaxRuntimeMin        int32      `json:"max_runtime_min"`
	FailureThreshold     int32      `json:"failure_threshold"`
	RecoveryThreshold    int32      `json:"recovery_threshold"`
	LastStatus           *string    `json:"last_status,omitempty"`
	ConsecutiveFailures  int32      `json:"consecutive_failures"`
	ConsecutiveSuccesses int32      `json:"consecutive_successes"`
	Alerting             bool       `json:"alerting"`
	NextExpectedAt       *time.Time `json:"next_expected_at,omitempty"`
	LastCheckinAt        *time.Time `json:"last_checkin_at,omitempty"`
	CreatedAt            time.Time  `json:"created_at"`
}

func toMonitorOut(m sqlc.Monitor) monitorOut {
	out := monitorOut{
		Slug: m.Slug, ScheduleType: m.ScheduleType, ScheduleValue: m.ScheduleValue, ScheduleUnit: m.ScheduleUnit,
		Timezone: m.Timezone, CheckinMarginMin: m.CheckinMarginMin, MaxRuntimeMin: m.MaxRuntimeMin,
		FailureThreshold: m.FailureThreshold, RecoveryThreshold: m.RecoveryThreshold,
		ConsecutiveFailures: m.ConsecutiveFailures, ConsecutiveSuccesses: m.ConsecutiveSuccesses, Alerting: m.Alerting,
		NextExpectedAt: m.NextExpectedAt, LastCheckinAt: m.LastCheckinAt, CreatedAt: m.CreatedAt,
	}
	if m.LastStatus.Valid {
		s := string(m.LastStatus.CheckinStatus)
		out.LastStatus = &s
	}
	return out
}

// listMonitors is GET /api/projects/{slug}/monitors: every monitor the
// project's SDKs have upserted a schedule for, alphabetical.
func (h *Handler) listMonitors(w http.ResponseWriter, r *http.Request) {
	p, ok := h.project(w, r)
	if !ok {
		return
	}
	rows, err := h.Store.ListMonitors(r.Context(), p.ID)
	if err != nil {
		h.fail(w, err)
		return
	}
	out := make([]monitorOut, 0, len(rows))
	for _, m := range rows {
		out = append(out, toMonitorOut(m))
	}
	writeJSON(w, http.StatusOK, map[string]any{"monitors": out})
}

// getMonitor is GET /api/projects/{slug}/monitors/{monitor}: config,
// state, and its most recent check-ins.
func (h *Handler) getMonitor(w http.ResponseWriter, r *http.Request) {
	p, ok := h.project(w, r)
	if !ok {
		return
	}
	slug := r.PathValue("monitor")
	m, err := h.Store.GetMonitor(r.Context(), sqlc.GetMonitorParams{ProjectID: p.ID, Slug: slug})
	if err != nil {
		h.fail(w, err)
		return
	}
	checkIns, err := h.Store.ListCheckIns(r.Context(), sqlc.ListCheckInsParams{ProjectID: p.ID, MonitorSlug: slug, Limit: monitorCheckInsPageSize})
	if err != nil {
		h.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"monitor": toMonitorOut(m), "check_ins": checkIns})
}

// deleteMonitor is DELETE /api/projects/{slug}/monitors/{monitor}: the
// only mutation — monitors are otherwise created only by the SDK's own
// monitor_config upsert.
func (h *Handler) deleteMonitor(w http.ResponseWriter, r *http.Request) {
	p, ok := h.project(w, r)
	if !ok {
		return
	}
	n, err := h.Store.DeleteMonitor(r.Context(), sqlc.DeleteMonitorParams{ProjectID: p.ID, Slug: r.PathValue("monitor")})
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
