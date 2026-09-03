package web

import (
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/crashcartapp/crashcart/internal/store"
)

// Poll cadence of the SSE endpoint (variables so tests can shorten them).
// With a Listener the poll is only the fallback, so it is slower.
var (
	streamPoll      = 5 * time.Second
	streamWakePoll  = 60 * time.Second
	streamKeepAlive = 15 * time.Second
)

// stream is GET /p/{slug}/stream?since=<RFC3339>: on every issue
// notification for the project (and every poll) it counts issues first
// seen after `since` plus current regressions and emits an `issues` event
// when the pair changes. Comments keep the connection alive.
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
	// The regression count uses the page's window (win=), like the
	// baseline it is compared with.
	win := ParseViewState(p.Slug, r.URL.Query()).Window(since)
	h := rw.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache")
	h.Set("X-Accel-Buffering", "no")
	rw.WriteHeader(http.StatusOK)
	fmt.Fprint(rw, ": connected\n\n")
	flusher.Flush()

	ctx := r.Context()
	interval := streamPoll
	var wake <-chan string
	if w.Listener != nil {
		var stop func()
		wake, stop = w.Listener.Subscribe(store.ChannelIssues, strconv.FormatInt(p.ID, 10))
		defer stop()
		interval = streamWakePoll
	}
	poll := time.NewTicker(interval)
	defer poll.Stop()
	keep := time.NewTicker(streamKeepAlive)
	defer keep.Stop()
	last := ""
	for {
		select {
		case <-ctx.Done():
			return
		case <-w.Stopping:
			return
		case <-keep.C:
			fmt.Fprint(rw, ": ping\n\n")
			flusher.Flush()
			continue
		case <-wake:
		case <-poll.C:
		}
		n, err := store.CountNewIssues(ctx, w.Store.Pool, p.ID, since)
		if err != nil {
			return
		}
		m, err := store.CountRegressions(ctx, w.Store.Pool, p.ID, win.From)
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
