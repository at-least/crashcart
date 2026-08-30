package alerts

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/crashcartapp/crashcart/internal/config"
	"github.com/crashcartapp/crashcart/internal/db/sqlc"
	"github.com/crashcartapp/crashcart/internal/sentry"
	"github.com/crashcartapp/crashcart/internal/store"
	"github.com/crashcartapp/crashcart/internal/testdb"
)

type sink struct {
	mu       sync.Mutex
	payloads []Payload
	paths    []string
	bodies   []map[string]any
}

func (s *sink) server() *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
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
}

func (s *sink) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.payloads)
}

func setup(t *testing.T) (*store.Store, sqlc.Project, *sink, *Notifier) {
	t.Helper()
	st := testdb.New(t)
	ctx := context.Background()
	p, err := st.CreateProject(ctx, sqlc.CreateProjectParams{Slug: "shop", Name: "Shop", PublicKey: fmt.Sprint(time.Now().UnixNano())})
	if err != nil {
		t.Fatal(err)
	}
	if err := EnsureRules(ctx, st, p.ID); err != nil {
		t.Fatal(err)
	}
	rules, _ := st.ListAlertRules(ctx, p.ID)
	if len(rules) != 3 {
		t.Fatalf("rules = %+v", rules)
	}
	if err := EnsureRules(ctx, st, p.ID); err != nil {
		t.Fatal(err)
	}
	s := &sink{}
	srv := s.server()
	t.Cleanup(srv.Close)
	cfg, _ := json.Marshal(map[string]string{"url": srv.URL + "/hook"})
	if _, err := st.CreateAlertChannel(ctx, sqlc.CreateAlertChannelParams{ProjectID: p.ID, Kind: "webhook", Config: cfg}); err != nil {
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
	if _, err := st.UpsertIssue(ctx, sqlc.UpsertIssueParams{
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
	if _, err := st.UpsertAlertRule(ctx, sqlc.UpsertAlertRuleParams{ProjectID: p.ID, Type: TypeRegression, Enabled: false, CooldownMinutes: 0}); err != nil {
		t.Fatal(err)
	}
	if err := n.Issue(ctx, p.ID, TypeRegression, string(sentry.DerivedID([]byte("abc123")))); err != nil || s.count() != 2 {
		t.Fatalf("disabled: err=%v count=%d", err, s.count())
	}
	// Unknown issue after the cooldown was claimed: nil, nothing sent.
	if _, err := st.UpsertAlertRule(ctx, sqlc.UpsertAlertRuleParams{ProjectID: p.ID, Type: TypeNewIssue, Enabled: true, CooldownMinutes: 0}); err != nil {
		t.Fatal(err)
	}
	if err := n.Issue(ctx, p.ID, TypeNewIssue, string(sentry.DerivedID([]byte("missing")))); err != nil || s.count() != 2 {
		t.Fatalf("missing issue: err=%v count=%d", err, s.count())
	}
}

func TestWebhookFailureIsBestEffort(t *testing.T) {
	st, p, _, n := setup(t)
	ctx := context.Background()
	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.Error(w, "nope", 500) }))
	defer bad.Close()
	cfg, _ := json.Marshal(map[string]string{"url": bad.URL})
	if _, err := st.CreateAlertChannel(ctx, sqlc.CreateAlertChannelParams{ProjectID: p.ID, Kind: "webhook", Config: cfg}); err != nil {
		t.Fatal(err)
	}
	id := time.Now().UTC()
	st.UpsertIssue(ctx, sqlc.UpsertIssueParams{ProjectID: p.ID, Fingerprint: sentry.DerivedID([]byte("f")), Title: "T", Level: "error", EventCount: 1, StoredCount: 1, FirstSeen: id, LastSeen: id})
	if err := n.Issue(ctx, p.ID, TypeNewIssue, string(sentry.DerivedID([]byte("f")))); err != nil {
		t.Fatalf("channel failures must not fail the job: %v", err)
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
	if _, err := st.CreateAlertChannel(ctx, sqlc.CreateAlertChannelParams{ProjectID: p.ID, Kind: "telegram", Config: cfg}); err != nil {
		t.Fatal(err)
	}
	id := time.Now().UTC()
	st.UpsertIssue(ctx, sqlc.UpsertIssueParams{ProjectID: p.ID, Fingerprint: sentry.DerivedID([]byte("tg")), Title: "Crash in Checkout", Level: "fatal", EventCount: 1, StoredCount: 1, FirstSeen: id, LastSeen: id})
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
	if text == "" || !strings.Contains(text, "New issue in Shop") || !strings.Contains(text, "Crash in Checkout") || !strings.Contains(text, "/p/shop/issues/"+string(sentry.DerivedID([]byte("tg")))) {
		t.Errorf("text = %q", text)
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
	// 12 crashes in the current hourly bucket on top of an empty baseline
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
	err := st.Tx(ctx, func(ctx context.Context, tx pgx.Tx, q *sqlc.Queries) error { return store.InsertEvents(ctx, tx, rows) })
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
	if got.Type != TypeCrashSpike || got.Recent == nil || *got.Recent != 12 || got.Baseline == nil || *got.Baseline != 0 || got.ProjectSlug != "shop" {
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
	err := n.send(context.Background(), sqlc.AlertChannel{Kind: "telegram", Config: cfg}, Payload{Title: "x"})
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
	good, _ := st.ListAlertChannels(ctx, p.ID)
	for _, ch := range good {
		st.DeleteAlertChannel(ctx, sqlc.DeleteAlertChannelParams{ProjectID: p.ID, ID: ch.ID})
	}
	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.Error(w, "nope", 500) }))
	defer bad.Close()
	cfg, _ := json.Marshal(map[string]string{"url": bad.URL})
	badCh, err := st.CreateAlertChannel(ctx, sqlc.CreateAlertChannelParams{ProjectID: p.ID, Kind: "webhook", Config: cfg})
	if err != nil {
		t.Fatal(err)
	}
	id := time.Now().UTC()
	fp := sentry.DerivedID([]byte("gb"))
	st.UpsertIssue(ctx, sqlc.UpsertIssueParams{ProjectID: p.ID, Fingerprint: fp, Title: "T", Level: "error", EventCount: 1, StoredCount: 1, FirstSeen: id, LastSeen: id})
	if err := n.Issue(ctx, p.ID, TypeNewIssue, string(fp)); err != nil {
		t.Fatal(err)
	}
	rule, _ := st.GetAlertRule(ctx, sqlc.GetAlertRuleParams{ProjectID: p.ID, Type: TypeNewIssue})
	if rule.LastTriggered != nil {
		t.Fatalf("cooldown kept although nothing was delivered: %v", rule.LastTriggered)
	}
	// The endpoint comes back: the next alert goes out at once.
	st.DeleteAlertChannel(ctx, sqlc.DeleteAlertChannelParams{ProjectID: p.ID, ID: badCh.ID})
	for _, ch := range good {
		st.CreateAlertChannel(ctx, sqlc.CreateAlertChannelParams{ProjectID: p.ID, Kind: ch.Kind, Config: ch.Config})
	}
	if err := n.Issue(ctx, p.ID, TypeNewIssue, string(fp)); err != nil || s.count() != 1 {
		t.Fatalf("after recovery: err=%v sent=%d", err, s.count())
	}
	rule, _ = st.GetAlertRule(ctx, sqlc.GetAlertRuleParams{ProjectID: p.ID, Type: TypeNewIssue})
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
		st.UpsertIssue(ctx, sqlc.UpsertIssueParams{ProjectID: p.ID, Fingerprint: fp, Title: "T", Level: "error", EventCount: 1, StoredCount: 1, FirstSeen: at, LastSeen: at})
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
