package web

import (
	"fmt"
	"net/http"
	"time"

	"github.com/crashcartapp/crashcart/internal/db/sqlc"
)

// Poll cadence of the SSE endpoint (variables so tests can shorten them).
var (
	streamPoll      = 5 * time.Second
	streamKeepAlive = 15 * time.Second
)

// stream is GET /p/{slug}/stream?since=<RFC3339>: every poll it counts issues
// first seen after `since` plus current regressions and emits an `issues`
// event when the pair changes. Comments keep the connection alive.
func (w *Web) stream(rw http.ResponseWriter, r *http.Request) {
	p, ok := w.project(rw, r)
	if !ok {
		return
	}
	flusher, ok := rw.(http.Flusher)
	if !ok {
		http.Error(rw, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	since, err := time.Parse(time.RFC3339Nano, r.URL.Query().Get("since"))
	if err != nil {
		http.Error(rw, "since must be RFC3339", http.StatusBadRequest)
		return
	}
	h := rw.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache")
	h.Set("X-Accel-Buffering", "no")
	rw.WriteHeader(http.StatusOK)
	fmt.Fprint(rw, ": connected\n\n")
	flusher.Flush()

	ctx := r.Context()
	poll := time.NewTicker(streamPoll)
	defer poll.Stop()
	keep := time.NewTicker(streamKeepAlive)
	defer keep.Stop()
	last := ""
	for {
		select {
		case <-ctx.Done():
			return
		case <-keep.C:
			fmt.Fprint(rw, ": ping\n\n")
			flusher.Flush()
		case <-poll.C:
			n, err := w.Store.CountNewIssues(ctx, sqlc.CountNewIssuesParams{ProjectID: p.ID, FirstSeen: since})
			if err != nil {
				return
			}
			m, err := w.Store.CountRegressions(ctx, sqlc.CountRegressionsParams{ProjectID: p.ID})
			if err != nil {
				return
			}
			data := fmt.Sprintf(`{"new":%d,"regressions":%d}`, n, m)
			if data == last {
				continue
			}
			last = data
			fmt.Fprintf(rw, "event: issues\ndata: %s\n\n", data)
			flusher.Flush()
		}
	}
}
