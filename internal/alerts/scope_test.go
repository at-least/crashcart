package alerts

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/at-least/crashcart/internal/config"
	"github.com/at-least/crashcart/internal/db/sqlc"
	"github.com/at-least/crashcart/internal/sentry"
	"github.com/at-least/crashcart/internal/store"
	"github.com/at-least/crashcart/internal/testdb"
)

// TestEnsureRulesDefaults: the four default rules, enabled, 60 min
// cooldown; a second EnsureRules keeps an edited row.
func TestEnsureRulesDefaults(t *testing.T) {
	st := testdb.New(t)
	ctx := context.Background()
	p, err := st.CreateProject(ctx, sqlc.CreateProjectParams{Slug: "r", Name: "R", PublicKey: "k"})
	if err != nil {
		t.Fatal(err)
	}
	if err := EnsureRules(ctx, st, p.ID); err != nil {
		t.Fatal(err)
	}
	rules, _ := st.ListAlertRules(ctx, p.ID)
	got := map[string]sqlc.AlertRule{}
	for _, r := range rules {
		got[string(r.Type)] = r
	}
	for _, typ := range []string{TypeNewIssue, TypeRegression, TypeUnhandledSpike, TypeEscalating} {
		r, ok := got[typ]
		if !ok || !r.Enabled || r.CooldownMinutes != defaultCooldown {
			t.Errorf("%s: %+v (present %v)", typ, r, ok)
		}
	}
	if len(rules) != 6 {
		t.Errorf("rules = %d", len(rules))
	}
	if _, err := st.UpsertAlertRule(ctx, sqlc.UpsertAlertRuleParams{ProjectID: p.ID, Type: TypeNewIssue, Enabled: false, CooldownMinutes: 5}); err != nil {
		t.Fatal(err)
	}
	if err := EnsureRules(ctx, st, p.ID); err != nil {
		t.Fatal(err)
	}
	r, _ := st.GetAlertRule(ctx, sqlc.GetAlertRuleParams{ProjectID: p.ID, Type: TypeNewIssue})
	if r.Enabled || r.CooldownMinutes != 5 {
		t.Errorf("EnsureRules overwrote an existing rule: %+v", r)
	}
}

func insertUnhandled(t *testing.T, st *store.Store, projectID int64, n int, ago time.Duration, handled bool, seed string) {
	t.Helper()
	rows := make([]store.EventInsert, 0, n)
	fp := sentry.DerivedID([]byte("fp" + seed))
	h := handled
	for i := 0; i < n; i++ {
		rows = append(rows, store.EventInsert{
			OccurredAt: time.Now().UTC().Add(-ago).Add(-time.Duration(i) * time.Second), ProjectID: projectID,
			EventID: sentry.DerivedID([]byte(fmt.Sprint(seed, projectID, ago, i))), Level: "fatal", Message: "boom", Handled: &h, Fingerprint: &fp, Tags: []byte("{}"),
		})
	}
	if err := st.Tx(context.Background(), func(ctx context.Context, tx pgx.Tx, q *sqlc.Queries) error { return store.InsertEvents(ctx, tx, rows) }); err != nil {
		t.Fatal(err)
	}
}

