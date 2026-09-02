package web

import (
	"context"
	"log/slog"
	mrand "math/rand"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/crashcartapp/crashcart/internal/config"
	"github.com/crashcartapp/crashcart/internal/db/sqlc"
	"github.com/crashcartapp/crashcart/internal/ingest"
	"github.com/crashcartapp/crashcart/internal/sentry"
	"github.com/crashcartapp/crashcart/internal/store"
)

// TestIssuePageHeader: the culprit sits under the title, and the badge
// follows exception.mechanism.handled — unhandled, handled, or none when
// the event carries no mechanism.
func TestIssuePageHeader(t *testing.T) {
	w, p, mux := setup(t)
	ctx := context.Background()
	in := &ingest.Ingester{Store: w.Store, Cfg: config.Config{}, Log: slog.Default()}
	n := time.Now().UTC()
	ts := n.Add(-5 * time.Minute).Format(time.RFC3339)
	ingestOne := func(id, body string) sentry.ID {
		t.Helper()
		env := "{\"event_id\":\"" + id + "\"}\n{\"type\":\"event\"}\n" + body + "\n"
		res, err := in.Ingest(ctx, p, sentry.Parse([]byte(env), n), n)
		if err != nil || res.Stored != 1 || len(res.NewIssues) != 1 {
			t.Fatalf("ingest %s: %+v %v", id, res, err)
		}
		return res.NewIssues[0]
	}
	handled := ingestOne(strings.Repeat("1", 32), `{"event_id":"`+strings.Repeat("1", 32)+`","timestamp":"`+ts+`","level":"error","platform":"android","release":"2.4.1","exception":{"values":[{"type":"IOException","value":"timeout","mechanism":{"type":"generic","handled":true},"stacktrace":{"frames":[{"module":"com.example.Net","function":"fetch","filename":"Net.kt","lineno":7,"in_app":true}]}}]}}`)
	noMech := ingestOne(strings.Repeat("2", 32), `{"event_id":"`+strings.Repeat("2", 32)+`","timestamp":"`+ts+`","level":"error","platform":"android","release":"2.4.1","exception":{"values":[{"type":"IllegalStateException","value":"closed","stacktrace":{"frames":[{"module":"com.example.Cart","function":"pay","filename":"Cart.kt","lineno":3,"in_app":true}]}}]}}`)

	// The issue from setup: unhandled NullPointerException.
	is, _, _ := w.Store.ListIssues(ctx, storeIssueFilter(p.ID))
	var unhandled sentry.ID
	for _, i := range is {
		if i.Title == "NullPointerException: Attempt to invoke virtual method" {
			unhandled = i.Fingerprint
		}
	}
	if unhandled == "" {
		t.Fatalf("setup issue missing: %+v", is)
	}
	badge := func(kind string) string { return `data-variant="outline" class="font-mono">` + kind + `</span>` }
	body := assertPage(t, mux, "/p/shop/issues/"+string(unhandled), badge("unhandled"))
	if strings.Contains(body, badge("handled")) {
		t.Error("unhandled issue shows a handled badge")
	}
	if i, j := strings.Index(body, "NullPointerException: Attempt to invoke virtual method"), strings.Index(body, "com/example/CartFragment.java in onCreateView"); i < 0 || j < 0 || j < i {
		t.Errorf("culprit must come after the title (title at %d, culprit at %d)", i, j)
	}
	body = assertPage(t, mux, "/p/shop/issues/"+string(handled), badge("handled"), "IOException: timeout", "com.example.Net in fetch")
	if strings.Contains(body, badge("unhandled")) {
		t.Error("handled issue shows an unhandled badge")
	}
	body = assertPage(t, mux, "/p/shop/issues/"+string(noMech), "IllegalStateException: closed", "com.example.Cart in pay")
	if strings.Contains(body, badge("handled")) || strings.Contains(body, badge("unhandled")) {
		t.Error("an event without a mechanism must show neither badge")
	}
	// The culprit is Sentry's "module in function": no line number.
	if strings.Contains(body, "com.example.Cart in pay:3") || strings.Contains(body, "Cart.kt:3 in pay") {
		t.Error("culprit carries a line number")
	}
	if ev, err := w.Store.GetEvent(ctx, sqlc.GetEventParams{ProjectID: p.ID, EventID: sentry.ID(strings.Repeat("2", 32))}); err != nil || ev.Culprit == nil || *ev.Culprit != "com.example.Cart in pay" {
		t.Errorf("event culprit = %v %v", ev.Culprit, err)
	}
}

