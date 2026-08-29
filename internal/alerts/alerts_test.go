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
		ProjectID: p.ID, Fingerprint: "abc123", Title: "NullPointerException in CartFragment", Level: "error",
		EventCount: 3, StoredCount: 3, FirstSeen: id, LastSeen: id, FirstRelease: &rel,
	}); err != nil {
		t.Fatal(err)
	}
	if err := n.Issue(ctx, p.ID, TypeNewIssue, "abc123"); err != nil {
		t.Fatal(err)
	}
	if s.count() != 1 {
		t.Fatalf("payloads = %d", s.count())
	}
	got := s.payloads[0]
	if got.Type != TypeNewIssue || got.Project != "Shop" || got.ProjectSlug != "shop" || got.Title != "NullPointerException in CartFragment" ||
		got.Fingerprint != "abc123" || got.Level != "error" || got.EventCount != 3 || got.FirstRelease == nil || *got.FirstRelease != rel ||
		got.URL != "https://crash.example.com/p/shop/issues/abc123" || s.paths[0] != "/hook" {
		t.Errorf("payload = %+v path=%s", got, s.paths[0])
	}
	// Cooling down: nothing sent, no error.
	if err := n.Issue(ctx, p.ID, TypeNewIssue, "abc123"); err != nil || s.count() != 1 {
		t.Fatalf("cooldown: err=%v count=%d", err, s.count())
	}
	// A different rule type is independent.
	if err := n.Issue(ctx, p.ID, TypeRegression, "abc123"); err != nil || s.count() != 2 || s.payloads[1].Type != TypeRegression {
		t.Fatalf("regression: err=%v count=%d", err, s.count())
	}
	// Disabled rule: nothing.
	if _, err := st.UpsertAlertRule(ctx, sqlc.UpsertAlertRuleParams{ProjectID: p.ID, Type: TypeRegression, Enabled: false, CooldownMinutes: 0}); err != nil {
		t.Fatal(err)
	}
	if err := n.Issue(ctx, p.ID, TypeRegression, "abc123"); err != nil || s.count() != 2 {
		t.Fatalf("disabled: err=%v count=%d", err, s.count())
	}
	// Unknown issue after the cooldown was claimed: nil, nothing sent.
	if _, err := st.UpsertAlertRule(ctx, sqlc.UpsertAlertRuleParams{ProjectID: p.ID, Type: TypeNewIssue, Enabled: true, CooldownMinutes: 0}); err != nil {
		t.Fatal(err)
	}
	if err := n.Issue(ctx, p.ID, TypeNewIssue, "missing"); err != nil || s.count() != 2 {
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
	st.UpsertIssue(ctx, sqlc.UpsertIssueParams{ProjectID: p.ID, Fingerprint: "f", Title: "T", Level: "error", EventCount: 1, StoredCount: 1, FirstSeen: id, LastSeen: id})
	if err := n.Issue(ctx, p.ID, TypeNewIssue, "f"); err != nil {
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
	st.UpsertIssue(ctx, sqlc.UpsertIssueParams{ProjectID: p.ID, Fingerprint: "tg", Title: "Crash in Checkout", Level: "fatal", EventCount: 1, StoredCount: 1, FirstSeen: id, LastSeen: id})
	if err := n.Issue(ctx, p.ID, TypeNewIssue, "tg"); err != nil {
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
	if text == "" || !strings.Contains(text, "New issue in Shop") || !strings.Contains(text, "Crash in Checkout") || !strings.Contains(text, "/p/shop/issues/tg") {
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
	fp := "crashfp"
	for i := 0; i < 12; i++ {
		rows = append(rows, store.EventInsert{
			OccurredAt: time.Now().UTC().Add(-time.Duration(i) * time.Second), ProjectID: p.ID, EventID: fmt.Sprint("e", i), Level: "fatal",
			Message: "boom", Handled: &f, Fingerprint: &fp, Tags: []byte("{}"), Breadcrumbs: []byte("[]"), Payload: []byte("{}"),
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