// TestCheckSpikesScope: the spike rule counts only unhandled events, only
// the exact last hour, and per project — another project's burst does not
// alert this one, and a burst two hours ago is baseline, not recent.
func TestCheckSpikesScope(t *testing.T) {
	st, p, s, n := setup(t)
	ctx := context.Background()
	other, err := st.CreateProject(ctx, sqlc.CreateProjectParams{Slug: "other", Name: "Other", PublicKey: "k2"})
	if err != nil {
		t.Fatal(err)
	}
	// Handled errors in a burst: not a spike.
	insertUnhandled(t, st, p.ID, 12, 0, true, "handled")
	// Unhandled, but 130 minutes ago: outside the recent hour, inside the
	// baseline (a full hourly bucket before the recent hour's bucket).
	insertUnhandled(t, st, p.ID, 12, 130*time.Minute, false, "old")
	// Unhandled in the partial bucket between the baseline (full hours)
	// and the recent hour: on neither side. That bucket is [truncate(now-1h),
	// now-1h) — it exists only a few minutes into each hour.
	if partial := time.Now().UTC().Add(-time.Hour); partial.Minute() >= 3 {
		insertUnhandled(t, st, p.ID, 12, time.Since(partial.Truncate(time.Hour).Add(time.Minute)), false, "partial")
	}
	if err := n.CheckSpikes(ctx); err != nil || s.count() != 0 {
		t.Fatalf("handled / old events alerted: err=%v count=%d", err, s.count())
	}
	// The other project spikes: only it is alerted (it has no channel, so
	// nothing arrives at the sink — its rule is claimed instead).
	insertUnhandled(t, st, other.ID, 12, 0, false, "burst")
	if err := n.CheckSpikes(ctx); err != nil {
		t.Fatal(err)
	}
	if s.count() != 0 {
		t.Fatalf("shop alerted for other's burst: %+v", s.payloads)
	}
	// Give it a channel and reset its claim: the payload names the other project with its own numbers.
	cfg, _ := json.Marshal(map[string]string{"url": s.serverURL})
	if _, err := st.CreateAlertChannel(ctx, sqlc.CreateAlertChannelParams{ProjectID: other.ID, Kind: "webhook", Config: cfg}); err != nil {
		t.Fatal(err)
	}
	if err := n.CheckSpikes(ctx); err != nil || s.count() != 1 {
		t.Fatalf("other's spike: err=%v count=%d", err, s.count())
	}
	if got := s.payloads[0]; got.ProjectSlug != "other" || got.Recent == nil || *got.Recent != 12 || *got.Baseline != 0 {
		t.Fatalf("payload = %+v", got)
	}
	// Now shop gets 12 unhandled in the last hour on top of its 12 two
	// hours earlier: mean 0.5/h, recent 12 → spike, with baseline 12 (the
	// partial bucket's 12 count on neither side).
	insertUnhandled(t, st, p.ID, 12, 0, false, "now")
	if err := n.CheckSpikes(ctx); err != nil || s.count() != 2 {
		t.Fatalf("shop's spike: err=%v count=%d", err, s.count())
	}
	if got := s.payloads[1]; got.ProjectSlug != "shop" || *got.Recent != 12 || *got.Baseline != 12 {
		t.Fatalf("payload = %+v", got)
	}
}

// TestCheckIgnoredCountNotReached: an issue ignored "until N more events"
// stays ignored one event short of N and comes back on the Nth.
func TestCheckIgnoredCountNotReached(t *testing.T) {
	st, p, _, n := setup(t)
	ctx := context.Background()
	at := time.Now().UTC()
	fp := sentry.DerivedID([]byte("cnt"))
	more := func(k int64) {
		t.Helper()
		if _, err := st.UpsertIssue(ctx, sqlc.UpsertIssueParams{ProjectID: p.ID, Fingerprint: fp, Title: "cnt", Level: "error", EventCount: k, StoredCount: 0, FirstSeen: at, LastSeen: at}); err != nil {
			t.Fatal(err)
		}
	}
	more(10)
	five := int64(5)
	if _, err := st.SetIssueStatus(ctx, sqlc.SetIssueStatusParams{ProjectID: p.ID, Fingerprint: fp, Status: "ignored", IgnoreEvents: &five}); err != nil {
		t.Fatal(err)
	}
	more(4)
	if err := n.CheckIgnored(ctx); err != nil {
		t.Fatal(err)
	}
	if is, _ := st.GetIssue(ctx, sqlc.GetIssueParams{ProjectID: p.ID, Fingerprint: fp}); is.Status != "ignored" || is.IgnoreUntilCount == nil || *is.IgnoreUntilCount != 15 {
		t.Fatalf("one short: %+v", is)
	}
	more(1)
	if err := n.CheckIgnored(ctx); err != nil {
		t.Fatal(err)
	}
	if is, _ := st.GetIssue(ctx, sqlc.GetIssueParams{ProjectID: p.ID, Fingerprint: fp}); is.Status != "unresolved" || is.IgnoreUntilCount != nil {
		t.Fatalf("on the Nth: %+v", is)
	}
}

