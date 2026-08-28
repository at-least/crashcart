package server

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/newlix/crashcart/internal/alerts"
	"github.com/newlix/crashcart/internal/config"
	"github.com/newlix/crashcart/internal/retention"
	"github.com/newlix/crashcart/internal/store"
	"github.com/newlix/crashcart/internal/testdb"
)

const apiKey = "sk-test"
const ingestToken = "ingest-secret"

type env struct {
	srv *httptest.Server
	st  *store.Store
	cfg config.Config
}

func newEnv(t *testing.T, mutate func(*config.Config)) *env {
	t.Helper()
	pool := testdb.Pool(t)
	st := store.New(pool)
	cfg := config.Config{
		DatabaseURL: "unused", APIKeys: []string{apiKey}, IngestToken: ingestToken, RateLimit: 0,
		CORSOrigin: "*", SampleRate: 1, RetentionDays: 30, CustomTags: config.ParseCustomTags("build:Build"),
	}
	if mutate != nil {
		mutate(&cfg)
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	srv := httptest.NewServer(New(cfg, st, log))
	t.Cleanup(srv.Close)
	return &env{srv: srv, st: st, cfg: cfg}
}

func (e *env) do(t *testing.T, method, path string, body []byte, headers map[string]string) (*http.Response, []byte) {
	t.Helper()
	req, err := http.NewRequest(method, e.srv.URL+path, bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp, b
}

func (e *env) api(t *testing.T, method, path string, body []byte) (*http.Response, []byte) {
	t.Helper()
	return e.do(t, method, path, body, map[string]string{"Authorization": "Bearer " + apiKey, "Content-Type": "application/json"})
}

func (e *env) ingest(t *testing.T, items ...string) (*http.Response, []byte) {
	t.Helper()
	body := "{\"event_id\":\"abcdef0123456789abcdef0123456789\",\"sent_at\":\"" + time.Now().UTC().Format(time.RFC3339) + "\"}\n" + strings.Join(items, "\n") + "\n"
	return e.do(t, "POST", "/ingest?token="+ingestToken, []byte(body), nil)
}

func event(ts time.Time, level, errType, release, user, device string, handled *bool, extra string) string {
	h := "null"
	if handled != nil {
		h = fmt.Sprint(*handled)
	}
	exc := ""
	if errType != "" {
		exc = fmt.Sprintf(`,"exception":{"values":[{"type":%q,"value":"boom","mechanism":{"handled":%s},"stacktrace":{"frames":[{"filename":"a/Main.kt","function":"run","in_app":true,"lineno":7}]}}]}`, errType, h)
	}
	return fmt.Sprintf(`{"type":"event"}
{"timestamp":%q,"level":%q,"platform":"android","release":%q,"transaction":"Home","user":{"id":%q},"tags":{"device_id":%q,"build":"42"},"contexts":{"device":{"model":"Pixel"},"os":{"version":"14"}},"breadcrumbs":{"values":[{"timestamp":%q,"category":"ui","message":"tap"}]}%s%s}`,
		ts.UTC().Format(time.RFC3339), level, release, user, device, ts.UTC().Format(time.RFC3339), exc, extra)
}

func bp(b bool) *bool { return &b }

func decode[T any](t *testing.T, b []byte) T {
	t.Helper()
	var v T
	if err := json.Unmarshal(b, &v); err != nil {
		t.Fatalf("decode %s: %v", b, err)
	}
	return v
}

func TestIngestAndQuery(t *testing.T) {
	e := newEnv(t, nil)
	now := time.Now().UTC().Truncate(time.Second)

	// auth
	if resp, _ := e.do(t, "POST", "/ingest", nil, nil); resp.StatusCode != 401 {
		t.Fatalf("ingest without token: %d", resp.StatusCode)
	}
	if resp, _ := e.do(t, "GET", "/api/events", nil, nil); resp.StatusCode != 401 {
		t.Fatalf("api without key: %d", resp.StatusCode)
	}
	if resp, _ := e.do(t, "POST", "/ingest?token="+ingestToken, []byte("{}\n"), nil); resp.StatusCode != 400 {
		t.Fatalf("empty envelope: %d", resp.StatusCode)
	}

	resp, body := e.ingest(t,
		event(now.Add(-2*time.Minute), "fatal", "NullPointerException", "1.0.0", "user-1", "dev-1", bp(false), ""),
		event(now.Add(-1*time.Minute), "error", "IOException", "1.0.0", "user-2", "dev-2", bp(true), ""),
		event(now.Add(-30*time.Second), "info", "", "1.0.1", "user-1", "dev-1", nil, `,"message":"hello world"`),
		event(now.Add(-20*time.Second), "error", "NullPointerException", "1.0.1", "", "dev-3", bp(false), ""),
		`{"type":"session"}`, `{"sid":"s1","status":"crashed","release":"1.0.1","started":"`+now.Format(time.RFC3339)+`"}`,
		`{"type":"session"}`, `{"sid":"s2","status":"exited","release":"1.0.1","started":"`+now.Format(time.RFC3339)+`"}`,
	)
	if resp.StatusCode != 200 {
		t.Fatalf("ingest: %d %s", resp.StatusCode, body)
	}
	res := decode[map[string]int](t, body)
	if res["events"] != 4 || res["sessions"] != 2 {
		t.Fatalf("ingest result = %v", res)
	}

	// Sentry-native path + X-Sentry-Auth header
	resp, body = e.do(t, "POST", "/api/1/envelope/", []byte("{}\n{\"type\":\"event\"}\n{\"message\":\"native\",\"level\":\"info\"}\n"),
		map[string]string{"X-Sentry-Auth": "Sentry sentry_version=7, sentry_key=" + ingestToken})
	if resp.StatusCode != 200 {
		t.Fatalf("native envelope: %d %s", resp.StatusCode, body)
	}

	// events list, newest first, no payload
	_, body = e.api(t, "GET", "/api/events", nil)
	events := decode[[]map[string]any](t, body)
	if len(events) != 5 {
		t.Fatalf("events = %d", len(events))
	}
	if events[0]["message"] != "native" || events[1]["error_type"] != "NullPointerException" {
		t.Errorf("order wrong: %v %v", events[0]["message"], events[1]["error_type"])
	}
	if _, ok := events[0]["payload"]; ok {
		t.Error("list must not include payload")
	}
	if events[1]["error_location"] != "Main.kt:7" || events[1]["fingerprint"] == nil {
		t.Errorf("analysis missing: %v", events[1])
	}

	// filters
	check := func(q string, want int) {
		t.Helper()
		_, b := e.api(t, "GET", "/api/events?"+q, nil)
		if got := len(decode[[]map[string]any](t, b)); got != want {
			t.Errorf("%s: got %d want %d", q, got, want)
		}
	}
	check("level=fatal,error", 3)
	check("crash=1", 2)
	check("release=1.0.1", 2)
	check("error_type=NullPointerException", 2)
	check("user_id=user-1", 2) // direct + via device mapping
	check("device_id=dev-3", 1)
	check("tag.build=42", 4)
	check("tag.build=99", 0)
	check("q=HELLO", 1)
	check("error_location=main.kt", 3)
	check("limit=2", 2)
	check("limit=2&offset=4", 1)
	check("days=1", 5)
	check("from=2000-01-01T00:00:00Z&to=2000-01-02T00:00:00Z", 0)

	// detail
	id := int64(events[1]["id"].(float64))
	_, body = e.api(t, "GET", fmt.Sprintf("/api/events/%d", id), nil)
	detail := decode[map[string]any](t, body)
	if detail["payload"].(map[string]any)["platform"] != "android" || detail["tags"].(map[string]any)["build"] != "42" {
		t.Errorf("detail = %v", detail)
	}
	if resp, _ := e.api(t, "GET", "/api/events/999999", nil); resp.StatusCode != 404 {
		t.Error("missing event → 404")
	}

	// stats
	_, body = e.api(t, "GET", "/api/stats?days=1", nil)
	st := decode[store.Stats](t, body)
	if st.Fatal != 1 || st.Crash != 2 || st.Error != 1 {
		t.Errorf("stats = %+v", st)
	}
	_, body = e.api(t, "GET", "/api/stats/timeline?days=7", nil)
	tl := decode[[]store.Point](t, body)
	if len(tl) != 7 || tl[6].Count != 2 {
		t.Errorf("timeline = %+v", tl)
	}
	_, body = e.api(t, "GET", "/api/stats/timeline?days=1&hourly=1", nil)
	if hl := decode[[]store.Point](t, body); len(hl) != 24 {
		t.Errorf("hourly timeline = %d", len(hl))
	}
	_, body = e.api(t, "GET", "/api/stats/volume?days=7", nil)
	vol := decode[[]store.VolumePoint](t, body)
	if len(vol) != 7 || vol[6].Fatal != 1 || vol[6].Error != 2 {
		t.Errorf("volume = %+v", vol)
	}
	_, body = e.api(t, "GET", "/api/stats/releases", nil)
	rels := decode[[]map[string]any](t, body)
	if len(rels) != 2 || rels[0]["version"] != "1.0.1" || rels[0]["total_events"].(float64) != 2 {
		t.Errorf("releases = %v", rels)
	}
	_, body = e.api(t, "GET", "/api/stats/release_versions", nil)
	if v := decode[[]string](t, body); len(v) != 2 || v[0] != "1.0.1" {
		t.Errorf("versions = %v", v)
	}
	_, body = e.api(t, "GET", "/api/stats/release_health?days=7", nil)
	rh := decode[[]store.ReleaseHealth](t, body)
	if len(rh) != 1 || rh[0].TotalSessions != 2 || rh[0].CrashedSessions != 1 || rh[0].CrashFreeRate != 50 {
		t.Errorf("release health = %+v", rh)
	}

	// issues
	_, body = e.api(t, "GET", "/api/issues?days=7", nil)
	issues := decode[[]map[string]any](t, body)
	if len(issues) != 2 {
		t.Fatalf("issues = %v", issues)
	}
	var npe map[string]any
	for _, is := range issues {
		if is["error_type"] == "NullPointerException" {
			npe = is
		}
	}
	if npe["event_count"].(float64) != 2 || npe["first_release"] != "1.0.0" || npe["last_release"] != "1.0.1" || npe["title"] != "NullPointerException in Home" {
		t.Errorf("npe issue = %v", npe)
	}
	_, body = e.api(t, "GET", "/api/issues?days=7&release=1.0.0", nil)
	if got := len(decode[[]map[string]any](t, body)); got != 2 {
		t.Errorf("issues by release = %d", got)
	}
	_, body = e.api(t, "GET", "/api/issues?days=7&user_id=user-2", nil)
	if got := len(decode[[]map[string]any](t, body)); got != 1 {
		t.Errorf("issues by user = %d", got)
	}
	fp := npe["fingerprint"].(string)
	check("fingerprint="+fp, 2)

	// lifecycle: resolve, then a new release reappearance → regression
	resp, _ = e.api(t, "PATCH", "/api/issues/"+fp, []byte(`{"status":"resolved"}`))
	if resp.StatusCode != 200 {
		t.Fatalf("patch issue: %d", resp.StatusCode)
	}
	if resp, _ := e.api(t, "PATCH", "/api/issues/"+fp, []byte(`{"status":"bogus"}`)); resp.StatusCode != 400 {
		t.Error("invalid status → 400")
	}
	if resp, _ := e.api(t, "PATCH", "/api/issues/nope", []byte(`{"status":"resolved"}`)); resp.StatusCode != 404 {
		t.Error("unknown issue → 404")
	}
	e.ingest(t, event(now, "error", "NullPointerException", "1.0.2", "user-9", "dev-9", bp(false), ""))
	_, body = e.api(t, "GET", "/api/issues/"+fp, nil)
	if is := decode[map[string]any](t, body); is["status"] != "regression" || is["event_count"].(float64) != 3 {
		t.Errorf("after regression = %v", is)
	}

	// alerts
	_, body = e.api(t, "GET", "/api/alerts", nil)
	al := decode[[]map[string]any](t, body)
	if len(al) != 3 || al[0]["type"] != "crash_spike" || al[0]["enabled"] != true {
		t.Errorf("alerts = %v", al)
	}
	if resp, _ := e.api(t, "PATCH", "/api/alerts/regression", []byte(`{"enabled":true}`)); resp.StatusCode != 200 {
		t.Error("toggle alert")
	}
	if resp, _ := e.api(t, "PATCH", "/api/alerts/nope", []byte(`{"enabled":true}`)); resp.StatusCode != 400 {
		t.Error("bad alert type")
	}
	_, body = e.api(t, "GET", "/api/alerts/channels", nil)
	if ch := decode[map[string]any](t, body); ch["telegram_configured"] != false {
		t.Errorf("channels = %v", ch)
	}

	// health + rate limit headers absent when disabled
	if resp, b := e.do(t, "GET", "/health", nil, nil); resp.StatusCode != 200 || string(b) != "ok" {
		t.Errorf("health: %d %s", resp.StatusCode, b)
	}
}

func TestViewer(t *testing.T) {
	e := newEnv(t, nil)
	now := time.Now().UTC()
	e.ingest(t, event(now.Add(-time.Minute), "fatal", "NullPointerException", "1.0.0", "user-1", "dev-1", bp(false), ""))

	resp, body := e.do(t, "GET", "/", nil, nil)
	html := string(body)
	if resp.StatusCode != 200 || !strings.Contains(strings.ToLower(html), "<!doctype html>") || !strings.Contains(html, `id="shell"`) {
		t.Fatalf("dashboard page: %d", resp.StatusCode)
	}
	for _, want := range []string{"NullPointerException", `data-chart=`, `class="level-badge" data-level="fatal"`, `hx-get="/events/`, `data-slot="card"`, "Build", `/static/app.css?v=`, `id="sheet-portal"`} {
		if !strings.Contains(html, want) {
			t.Errorf("dashboard missing %q", want)
		}
	}
	// fragment for htmx swap pushes the canonical URL
	resp, body = e.do(t, "GET", "/dashboard?error_type=NullPointerException&win=24h", nil, map[string]string{"HX-Request": "true"})
	if resp.Header.Get("HX-Push-Url") != "/dashboard?error_type=NullPointerException&win=24h" || strings.Contains(strings.ToLower(string(body)), "<!doctype") {
		t.Errorf("fragment: push=%q", resp.Header.Get("HX-Push-Url"))
	}
	if !strings.Contains(string(body), `class="chip"`) {
		t.Error("chip row expected for active filter")
	}
	// poll → no push
	resp, _ = e.do(t, "GET", "/dashboard", nil, map[string]string{"HX-Request": "true", "X-Poll": "1"})
	if resp.Header.Get("HX-Push-Url") != "" {
		t.Error("poll must not push url")
	}

	// detail sheet
	_, body = e.api(t, "GET", "/api/events?limit=1", nil)
	id := int64(decode[[]map[string]any](t, body)[0]["id"].(float64))
	resp, body = e.do(t, "GET", fmt.Sprintf("/events/%d/detail", id), nil, nil)
	if resp.StatusCode != 200 || !strings.Contains(string(body), "Raw payload") || !strings.Contains(string(body), "Breadcrumbs") || !strings.Contains(string(body), "build:42") {
		t.Errorf("detail: %d %s", resp.StatusCode, body[:min(len(body), 300)])
	}
	if resp, _ := e.do(t, "GET", "/events/424242/detail", nil, nil); resp.StatusCode != 404 {
		t.Error("missing detail → 404")
	}

	// settings + CSRF guard on toggle
	resp, body = e.do(t, "GET", "/settings", nil, nil)
	if resp.StatusCode != 200 || !strings.Contains(string(body), `role="switch"`) || !strings.Contains(string(body), "not configured") {
		t.Errorf("settings: %d", resp.StatusCode)
	}
	if !strings.Contains(string(body), "no notification channels configured") {
		t.Error("banner should warn about silent alerts")
	}
	form := map[string]string{"Content-Type": "application/x-www-form-urlencoded"}
	if resp, _ := e.do(t, "PATCH", "/settings/alerts/crash_spike", []byte("enabled=false"), form); resp.StatusCode != 403 {
		t.Errorf("non-htmx toggle should be forbidden: %d", resp.StatusCode)
	}
	form["HX-Request"] = "true"
	resp, body = e.do(t, "PATCH", "/settings/alerts/crash_spike", []byte("enabled=false"), form)
	if resp.StatusCode != 200 || !strings.Contains(string(body), `id="settings-panel"`) {
		t.Errorf("htmx toggle: %d", resp.StatusCode)
	}
	al, _ := e.st.ListAlertTypes(context.Background())
	if al[0].Type != "crash_spike" || al[0].Enabled {
		t.Error("toggle did not persist")
	}

	// static assets
	if resp, _ := e.do(t, "GET", "/static/app.js", nil, nil); resp.StatusCode != 200 || !strings.HasPrefix(resp.Header.Get("Content-Type"), "application/javascript") {
		t.Error("static asset")
	}
	if resp, _ := e.do(t, "GET", "/static/../go.mod", nil, nil); resp.StatusCode == 200 {
		t.Error("traversal")
	}
	if resp, _ := e.do(t, "GET", "/nope/dashboard", nil, nil); resp.StatusCode != 404 {
		t.Error("slug routes need DEPLOYMENTS")
	}
}

func TestPortal(t *testing.T) {
	e := newEnvSelf(t)
	e.ingest(t, event(time.Now().UTC(), "fatal", "Crash", "9.9", "u", "d", bp(false), ""))

	resp, body := e.do(t, "GET", "/", nil, nil)
	html := string(body)
	if resp.StatusCode != 200 || !strings.Contains(html, "unreachable") || !strings.Contains(html, `class="portal-name"`) {
		t.Fatalf("portal: %d", resp.StatusCode)
	}
	if !strings.Contains(html, `href="/android/settings"`) {
		t.Error("self settings link")
	}
	resp, _ = e.do(t, "GET", "/dashboard", nil, nil)
	if resp.StatusCode != 200 { // followed redirect to /android/dashboard
		t.Errorf("bare dashboard redirect: %d", resp.StatusCode)
	}
	resp, body = e.do(t, "GET", "/android/dashboard", nil, nil)
	if resp.StatusCode != 200 || !strings.Contains(string(body), `hx-get="/android/events/`) || !strings.Contains(string(body), "brand-project") {
		t.Errorf("slug dashboard: %d", resp.StatusCode)
	}
	if resp, _ := e.do(t, "GET", "/android/settings", nil, nil); resp.StatusCode != 200 {
		t.Error("slug settings")
	}
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	r, _ := http.NewRequest("GET", e.srv.URL+"/ios/dashboard?win=24h", nil)
	rr, _ := client.Do(r)
	if rr.StatusCode != 302 || rr.Header.Get("Location") != "http://127.0.0.1:1/ios/dashboard?win=24h" {
		t.Errorf("foreign slug redirect: %d %s", rr.StatusCode, rr.Header.Get("Location"))
	}
}

// newEnvSelf builds an env whose DEPLOYMENTS lists its own URL as "Android".
func newEnvSelf(t *testing.T) *env {
	t.Helper()
	pool := testdb.Pool(t)
	st := store.New(pool)
	cfg := config.Config{APIKeys: []string{apiKey}, IngestToken: ingestToken, SampleRate: 1, RetentionDays: 30}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	var handler http.Handler
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { handler.ServeHTTP(w, r) }))
	t.Cleanup(srv.Close)
	cfg.Deployments = "iOS|http://127.0.0.1:1|k,Android|" + srv.URL
	handler = New(cfg, st, log)
	return &env{srv: srv, st: st, cfg: cfg}
}

