package api

import (
	"net/http"
	"time"

	"github.com/crashcartapp/crashcart/internal/store"
)

type clientReportCountOut struct {
	Reason   string `json:"reason"`
	Category string `json:"category"`
	Quantity int64  `json:"quantity"`
}

type clientReportsOut struct {
	From   time.Time              `json:"from"`
	To     time.Time              `json:"to"`
	Counts []clientReportCountOut `json:"counts"`
}

// listClientReports is GET /api/projects/{slug}/client_reports: how many
// events the project's SDKs discarded client-side over a window, summed
// by reason and category — largest first. Windowed like overview, not
// paged like user_reports: this is a small summed breakdown, not a
// growing list of individual facts.
func (h *Handler) listClientReports(w http.ResponseWriter, r *http.Request) {
	p, ok := h.project(w, r)
	if !ok {
		return
	}
	from, to, err := ParseWindow(r.URL.Query())
	if err != nil {
		h.fail(w, err)
		return
	}
	rows, err := store.ListClientReportCounts(r.Context(), h.Store.Pool, p.ID, from.Truncate(time.Hour), to)
	if err != nil {
		h.fail(w, err)
		return
	}
	out := clientReportsOut{From: from, To: to, Counts: make([]clientReportCountOut, 0, len(rows))}
	for _, r := range rows {
		out.Counts = append(out.Counts, clientReportCountOut{Reason: r.Reason, Category: r.Category, Quantity: r.Quantity})
	}
	writeJSON(w, http.StatusOK, out)
}