// TestWebhookClientChecksAfterDNS: a hostname that resolves inward
// ("localhost") is refused at connect time, after resolution — the URL
// itself passed no literal-address check.
func TestWebhookClientChecksAfterDNS(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { t.Error("connected") }))
	defer srv.Close()
	_, port, _ := net.SplitHostPort(srv.Listener.Addr().String())
	n := &Notifier{Cfg: config.Config{WebhookAllowPrivate: true}}
	cfg, _ := json.Marshal(map[string]string{"url": "http://localhost:" + port + "/hook"})
	err := n.send(context.Background(), sqlc.AlertChannel{Kind: "webhook", Config: cfg}, Payload{Title: "x"})
	if !errors.Is(err, ErrBlockedURL) {
		t.Fatalf("localhost by name: %v (want ErrBlockedURL)", err)
	}
}

// privateAddr finds a private, non-loopback IPv4 address of this host: the
// only target the hardened client will connect to in a test (loopback is
// refused always), and only with WEBHOOK_ALLOW_PRIVATE.
func privateAddr(t *testing.T) string {
	t.Helper()
	addrs, _ := net.InterfaceAddrs()
	for _, a := range addrs {
		ipn, ok := a.(*net.IPNet)
		if !ok {
			continue
		}
		ip, ok := netip.AddrFromSlice(ipn.IP)
		if !ok {
			continue
		}
		ip = ip.Unmap()
		if ip.Is4() && ip.IsPrivate() && !ip.IsLoopback() && !ip.IsLinkLocalUnicast() {
			return ip.String()
		}
	}
	t.Skip("no private IPv4 interface on this host")
	return ""
}

// TestWebhookClientPrivateAndRedirects: a private target is refused at
// connect time unless WEBHOOK_ALLOW_PRIVATE; with it allowed, a redirect
// is still not followed — the second hop never receives a request.
func TestWebhookClientPrivateAndRedirects(t *testing.T) {
	ip := privateAddr(t)
	var hits, redirected atomic.Int32
	l, err := net.Listen("tcp", net.JoinHostPort(ip, "0"))
	if err != nil {
		t.Skip("cannot listen on", ip, err)
	}
	srv := &httptest.Server{Listener: l, Config: &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/hook":
			hits.Add(1)
			http.Redirect(w, r, "/elsewhere", http.StatusFound)
		case "/elsewhere":
			redirected.Add(1)
			w.WriteHeader(200)
		}
	})}}
	srv.Start()
	defer srv.Close()
	cfg, _ := json.Marshal(map[string]string{"url": srv.URL + "/hook"})

	n := &Notifier{Cfg: config.Config{}}
	if err := n.send(context.Background(), sqlc.AlertChannel{Kind: "webhook", Config: cfg}, Payload{Title: "x"}); !errors.Is(err, ErrBlockedURL) {
		t.Fatalf("private target without WEBHOOK_ALLOW_PRIVATE: %v", err)
	}
	if hits.Load() != 0 {
		t.Fatal("the refused target was connected")
	}
	n = &Notifier{Cfg: config.Config{WebhookAllowPrivate: true}}
	err = n.send(context.Background(), sqlc.AlertChannel{Kind: "webhook", Config: cfg}, Payload{Title: "x"})
	if !errors.Is(err, ErrBlockedURL) {
		t.Fatalf("redirect: %v (want ErrBlockedURL)", err)
	}
	if hits.Load() != 1 || redirected.Load() != 0 {
		t.Fatalf("hits=%d redirected=%d (want 1, 0)", hits.Load(), redirected.Load())
	}
}
