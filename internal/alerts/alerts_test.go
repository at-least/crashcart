package alerts

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/crashcartapp/crashcart/internal/config"
	"github.com/crashcartapp/crashcart/internal/sentry"
	"github.com/crashcartapp/crashcart/internal/store"
	"github.com/crashcartapp/crashcart/internal/testdb"
)

type sink struct {
	mu        sync.Mutex
	payloads  []Payload
	paths     []string
	bodies    []map[string]any
	serverURL string // set by server()
}

func (s *sink) server() *httptest.Server {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		defer s.mu.Unlock()
		var body map[string]any
		json.NewDecoder(r.Body).Decode(&body)
		s.bodies = append(s.bodies, body)
		s.paths = append(s.paths, r.URL.Path)
		var p Payload
		b, _ := json.Marshal(body)
		json.Unmarshal(b, &p)
		s.payloads = append(s.payloads, p)
		w.WriteHeader(http.StatusOK)
	}))
	if s.serverURL == "" {
		s.serverURL = srv.URL + "/hook"
	}
	return srv
}

func (s *sink) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.payloads)
}

func setup(t *testing.T) (*store.Store, store.Project, *sink, *Notifier) {
	t.Helper()
	st := testdb.New(t)
	ctx := context.Background()
	p, err := store.CreateProject(ctx, st.Pool, "shop", "Shop", nil, fmt.Sprint(time.Now().UnixNano()))
	if err != nil {
		t.Fatal(err)
	}
	if err := EnsureRules(ctx, st, p.ID); err != nil {
		t.Fatal(err)
	}
	rules, _ := store.ListAlertRules(ctx, st.Pool, p.ID)
	if len(rules) != 6 {
		t.Fatalf("rules = %+v", rules)
	}
	if err := EnsureRules(ctx, st, p.ID); err != nil {
		t.Fatal(err)
	}
	s := &sink{}
	srv := s.server()
	t.Cleanup(srv.Close)
	cfg, _ := json.Marshal(map[string]string{"url": srv.URL + "/hook"})
	if _, err := store.CreateAlertChannel(ctx, st.Pool, p.ID, "webhook", cfg); err != nil {
		t.Fatal(err)
	}
	n := &Notifier{Store: st, Cfg: config.Config{PublicURL: "https://crash.example.com"}, HTTP: srv.Client()}
	return st, p, s, n
}

func TestIssueWebhookAndCooldown(t *testing.T) {
	st, p, s, n := setup(t)
	ctx := context.Background()
	rel := "1.2.3"
	id := time.Now().UTC()
	if _, err := store.UpsertIssue(ctx, st.Pool, store.UpsertIssueParams{
		ProjectID: p.ID, Fingerprint: sentry.DerivedID([]byte("abc123")), Title: "NullPointerException in CartFragment", Level: "error",
		EventCount: 3, StoredCount: 3, FirstSeen: id, LastSeen: id, FirstRelease: &rel,
	}); err != nil {
		t.Fatal(err)
	}
	if err := n.Issue(ctx, p.ID, TypeNewIssue, string(sentry.DerivedID([]byte("abc123")))); err != nil {
		t.Fatal(err)
	}
	if s.count() != 1 {
		t.Fatalf("payloads = %d", s.count())
	}
	abc := sentry.DerivedID([]byte("abc123"))
	got := s.payloads[0]
	if got.Type != TypeNewIssue || got.Project != "Shop" || got.ProjectSlug != "shop" || got.Title != "NullPointerException in CartFragment" ||
		got.Fingerprint != string(abc) || got.Level != "error" || got.EventCount != 3 || got.FirstRelease == nil || *got.FirstRelease != rel ||
		got.URL != "https://crash.example.com/p/shop/issues/"+string(abc) || s.paths[0] != "/hook" {
		t.Errorf("payload = %+v path=%s", got, s.paths[0])
	}
	// Cooling down: nothing sent, no error.
	if err := n.Issue(ctx, p.ID, TypeNewIssue, string(sentry.DerivedID([]byte("abc123")))); err != nil || s.count() != 1 {
		t.Fatalf("cooldown: err=%v count=%d", err, s.count())
	}
	// A different rule type is independent.
	if err := n.Issue(ctx, p.ID, TypeRegression, string(sentry.DerivedID([]byte("abc123")))); err != nil || s.count() != 2 || s.payloads[1].Type != TypeRegression {
		t.Fatalf("regression: err=%v count=%d", err, s.count())
	}
	// Disabled rule: nothing.
	if _, err := store.UpsertAlertRule(ctx, st.Pool, p.ID, TypeRegression, false, 0); err != nil {
		t.Fatal(err)
	}
	if err := n.Issue(ctx, p.ID, TypeRegression, string(sentry.DerivedID([]byte("abc123")))); err != nil || s.count() != 2 {
		t.Fatalf("disabled: err=%v count=%d", err, s.count())
	}
	// Unknown issue after the cooldown was claimed: nil, nothing sent.
	if _, err := store.UpsertAlertRule(ctx, st.Pool, p.ID, TypeNewIssue, true, 0); err != nil {
		t.Fatal(err)
	}
	if err := n.Issue(ctx, p.ID, TypeNewIssue, string(sentry.DerivedID([]byte("missing")))); err != nil || s.count() != 2 {
		t.Fatalf("missing issue: err=%v count=%d", err, s.count())
	}
}