func TestSymbols(t *testing.T) {
	e := newEnv(t, nil)
	mapping := "com.example.Main -> a.b:\n    void run() -> c\n"
	resp, body := e.api(t, "POST", "/api/symbols?platform=android&release=1.0&file=mapping.txt", []byte(mapping))
	if resp.StatusCode != 201 {
		t.Fatalf("upload: %d %s", resp.StatusCode, body)
	}
	_, body = e.api(t, "GET", "/api/symbols", nil)
	if list := decode[[]map[string]any](t, body); len(list) != 1 || list[0]["filename"] != "mapping.txt" {
		t.Errorf("list = %s", body)
	}
	_, body = e.api(t, "POST", "/api/symbolicate", []byte(`{"platform":"android","release":"1.0","frames":[{"filename":"a.b","function":"c","lineno":3}]}`))
	out := decode[map[string]any](t, body)
	if out["symbolicated"] != true || out["frames"].([]any)[0].(map[string]any)["filename"] != "com.example.Main.run" {
		t.Errorf("symbolicate = %s", body)
	}
	_, body = e.api(t, "POST", "/api/symbolicate", []byte(`{"platform":"android","release":"2.0","frames":[]}`))
	if out := decode[map[string]any](t, body); out["symbolicated"] != false {
		t.Error("no mapping → not symbolicated")
	}
	if resp, _ := e.api(t, "POST", "/api/symbols?platform=android&release=1.0&file=../x", []byte("x")); resp.StatusCode != 400 {
		t.Error("bad file name")
	}
}