// TestStreamWakesOnOwnProjectOnly: with a Listener the stream is woken by
// crashcart_issues notifications carrying its project id — another
// project's notification (or a foreign payload) does not produce an event.
func TestStreamWakesOnOwnProjectOnly(t *testing.T) {
	w, _, mux := setup(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	// NOTIFY channels are per database, not per schema: the other packages'
	// tests share this database and their issue triggers raise
	// crashcart_issues with *their* small sequential project ids, so the
	// streamed project gets an id no other test schema will produce.
	// (In production there is one schema, so ids are unique by construction.)
	id := int64(1)<<40 + mrand.Int63n(1<<30)
	if _, err := w.Store.Pool.Exec(ctx, `INSERT INTO projects (id, slug, name, public_key) OVERRIDING SYSTEM VALUE VALUES ($1, 'stream', 'Stream', $2)`, id, strings.Repeat("f", 32)); err != nil {
		t.Fatal(err)
	}
	p, err := w.Store.GetProject(ctx, "stream")
	if err != nil {
		t.Fatal(err)
	}
	l := &store.Listener{Pool: w.Store.Pool}
	go l.Run(ctx)
	// Wait for LISTEN to be up: a subscription with no key sees everything.
	any, stopAny := l.Subscribe(store.ChannelIssues, "")
	defer stopAny()
	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, err := w.Store.Pool.Exec(ctx, "SELECT pg_notify($1, 'warmup')", store.ChannelIssues); err != nil {
			t.Fatal(err)
		}
		select {
		case <-any:
		case <-time.After(200 * time.Millisecond):
			if time.Now().After(deadline) {
				t.Fatal("listener never came up")
			}
			continue
		}
		break
	}
	w.Listener = l
	streamWakePoll, streamKeepAlive = time.Hour, time.Hour // no poll, no ping: only wake-ups produce output
	defer func() { streamWakePoll, streamKeepAlive = 60*time.Second, 15*time.Second }()

	rctx, rcancel := context.WithCancel(ctx)
	req := httptest.NewRequest("GET", "/p/stream/stream?since=2000-01-01T00%3A00%3A00Z", nil).WithContext(rctx)
	req.AddCookie(sessionCookie)
	rec := &syncRecorder{ResponseRecorder: httptest.NewRecorder()}
	done := make(chan struct{})
	go func() { mux.ServeHTTP(rec, req); close(done) }()
	time.Sleep(100 * time.Millisecond)

	notify := func(payload string) {
		t.Helper()
		select { // drop a stale wake-up so the read below is this notification's
		case <-any:
		default:
		}
		if _, err := w.Store.Pool.Exec(ctx, "SELECT pg_notify($1, $2)", store.ChannelIssues, payload); err != nil {
			t.Fatal(err)
		}
		<-any // the unkeyed subscription proves the notification arrived in this process
		time.Sleep(100 * time.Millisecond)
	}
	other := p.ID + 1
	notify(strconvI64(other))
	notify("garbage")
	if b := rec.String(); strings.Contains(b, "event: issues") {
		t.Fatalf("stream woke on another project's notification: %q", b)
	}
	notify(strconvI64(p.ID))
	rcancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("stream did not exit")
	}
	if b := rec.String(); !strings.Contains(b, "event: issues\ndata: {\"new\":0,\"regressions\":0}\n\n") {
		t.Fatalf("stream did not wake on its own project: %q", b)
	}
}

func strconvI64(n int64) string { return i64(n) }

// syncRecorder is a ResponseRecorder the test may read while the handler
// is still writing.
type syncRecorder struct {
	mu sync.Mutex
	*httptest.ResponseRecorder
}

func (r *syncRecorder) Write(b []byte) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.ResponseRecorder.Write(b)
}

func (r *syncRecorder) WriteHeader(c int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.ResponseRecorder.WriteHeader(c)
}

func (r *syncRecorder) Flush() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.ResponseRecorder.Flush()
}

func (r *syncRecorder) String() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.Body.String()
}