func TestTelegram(t *testing.T) {
	st, p, s, n := setup(t)
	ctx := context.Background()
	tg := s.server()
	defer tg.Close()
	old := TelegramAPI
	TelegramAPI = tg.URL
	t.Cleanup(func() { TelegramAPI = old })
	n.Cfg.TelegramBotToken = "123:ABC"
	cfg, _ := json.Marshal(map[string]string{"chat_id": "-100"})
	if _, err := store.CreateAlertChannel(ctx, st.Pool, p.ID, "telegram", cfg); err != nil {
		t.Fatal(err)
	}
	id := time.Now().UTC()
	store.UpsertIssue(ctx, st.Pool, store.UpsertIssueParams{ProjectID: p.ID, Fingerprint: sentry.DerivedID([]byte("tg")), Title: "NSRangeException: index 3 beyond bounds", Level: "fatal", EventCount: 1, StoredCount: 1, FirstSeen: id, LastSeen: id})
	if err := n.Issue(ctx, p.ID, TypeNewIssue, string(sentry.DerivedID([]byte("tg")))); err != nil {
		t.Fatal(err)
	}
	if s.count() != 2 {
		t.Fatalf("expected webhook + telegram, got %d", s.count())
	}
	var tgBody map[string]any
	for i, path := range s.paths {
		if path == "/bot123:ABC/sendMessage" {
			tgBody = s.bodies[i]
		}
	}
	if tgBody == nil || tgBody["chat_id"] != "-100" {
		t.Fatalf("telegram call = %v %v", s.paths, s.bodies)
	}
	text, _ := tgBody["text"].(string)
	if text == "" || !strings.Contains(text, "New issue in Shop") || !strings.Contains(text, "NSRangeException: index 3 beyond bounds") || !strings.Contains(text, "/p/shop/issues/"+string(sentry.DerivedID([]byte("tg")))) {
		t.Errorf("text = %q", text)
	}
}

// TestSlack: a slack channel posts to its own URL (like webhook, unlike
// telegram's bot-token API endpoint) with a plain {"text": ...} body.
func TestSlack(t *testing.T) {
	st, p, s, n := setup(t)
	ctx := context.Background()
	sl := s.server()
	defer sl.Close()
	cfg, _ := json.Marshal(map[string]string{"url": sl.URL + "/slack"})
	if _, err := store.CreateAlertChannel(ctx, st.Pool, p.ID, "slack", cfg); err != nil {
		t.Fatal(err)
	}
	id := time.Now().UTC()
	fp := sentry.DerivedID([]byte("slack"))
	store.UpsertIssue(ctx, st.Pool, store.UpsertIssueParams{ProjectID: p.ID, Fingerprint: fp, Title: "NullPointerException: <init> failed & died", Level: "fatal", EventCount: 1, StoredCount: 1, FirstSeen: id, LastSeen: id})
	if err := n.Issue(ctx, p.ID, TypeNewIssue, string(fp)); err != nil {
		t.Fatal(err)
	}
	if s.count() != 2 {
		t.Fatalf("expected webhook + slack, got %d", s.count())
	}
	var slBody map[string]any
	for i, path := range s.paths {
		if path == "/slack" {
			slBody = s.bodies[i]
		}
	}
	text, _ := slBody["text"].(string)
	if text == "" {
		t.Fatalf("slack call = %v %v", s.paths, s.bodies)
	}
	if strings.Contains(text, "<init>") || strings.Contains(text, "& died") {
		t.Errorf("title not escaped for slack mrkdwn: %q", text)
	}
	if !strings.Contains(text, "&lt;init&gt;") || !strings.Contains(text, "failed &amp; died") {
		t.Errorf("expected escaped title, got %q", text)
	}
	if !strings.Contains(text, "/p/shop/issues/"+string(fp)) {
		t.Errorf("expected an unescaped link line, got %q", text)
	}
}