func TestRetentionAndAlerts(t *testing.T) {
	e := newEnv(t, nil)
	now := time.Now().UTC()
	old := now.Add(-40 * 24 * time.Hour)
	e.ingest(t,
		event(old, "fatal", "OldCrash", "0.1", "u-old", "d-old", bp(false), ""),
		event(now.Add(-time.Minute), "fatal", "NewCrash", "1.0", "u", "d", bp(false), ""),
	)
	ctx := context.Background()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	rep, err := (&retention.Runner{Store: e.st, Days: 30, Log: log, Now: time.Now}).Run(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if rep.Events != 1 || rep.Issues != 1 || rep.UserDevices != 1 || rep.HourlyStats != 1 {
		t.Errorf("retention report = %+v", rep)
	}
	evs, _ := e.st.ListEvents(ctx, store.EventFilter{})
	if len(evs) != 1 || *evs[0].ErrorType != "NewCrash" {
		t.Errorf("remaining = %+v", evs)
	}

	// alerts: new_error fires for the fresh fingerprint; cooldown blocks a rerun
	var sent []string
	chk := &alerts.Checker{Store: e.st, Log: log, Now: time.Now, Notify: notifyFunc(func(m string) { sent = append(sent, m) })}
	if err := chk.Run(ctx); err != nil {
		t.Fatal(err)
	}
	if len(sent) != 1 || !strings.Contains(sent[0], "NewCrash") {
		t.Errorf("sent = %q", sent)
	}
	if err := chk.Run(ctx); err != nil || len(sent) != 1 {
		t.Errorf("cooldown should suppress: %v %q", err, sent)
	}
	al, _ := e.st.ListAlertTypes(ctx)
	for _, a := range al {
		if a.Type == "new_error" && (a.LastTriggered == nil || a.CooldownUntil == nil) {
			t.Error("bookkeeping missing")
		}
	}
}

type notifyFunc func(string)

func (f notifyFunc) Send(_ context.Context, m string) { f(m) }

func TestSamplingAndRedaction(t *testing.T) {
	e := newEnv(t, func(c *config.Config) { c.PIIRedact = true; c.SampleRate = 0 })
	now := time.Now().UTC()
	resp, body := e.ingest(t,
		event(now, "info", "", "1.0", "user-12345678", "d", nil, `,"message":"mail john@example.com"`),
		event(now, "error", "E", "1.0", "user-12345678", "d", bp(true), `,"message":"call john@example.com"`),
	)
	if resp.StatusCode != 200 {
		t.Fatalf("%d %s", resp.StatusCode, body)
	}
	if r := decode[map[string]int](t, body); r["events"] != 1 || r["dropped"] != 1 {
		t.Errorf("sampling result = %v", r)
	}
	evs, _ := e.st.ListEvents(context.Background(), store.EventFilter{})
	if len(evs) != 1 || *evs[0].UserID != "user****5678" || strings.Contains(evs[0].Message, "@") {
		t.Errorf("redaction: %+v", evs)
	}
}

func TestRateLimitHeaders(t *testing.T) {
	e := newEnv(t, func(c *config.Config) { c.RateLimit = 2 })
	for i := 0; i < 3; i++ {
		resp, _ := e.api(t, "GET", "/api/alerts", nil)
		if i < 2 && resp.StatusCode != 200 {
			t.Errorf("req %d: %d", i, resp.StatusCode)
		}
		if i == 2 && resp.StatusCode != 429 {
			t.Errorf("third request should be limited: %d", resp.StatusCode)
		}
		if resp.Header.Get("X-RateLimit-Limit") != "2" {
			t.Error("limit header")
		}
	}
}
