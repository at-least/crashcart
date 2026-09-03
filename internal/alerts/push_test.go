package alerts

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/crashcartapp/crashcart/internal/config"
	"github.com/crashcartapp/crashcart/internal/sentry"
	"github.com/crashcartapp/crashcart/internal/store"
	"github.com/crashcartapp/crashcart/internal/testdb"
)

// fcmSink is an FCM HTTP v1 stand-in: it records every send request and
// answers ok unless told to fail.
type fcmSink struct {
	mu    sync.Mutex
	paths []string
	auths []string
	sent  []map[string]any
	fail  bool
}

func (f *fcmSink) server() *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		f.paths = append(f.paths, r.URL.Path)
		f.auths = append(f.auths, r.Header.Get("Authorization"))
		var body map[string]any
		json.NewDecoder(r.Body).Decode(&body)
		f.sent = append(f.sent, body)
		if f.fail {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
}

func (f *fcmSink) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.sent)
}

func setupPush(t *testing.T) (*store.Store, store.Project, *fcmSink, *Notifier) {
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
	key, err := store.CreateAPIKey(ctx, st.Pool, "phone", []byte("h"), "cc_1", nil)
	if err != nil {
		t.Fatal(err)
	}
	d, err := store.UpsertPushDevice(ctx, st.Pool, key.ID, "device-token", "ios")
	if err != nil {
		t.Fatal(err)
	}
	if ok, err := store.SubscribePush(ctx, st.Pool, key.ID, d.ID, p.ID); err != nil || !ok {
		t.Fatalf("subscribe: ok=%v err=%v", ok, err)
	}
	f := &fcmSink{}
	srv := f.server()
	t.Cleanup(srv.Close)
	FCMEndpoint = srv.URL
	t.Cleanup(func() { FCMEndpoint = "https://fcm.googleapis.com" })
	n := &Notifier{
		Store: st, Cfg: config.Config{PublicURL: "https://crash.example.com"}, HTTP: srv.Client(),
		FCMToken: func(context.Context) (string, string, error) { return "test-access-token", "cc-project", nil },
	}
	return st, p, f, n
}

func TestIssuePush(t *testing.T) {
	st, p, f, n := setupPush(t)
	ctx := context.Background()
	fp := sentry.DerivedID([]byte("push1"))
	id := time.Now().UTC()
	if _, err := store.UpsertIssue(ctx, st.Pool, store.UpsertIssueParams{
		ProjectID: p.ID, Fingerprint: fp, Title: "Crash on launch", Level: "fatal", EventCount: 1, StoredCount: 1, FirstSeen: id, LastSeen: id,
	}); err != nil {
		t.Fatal(err)
	}
	if err := n.Issue(ctx, p.ID, TypeNewIssue, string(fp)); err != nil {
		t.Fatal(err)
	}
	if f.count() != 1 {
		t.Fatalf("push sent = %d, want 1", f.count())
	}
	if f.paths[0] != "/v1/projects/cc-project/messages:send" {
		t.Errorf("path = %s", f.paths[0])
	}
	if f.auths[0] != "Bearer test-access-token" {
		t.Errorf("authorization = %s", f.auths[0])
	}
	msg, _ := f.sent[0]["message"].(map[string]any)
	if msg["token"] != "device-token" {
		t.Errorf("message.token = %v", msg["token"])
	}
	notif, _ := msg["notification"].(map[string]any)
	if notif["title"] == "" || notif["body"] == "" {
		t.Errorf("notification = %v", notif)
	}
	data, _ := msg["data"].(map[string]any)
	if data["fingerprint"] != string(fp) || data["project_slug"] != "shop" || data["type"] != TypeNewIssue {
		t.Errorf("data = %v", data)
	}
}

// TestIssuePushFailureReleasesCooldown: an FCM error is logged, not fatal,
// and — like a failed webhook — must not eat the cooldown when nothing
// else could be delivered, so the next real alert is not silently dropped.
func TestIssuePushFailureReleasesCooldown(t *testing.T) {
	st, p, f, n := setupPush(t)
	f.fail = true
	ctx := context.Background()
	fp := sentry.DerivedID([]byte("push2"))
	id := time.Now().UTC()
	if _, err := store.UpsertIssue(ctx, st.Pool, store.UpsertIssueParams{
		ProjectID: p.ID, Fingerprint: fp, Title: "Crash", Level: "fatal", EventCount: 1, StoredCount: 1, FirstSeen: id, LastSeen: id,
	}); err != nil {
		t.Fatal(err)
	}
	if err := n.Issue(ctx, p.ID, TypeNewIssue, string(fp)); err != nil {
		t.Fatal(err)
	}
	rule, err := store.GetAlertRule(ctx, st.Pool, p.ID, TypeNewIssue)
	if err != nil {
		t.Fatal(err)
	}
	if rule.LastTriggered != nil {
		t.Errorf("cooldown claimed despite total delivery failure: %+v", rule)
	}
}

// TestPushNoDevices: a project with no subscribers sends nothing and
// requires no FCM configuration at all.
func TestPushNoDevices(t *testing.T) {
	st := testdb.New(t)
	ctx := context.Background()
	p, err := store.CreateProject(ctx, st.Pool, "solo", "Solo", nil, fmt.Sprint(time.Now().UnixNano()))
	if err != nil {
		t.Fatal(err)
	}
	if err := EnsureRules(ctx, st, p.ID); err != nil {
		t.Fatal(err)
	}
	fp := sentry.DerivedID([]byte("push3"))
	id := time.Now().UTC()
	if _, err := store.UpsertIssue(ctx, st.Pool, store.UpsertIssueParams{
		ProjectID: p.ID, Fingerprint: fp, Title: "Crash", Level: "fatal", EventCount: 1, StoredCount: 1, FirstSeen: id, LastSeen: id,
	}); err != nil {
		t.Fatal(err)
	}
	n := &Notifier{Store: st, Cfg: config.Config{PublicURL: "https://crash.example.com"}}
	if err := n.Issue(ctx, p.ID, TypeNewIssue, string(fp)); err != nil {
		t.Fatal(err)
	}
}