func TestSlackTextEscaping(t *testing.T) {
	p := Payload{Type: TypeNewIssue, Project: "Shop", Title: "A <b>bold</b> & broken title", URL: "https://x.example.com/p/shop"}
	got := SlackText(p)
	if strings.Contains(got, "<b>") || strings.Contains(got, "broken title\"") {
		t.Errorf("raw markup leaked: %q", got)
	}
	if !strings.Contains(got, "&lt;b&gt;bold&lt;/b&gt; &amp; broken title") {
		t.Errorf("escaped title missing, got %q", got)
	}
	if !strings.Contains(got, "https://x.example.com/p/shop") {
		t.Errorf("url should not be escaped, got %q", got)
	}
	// TelegramText on the same payload stays unescaped.
	if tg := TelegramText(p); !strings.Contains(tg, "<b>bold</b> & broken title") {
		t.Errorf("telegram text should stay unescaped, got %q", tg)
	}
}

func TestChannelConfigSlack(t *testing.T) {
	get := map[string]string{"url": "https://hooks.slack.com/services/T000/B000/XXXX"}
	cfg, err := ChannelConfig("slack", func(k string) string { return get[k] }, false)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]string
	json.Unmarshal(cfg, &m)
	if m["url"] != get["url"] {
		t.Errorf("config = %v", m)
	}
	if _, err := ChannelConfig("slack", func(string) string { return "not-a-url" }, false); err == nil {
		t.Fatal("expected the same URL validation webhook gets")
	}
}

func TestCheckSpikes(t *testing.T) {
	st, p, s, n := setup(t)
	ctx := context.Background()
	if !IsSpike(10, 0) || IsSpike(9, 0) || IsSpike(29, 240) || !IsSpike(30, 240) {
		t.Fatal("IsSpike thresholds")
	}
	// Quiet project: nothing.
	if err := n.CheckSpikes(ctx); err != nil || s.count() != 0 {
		t.Fatalf("quiet: err=%v count=%d", err, s.count())
	}
	// 12 unhandled in the current hourly bucket on top of an empty baseline
	// (seconds apart, so they cannot straddle a bucket boundary).
	rows := make([]store.EventInsert, 0, 12)
	f := false
	fp := sentry.DerivedID([]byte("crashfp"))
	for i := 0; i < 12; i++ {
		rows = append(rows, store.EventInsert{
			OccurredAt: time.Now().UTC().Add(-time.Duration(i) * time.Second), ProjectID: p.ID, EventID: sentry.DerivedID([]byte(fmt.Sprint("e", i))), Level: "fatal",
			Message: "boom", Handled: &f, Fingerprint: &fp, Tags: []byte("{}"),
		})
	}
	err := st.Tx(ctx, func(ctx context.Context, tx pgx.Tx) error { return st.InsertEvents(ctx, tx, rows) })
	if err != nil {
		t.Fatal(err)
	}
	if err := n.CheckSpikes(ctx); err != nil {
		t.Fatal(err)
	}
	if s.count() != 1 {
		t.Fatalf("payloads = %d", s.count())
	}
	got := s.payloads[0]
	if got.Type != TypeUnhandledSpike || got.Recent == nil || *got.Recent != 12 || got.Baseline == nil || *got.Baseline != 0 || got.ProjectSlug != "shop" {
		t.Errorf("payload = %+v", got)
	}
	// Cooldown holds on the next check.
	if err := n.CheckSpikes(ctx); err != nil || s.count() != 1 {
		t.Fatalf("cooldown: err=%v count=%d", err, s.count())
	}
}

// TestTelegramErrorHidesToken: a transport failure's error must not carry
// the request URL (it holds the bot token) into the log.
func TestTelegramErrorHidesToken(t *testing.T) {
	n := &Notifier{Cfg: config.Config{TelegramBotToken: "123:SECRET"}, HTTP: &http.Client{Timeout: time.Second}}
	old := TelegramAPI
	TelegramAPI = "http://127.0.0.1:1" // nothing listens
	t.Cleanup(func() { TelegramAPI = old })
	cfg, _ := json.Marshal(map[string]string{"chat_id": "-100"})
	err := n.send(context.Background(), store.AlertChannel{Kind: "telegram", Config: cfg}, Payload{Title: "x"})
	if err == nil {
		t.Fatal("expected a transport error")
	}
	if strings.Contains(err.Error(), "SECRET") {
		t.Fatalf("token leaked in error: %v", err)
	}
}

// TestCooldownGivenBackWhenNothingDelivered: a claim that delivered to no
// channel is returned, so the outage does not also eat the next alert.
func TestCooldownGivenBackWhenNothingDelivered(t *testing.T) {
	st, p, s, n := setup(t)
	ctx := context.Background()
	good, _ := store.ListAlertChannels(ctx, st.Pool, p.ID)
	for _, ch := range good {
		store.DeleteAlertChannel(ctx, st.Pool, p.ID, ch.ID)
	}
	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.Error(w, "nope", 500) }))
	defer bad.Close()
	cfg, _ := json.Marshal(map[string]string{"url": bad.URL})
	badCh, err := store.CreateAlertChannel(ctx, st.Pool, p.ID, "webhook", cfg)
	if err != nil {
		t.Fatal(err)
	}
	id := time.Now().UTC()
	fp := sentry.DerivedID([]byte("gb"))
	store.UpsertIssue(ctx, st.Pool, store.UpsertIssueParams{ProjectID: p.ID, Fingerprint: fp, Title: "T", Level: "error", EventCount: 1, StoredCount: 1, FirstSeen: id, LastSeen: id})
	if err := n.Issue(ctx, p.ID, TypeNewIssue, string(fp)); err != nil {
		t.Fatal(err)
	}
	rule, _ := store.GetAlertRule(ctx, st.Pool, p.ID, TypeNewIssue)
	if rule.LastTriggered != nil {
		t.Fatalf("cooldown kept although nothing was delivered: %v", rule.LastTriggered)
	}
	// The endpoint comes back: the next alert goes out at once.
	store.DeleteAlertChannel(ctx, st.Pool, p.ID, badCh.ID)
	for _, ch := range good {
		store.CreateAlertChannel(ctx, st.Pool, p.ID, ch.Kind, ch.Config)
	}
	if err := n.Issue(ctx, p.ID, TypeNewIssue, string(fp)); err != nil || s.count() != 1 {
		t.Fatalf("after recovery: err=%v sent=%d", err, s.count())
	}
	rule, _ = store.GetAlertRule(ctx, st.Pool, p.ID, TypeNewIssue)
	if rule.LastTriggered == nil {
		t.Fatal("cooldown not claimed after a delivery")
	}
}

// TestNewIssueAlertCountsSuppressed: the cooldown is per project, so the
// alert that gets through says how many other new issues it stands for.
func TestNewIssueAlertCountsSuppressed(t *testing.T) {
	st, p, s, n := setup(t)
	ctx := context.Background()
	past := time.Now().UTC().Add(-2 * time.Hour)
	if _, err := st.Pool.Exec(ctx, "UPDATE alert_rules SET last_triggered = $1 WHERE project_id = $2 AND type = 'new_issue'", past, p.ID); err != nil {
		t.Fatal(err)
	}
	var fp sentry.ID
	for i := 0; i < 4; i++ {
		fp = sentry.DerivedID([]byte{byte(i)})
		at := time.Now().UTC().Add(-time.Duration(4-i) * time.Minute)
		store.UpsertIssue(ctx, st.Pool, store.UpsertIssueParams{ProjectID: p.ID, Fingerprint: fp, Title: "T", Level: "error", EventCount: 1, StoredCount: 1, FirstSeen: at, LastSeen: at})
	}
	if err := n.Issue(ctx, p.ID, TypeNewIssue, string(fp)); err != nil || s.count() != 1 {
		t.Fatalf("err=%v sent=%d", err, s.count())
	}
	if got := s.payloads[0].MoreSinceLast; got == nil || *got != 3 {
		t.Fatalf("more_since_last = %v, want 3", got)
	}
	if !strings.Contains(TelegramText(s.payloads[0]), "+3 more new issues") {
		t.Errorf("telegram text: %q", TelegramText(s.payloads[0]))
	}
}

func TestValidateWebhookURL(t *testing.T) {
	cases := map[string]bool{ // url → allowed (private off)
		"https://hooks.example.com/x":             true,
		"http://203.0.113.9/hook":                 true,
		"ftp://hooks.example.com/x":               false,
		"https:///nohost":                         false,
		"http://localhost:8080/x":                 false,
		"http://app.localhost/x":                  false,
		"http://127.0.0.1/x":                      false,
		"http://[::1]/x":                          false,
		"http://169.254.169.254/latest/meta-data": false,
		"http://10.1.2.3/hook":                    false,
		"http://192.168.1.10/hook":                false,
		"http://[fd00::1]/hook":                   false,
		"http://0.0.0.0/x":                        false,
	}
	for u, ok := range cases {
		if err := ValidateWebhookURL(u, false); (err == nil) != ok {
			t.Errorf("%s: err=%v, want allowed=%v", u, err, ok)
		}
	}
	// Private ranges only with WEBHOOK_ALLOW_PRIVATE; loopback and
	// link-local never.
	if err := ValidateWebhookURL("http://10.1.2.3/hook", true); err != nil {
		t.Errorf("private with allowPrivate: %v", err)
	}
	if err := ValidateWebhookURL("http://169.254.169.254/x", true); !errors.Is(err, ErrBlockedURL) {
		t.Errorf("link-local with allowPrivate must stay blocked: %v", err)
	}
}

// TestWebhookClientBlocksLoopback: the hardened client refuses the
// connection after resolution (a name pointing inward is caught here),
// and does not follow redirects.
func TestWebhookClientBlocksLoopback(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(200) }))
	defer srv.Close()
	n := &Notifier{Cfg: config.Config{}} // HTTP nil: the real client
	cfg, _ := json.Marshal(map[string]string{"url": srv.URL + "/hook"})
	err := n.send(context.Background(), store.AlertChannel{Kind: "webhook", Config: cfg}, Payload{Title: "x"})
	if !errors.Is(err, ErrBlockedURL) {
		t.Fatalf("loopback webhook: %v (want ErrBlockedURL)", err)
	}
	// With private targets allowed, loopback is still refused.
	n = &Notifier{Cfg: config.Config{WebhookAllowPrivate: true}}
	if err := n.send(context.Background(), store.AlertChannel{Kind: "webhook", Config: cfg}, Payload{Title: "x"}); !errors.Is(err, ErrBlockedURL) {
		t.Fatalf("loopback with allowPrivate: %v", err)
	}
}

// TestCheckIgnored: an ignored issue comes back when its time passes, its
// count is reached, or it escalates (with an alert).
func TestCheckIgnored(t *testing.T) {
	st, p, s, n := setup(t)
	ctx := context.Background()
	at := time.Now().UTC()
	mk := func(name string, count int64) sentry.ID {
		fp := sentry.DerivedID([]byte(name))
		if _, err := store.UpsertIssue(ctx, st.Pool, store.UpsertIssueParams{ProjectID: p.ID, Fingerprint: fp, Title: name, Level: "error", EventCount: count, StoredCount: count, FirstSeen: at, LastSeen: at}); err != nil {
			t.Fatal(err)
		}
		return fp
	}
	insert := func(fp sentry.ID, n int, ago time.Duration) {
		rows := make([]store.EventInsert, 0, n)
		for i := 0; i < n; i++ {
			rows = append(rows, store.EventInsert{
				OccurredAt: at.Add(-ago).Add(-time.Duration(i) * time.Second), ProjectID: p.ID, EventID: sentry.DerivedID([]byte(fmt.Sprint(fp, ago, i))),
				Level: "error", Message: "boom", Fingerprint: &fp, Tags: []byte("{}"),
			})
		}
		if err := st.Tx(ctx, func(ctx context.Context, tx pgx.Tx) error { return st.InsertEvents(ctx, tx, rows) }); err != nil {
			t.Fatal(err)
		}
	}
	set := func(fp sentry.ID, pr store.SetIssueStatusParams) store.Issue {
		pr.ProjectID, pr.Fingerprint, pr.Status = p.ID, fp, "ignored"
		is, err := store.SetIssueStatus(ctx, st.Pool, pr)
		if err != nil {
			t.Fatal(err)
		}
		return is
	}
	status := func(fp sentry.ID) store.Issue {
		is, err := store.GetIssue(ctx, st.Pool, p.ID, fp)
		if err != nil {
			t.Fatal(err)
		}
		return is
	}
	past, future, five := at.Add(-time.Minute), at.Add(time.Hour), int64(5)
	timed, later, counted, esc, busy, forever := mk("timed", 1), mk("later", 1), mk("counted", 10), mk("esc", 10), mk("busy", 24), mk("forever", 1)
	insert(busy, 24, 2*time.Hour) // its baseline: 24 stored events in the 24 h before now
	set(timed, store.SetIssueStatusParams{IgnoreUntil: &past})
	set(later, store.SetIssueStatusParams{IgnoreUntil: &future})
	if is := set(counted, store.SetIssueStatusParams{IgnoreEvents: &five}); is.IgnoreUntilCount == nil || *is.IgnoreUntilCount != 15 {
		t.Fatalf("counted: %+v", is)
	}
	if is := set(esc, store.SetIssueStatusParams{IgnoreEscalating: true}); !is.IgnoreUntilEscalating || is.IgnoreBaseline == nil || *is.IgnoreBaseline != 0 {
		t.Fatalf("esc: %+v", is)
	}
	if is := set(busy, store.SetIssueStatusParams{IgnoreEscalating: true}); is.IgnoreBaseline == nil || *is.IgnoreBaseline != 24 {
		t.Fatalf("busy baseline: %+v", is)
	}
	set(forever, store.SetIssueStatusParams{})
	// Five more events reach counted's threshold.
	if _, err := store.UpsertIssue(ctx, st.Pool, store.UpsertIssueParams{ProjectID: p.ID, Fingerprint: counted, Title: "counted", Level: "error", EventCount: 5, StoredCount: 0, FirstSeen: at, LastSeen: at}); err != nil {
		t.Fatal(err)
	}
	if err := n.CheckIgnored(ctx); err != nil {
		t.Fatal(err)
	}
	if is := status(timed); is.Status != "unresolved" || is.IgnoreUntil != nil {
		t.Errorf("timed: %+v", is)
	}
	if is := status(later); is.Status != "ignored" {
		t.Errorf("later: %+v", is)
	}
	if is := status(counted); is.Status != "unresolved" || is.IgnoreUntilCount != nil {
		t.Errorf("counted: %+v", is)
	}
	for _, fp := range []sentry.ID{esc, busy, forever} {
		if is := status(fp); is.Status != "ignored" {
			t.Errorf("%s: %+v", is.Title, is)
		}
	}
	if s.count() != 0 {
		t.Fatalf("alerts = %d, want 0", s.count())
	}
	// esc: 12 events in the last hour on a baseline of 0 → escalates, one
	// alert. busy: 3 in the last hour on 24/day → not a spike.
	insert(esc, 12, 0)
	insert(busy, 3, 0)
	if err := n.CheckIgnored(ctx); err != nil {
		t.Fatal(err)
	}
	if is := status(esc); is.Status != "unresolved" || is.IgnoreUntilEscalating || is.IgnoreBaseline != nil {
		t.Errorf("esc after: %+v", is)
	}
	if is := status(busy); is.Status != "ignored" {
		t.Errorf("busy after: %+v", is)
	}
	if s.count() != 1 {
		t.Fatalf("alerts = %d, want 1", s.count())
	}
	got := s.payloads[0]
	if got.Type != TypeEscalating || got.Fingerprint != string(esc) || got.Recent == nil || *got.Recent != 12 || got.Baseline == nil || *got.Baseline != 0 ||
		got.Title != "esc" || !strings.HasSuffix(got.URL, "/p/shop/issues/"+string(esc)) {
		t.Errorf("payload = %+v", got)
	}
	if txt := TelegramText(got); !strings.Contains(txt, "Escalating in Shop") || !strings.Contains(txt, "12 in the last hour") {
		t.Errorf("telegram text = %q", txt)
	}
	// Nothing left to do.
	if err := n.CheckIgnored(ctx); err != nil || s.count() != 1 {
		t.Fatalf("second run: err=%v alerts=%d", err, s.count())
	}
}
