package api

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/at-least/crashcart/internal/config"
	"github.com/at-least/crashcart/internal/db/sqlc"
	"github.com/at-least/crashcart/internal/ingest"
	"github.com/at-least/crashcart/internal/sentry"
	"github.com/at-least/crashcart/internal/store"
	"github.com/at-least/crashcart/internal/symbolicate"
	"github.com/at-least/crashcart/internal/testdb"
)

const apiKey = "test-key"

type env struct {
	t   *testing.T
	st  *store.Store
	mux *http.ServeMux
	in  *ingest.Ingester
}

func newEnv(t *testing.T) *env {
	st := testdb.New(t)
	log := slog.New(slog.NewTextHandler(os.Stderr, nil))
	cfg := config.Config{APIKeys: []string{apiKey}, CORSOrigin: "*", Addr: ":8080"}
	mux := http.NewServeMux()
	(&Handler{Store: st, Cfg: cfg, Log: log, Symbols: &symbolicate.Service{Store: st, DSYM: symbolicate.NewDSYMClient("")}}).Register(mux)
	return &env{t: t, st: st, mux: mux, in: &ingest.Ingester{Store: st, Cfg: cfg, Log: log}}
}

func (e *env) do(method, path string, body any) (*httptest.ResponseRecorder, map[string]any) {
	e.t.Helper()
	var rd io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	}
	req := httptest.NewRequest(method, path, rd)
	req.Header.Set("Authorization", "Bearer "+apiKey)
	req.Host = "crash.example.com"
	rec := httptest.NewRecorder()
	e.mux.ServeHTTP(rec, req)
	var out map[string]any
	if rec.Body.Len() > 0 && strings.HasPrefix(rec.Body.String(), "{") {
		if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
			e.t.Fatalf("%s %s: bad JSON %q", method, path, rec.Body.String())
		}
	}
	return rec, out
}

func (e *env) get(path string, want int) map[string]any {
	e.t.Helper()
	rec, out := e.do("GET", path, nil)
	if rec.Code != want {
		e.t.Fatalf("GET %s = %d %s", path, rec.Code, rec.Body.String())
	}
	return out
}

func (e *env) upload(path, filename string, data []byte, fields map[string]string) (*httptest.ResponseRecorder, []byte) {
	e.t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	for k, v := range fields {
		mw.WriteField(k, v)
	}
	fw, _ := mw.CreateFormFile("file", filename)
	fw.Write(data)
	mw.Close()
	req := httptest.NewRequest("POST", path, &buf)
	req.Header.Set("Authorization", "Bearer "+apiKey)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	rec := httptest.NewRecorder()
	e.mux.ServeHTTP(rec, req)
	return rec, rec.Body.Bytes()
}

func (e *env) createProject(slug string) sqlc.Project {
	e.t.Helper()
	rec, _ := e.do("POST", "/api/projects", map[string]any{"slug": slug, "name": "Demo", "platform": "android"})
	if rec.Code != http.StatusCreated {
		e.t.Fatalf("create project: %d %s", rec.Code, rec.Body.String())
	}
	p, err := e.st.GetProject(context.Background(), slug)
	if err != nil {
		e.t.Fatal(err)
	}
	return p
}

func event(id, ts, level, release, user, typ string, handled bool, lineno int, extra string) string {
	return fmt.Sprintf(`{"event_id":"%s","timestamp":"%s","level":"%s","platform":"android","environment":"production","release":"%s","transaction":"CartFragment","tags":{"device_id":"did-1","build":"42"},"user":{"id":"%s"},"sdk":{"name":"sentry.java.android"},"contexts":{"device":{"model":"Pixel 8"},"os":{"version":"14"}},"exception":{"values":[{"type":"%s","value":"boom","mechanism":{"handled":%t},"stacktrace":{"frames":[{"filename":"Looper.java","function":"loop","in_app":false,"lineno":10},{"filename":"com/example/CartFragment.java","function":"onCreateView","in_app":true,"lineno":%d}]}}]}%s}`,
		id, ts, level, release, user, typ, handled, lineno, extra)
}

// seed ingests: two fatal crashes of one issue (2.4.1, two users), one
// handled error of another issue (2.4.0), one info message, and sessions.
func (e *env) seed(p sqlc.Project) {
	e.t.Helper()
	now := time.Now().UTC()
	ts := func(d time.Duration) string { return now.Add(-d).Format(time.RFC3339) }
	items := []string{
		`{"type":"event"}`, event("e1e1e1e1e1e1e1e1e1e1e1e1e1e1e1e1", ts(time.Hour), "fatal", "2.4.1", "user-001", "NullPointerException", false, 142, ""),
		`{"type":"event"}`, event("e2e2e2e2e2e2e2e2e2e2e2e2e2e2e2e2", ts(30*time.Minute), "fatal", "2.4.1", "user-002", "NullPointerException", false, 999, ""),
		`{"type":"event"}`, event("e3e3e3e3e3e3e3e3e3e3e3e3e3e3e3e3", ts(2*time.Hour), "error", "2.4.0", "user-001", "IOException", true, 7, ""),
		`{"type":"event"}`, fmt.Sprintf(`{"event_id":"e4e4e4e4e4e4e4e4e4e4e4e4e4e4e4e4","timestamp":"%s","level":"info","message":"app started","release":"2.4.1","tags":{"build":"43"}}`, ts(3*time.Hour)),
		`{"type":"sessions"}`, fmt.Sprintf(`{"aggregates":[{"started":"%s","exited":10,"crashed":1}],"attrs":{"release":"2.4.1","environment":"production"}}`, ts(time.Hour)),
		`{"type":"session"}`, fmt.Sprintf(`{"sid":"s1","status":"exited","started":"%s","attrs":{"release":"2.4.0"}}`, ts(4*time.Hour)),
	}
	body := []byte("{\"event_id\":\"h1\"}\n" + strings.Join(items, "\n") + "\n")
	res, err := e.in.Ingest(context.Background(), p, sentry.Parse(body, now), now)
	if err != nil {
		e.t.Fatal(err)
	}
	if res.Stored != 4 || res.Sessions != 3 || len(res.NewIssues) != 2 {
		e.t.Fatalf("seed: %+v", res)
	}
}

func TestProjectsAndAuth(t *testing.T) {
	e := newEnv(t)
	req := httptest.NewRequest("GET", "/api/projects", nil)
	rec := httptest.NewRecorder()
	e.mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("no bearer: %d", rec.Code)
	}
	req = httptest.NewRequest("OPTIONS", "/api/projects/demo/issues", nil)
	req.Header.Set("Origin", "http://x")
	rec = httptest.NewRecorder()
	e.mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusNoContent || rec.Header().Get("Access-Control-Allow-Origin") != "*" {
		t.Fatalf("preflight: %d %v", rec.Code, rec.Header())
	}

	rec, out := e.do("POST", "/api/projects", map[string]any{"slug": "demo", "name": "Demo", "platform": "android"})
	if rec.Code != http.StatusCreated {
		t.Fatalf("create: %d %s", rec.Code, rec.Body.String())
	}
	dsn, _ := out["dsn"].(string)
	if !strings.HasPrefix(dsn, "http://") || !strings.HasSuffix(dsn, "@crash.example.com/"+fmt.Sprint(int64(out["id"].(float64)))) {
		t.Errorf("dsn = %q", dsn)
	}
	rec, _ = e.do("POST", "/api/projects", map[string]any{"slug": "demo", "name": "Dup"})
	if rec.Code != http.StatusConflict {
		t.Errorf("duplicate slug: %d", rec.Code)
	}
	rec, _ = e.do("POST", "/api/projects", map[string]any{"slug": "Bad Slug"})
	if rec.Code != http.StatusBadRequest {
		t.Errorf("bad slug: %d", rec.Code)
	}
	if out := e.get("/api/projects/demo/alerts", 200); len(out["rules"].([]any)) != 3 {
		t.Errorf("default alert rules: %v", out["rules"])
	}
	req = httptest.NewRequest("GET", "/api/projects/demo", nil)
	req.Header.Set("Authorization", "Bearer "+apiKey)
	req.Header.Set("X-Forwarded-Proto", "https")
	req.Host = "public.example.org"
	rec = httptest.NewRecorder()
	e.mux.ServeHTTP(rec, req)
	if !strings.Contains(rec.Body.String(), `"dsn":"https://`) || !strings.Contains(rec.Body.String(), "@public.example.org/") {
		t.Errorf("forwarded dsn: %s", rec.Body.String())
	}
	rec, out = e.do("PATCH", "/api/projects/demo", map[string]any{"name": "Renamed", "sample_rate": 0.5, "sample_keep_first": 10})
	if rec.Code != 200 || out["name"] != "Renamed" || out["sample_rate"] != 0.5 || out["sample_keep_first"] != float64(10) {
		t.Errorf("patch: %d %v", rec.Code, out)
	}
	// Daily quota and key rotation.
	rec, out = e.do("PATCH", "/api/projects/demo", map[string]any{"daily_quota": 5000})
	if rec.Code != 200 || out["daily_quota"] != float64(5000) {
		t.Errorf("daily_quota: %d %v", rec.Code, out)
	}
	if rec, _ = e.do("PATCH", "/api/projects/demo", map[string]any{"daily_quota": -1}); rec.Code != 400 {
		t.Errorf("negative quota accepted: %d", rec.Code)
	}
	before := e.get("/api/projects/demo", 200)["dsn"]
	rec, out = e.do("POST", "/api/projects/demo/rotate-key", nil)
	if rec.Code != 200 || out["dsn"] == before || out["dsn"] == nil {
		t.Errorf("rotate-key: %d %v (before %v)", rec.Code, out, before)
	}
	if rec, _ = e.do("PATCH", "/api/projects/demo", map[string]any{"sample_rate": 2}); rec.Code != 400 {
		t.Errorf("bad sample_rate: %d", rec.Code)
	}
	if rec, _ := e.do("GET", "/api/projects", nil); rec.Code != 200 || !strings.HasPrefix(rec.Body.String(), "[{") {
		t.Errorf("list: %d %s", rec.Code, rec.Body.String())
	}
	e.get("/api/projects/nope", 404)
	e.get("/api/projects/nope/issues", 404)
	if rec, _ := e.do("DELETE", "/api/projects/demo", nil); rec.Code != 204 {
		t.Errorf("delete: %d", rec.Code)
	}
	e.get("/api/projects/demo", 404)
}

func TestOverviewIssuesEventsReleases(t *testing.T) {
	e := newEnv(t)
	p := e.createProject("demo")
	e.seed(p)

	// overview
	ov := e.get("/api/projects/demo/overview?days=1", 200)
	tot := ov["totals"].(map[string]any)
	if tot["events"] != float64(4) || tot["crashes"] != float64(2) || tot["errors"] != float64(1) {
		t.Errorf("totals = %v", tot)
	}
	if ov["new_issues"] != float64(2) || ov["regressions"] != float64(0) {
		t.Errorf("new/regressions = %v %v", ov["new_issues"], ov["regressions"])
	}
	if lv := ov["levels"].(map[string]any); lv["fatal"] != float64(2) || lv["info"] != float64(1) {
		t.Errorf("levels = %v", lv)
	}
	cf, _ := ov["crash_free"].(map[string]any)
	if cf == nil || cf["release"] != "2.4.1" || cf["sessions"] != float64(11) || cf["rate"].(float64) < 0.9 || cf["rate"].(float64) > 0.91 {
		t.Errorf("crash_free = %v", cf)
	}
	tl := ov["timeline"].([]any)
	if len(tl) < 24 {
		t.Errorf("timeline len = %d", len(tl))
	}
	var sum float64
	for _, pt := range tl {
		sum += pt.(map[string]any)["events"].(float64)
	}
	if sum != 4 {
		t.Errorf("timeline events sum = %v", sum)
	}
	e.get("/api/projects/demo/overview?days=100", 400)

	// issues
	is := e.get("/api/projects/demo/issues?sort=events", 200)
	if is["total"] != float64(2) {
		t.Fatalf("issues total = %v", is["total"])
	}
	list := is["issues"].([]any)
	first := list[0].(map[string]any)
	if first["error_type"] != "NullPointerException" || first["event_count"] != float64(2) || first["users"] != float64(2) {
		t.Errorf("first issue = %v", first)
	}
	sp := first["sparkline"].([]any)
	var spSum float64
	for _, v := range sp {
		spSum += v.(float64)
	}
	if len(sp) != sparklineHours || spSum != 2 {
		t.Errorf("sparkline len=%d sum=%v", len(sp), spSum)
	}
	if _, err := time.Parse(time.RFC3339, first["last_seen"].(string)); err != nil {
		t.Errorf("last_seen not RFC3339: %v", first["last_seen"])
	}
	fp := first["fingerprint"].(string)
	if q := e.get("/api/projects/demo/issues?q=ioexc", 200); q["total"] != float64(1) {
		t.Errorf("q filter total = %v", q["total"])
	}
	if q := e.get("/api/projects/demo/issues?release=2.4.0", 200); q["total"] != float64(1) {
		t.Errorf("release filter total = %v", q["total"])
	}
	if q := e.get("/api/projects/demo/issues?level=fatal&limit=1&offset=1", 200); q["total"] != float64(1) || len(q["issues"].([]any)) != 0 {
		t.Errorf("level/offset = %v", q)
	}
	e.get("/api/projects/demo/issues?status=bogus", 400)
	e.get("/api/projects/demo/issues?sort=drop", 400)

	// issue detail
	d := e.get("/api/projects/demo/issues/"+fp, 200)
	if d["users"] != float64(2) || d["latest_event_id"].(float64) <= 0 || d["oldest_event_id"].(float64) >= d["latest_event_id"].(float64) {
		t.Errorf("detail = %v", d)
	}
	bd := d["breakdown"].(map[string]any)
	if rel := bd["release"].([]any); len(rel) != 1 || rel[0].(map[string]any)["value"] != "2.4.1" || rel[0].(map[string]any)["count"] != float64(2) {
		t.Errorf("breakdown.release = %v", bd["release"])
	}
	if len(d["timeline"].([]any)) < 7*24 {
		t.Errorf("issue timeline len = %d", len(d["timeline"].([]any)))
	}
	e.get("/api/projects/demo/issues/nope", 404)

	// status changes
	rec, out := e.do("PATCH", "/api/projects/demo/issues/"+fp, map[string]any{"status": "resolved"})
	if rec.Code != 200 || out["status"] != "resolved" || out["resolved_release"] != "2.4.1" {
		t.Errorf("patch status: %d %v", rec.Code, out)
	}
	if rec, _ := e.do("PATCH", "/api/projects/demo/issues/"+fp, map[string]any{"status": "wat"}); rec.Code != 400 {
		t.Errorf("bad status: %d", rec.Code)
	}
	if rec, _ := e.do("PATCH", "/api/projects/demo/issues/nope", map[string]any{"status": "ignored"}); rec.Code != 404 {
		t.Errorf("patch unknown: %d", rec.Code)
	}
	second := list[1].(map[string]any)["fingerprint"].(string)
	rec, out = e.do("POST", "/api/projects/demo/issues/bulk", map[string]any{"fingerprints": []string{fp, second}, "status": "ignored"})
	if rec.Code != 200 || out["updated"] != float64(2) {
		t.Errorf("bulk: %d %v", rec.Code, out)
	}
	if q := e.get("/api/projects/demo/issues?status=ignored", 200); q["total"] != float64(2) {
		t.Errorf("ignored total = %v", q["total"])
	}

	// events
	ev := e.get("/api/projects/demo/events", 200)
	if len(ev["events"].([]any)) != 4 || ev["more"] != false || ev["next_before"] != nil {
		t.Errorf("events = %v", ev)
	}
	if q := e.get("/api/projects/demo/events?crash=1", 200); len(q["events"].([]any)) != 2 {
		t.Errorf("crash filter = %v", q)
	}
	if q := e.get("/api/projects/demo/events?tag.build=43", 200); len(q["events"].([]any)) != 1 {
		t.Errorf("tag filter = %v", q)
	}
	if q := e.get("/api/projects/demo/events?fingerprint="+fp+"&user_id=user-002", 200); len(q["events"].([]any)) != 1 {
		t.Errorf("fingerprint+user filter = %v", q)
	}
	if q := e.get("/api/projects/demo/events?q=started", 200); len(q["events"].([]any)) != 1 {
		t.Errorf("q filter = %v", q)
	}
	page := e.get("/api/projects/demo/events?limit=3", 200)
	if page["more"] != true || page["next_before"] == nil {
		t.Fatalf("page 1 = %v", page)
	}
	next := e.get(fmt.Sprintf("/api/projects/demo/events?limit=3&before=%d", int64(page["next_before"].(float64))), 200)
	if len(next["events"].([]any)) != 1 || next["more"] != false {
		t.Errorf("page 2 = %v", next)
	}
	if q := e.get("/api/projects/demo/events?days=1&level=info", 200); len(q["events"].([]any)) != 1 {
		t.Errorf("window+level = %v", q)
	}
	e.get("/api/projects/demo/events?before=x", 400)
	item := page["events"].([]any)[0].(map[string]any)
	if _, ok := item["payload"]; ok {
		t.Error("list must not include payload")
	}
	id := int64(item["id"].(float64))
	det := e.get(fmt.Sprintf("/api/projects/demo/events/%d", id), 200)
	payload, ok := det["payload"].(map[string]any)
	if !ok || payload["event_id"] != det["event_id"] {
		t.Errorf("payload not an object: %v", det["payload"])
	}
	if _, ok := det["breadcrumbs"].([]any); !ok {
		t.Errorf("breadcrumbs = %v", det["breadcrumbs"])
	}
	if _, err := time.Parse(time.RFC3339, det["time"].(string)); err != nil {
		t.Errorf("time = %v", det["time"])
	}
	byEID := e.get("/api/projects/demo/events/e1e1e1e1e1e1e1e1e1e1e1e1e1e1e1e1", 200)
	if byEID["event_id"] != "e1e1e1e1e1e1e1e1e1e1e1e1e1e1e1e1" || byEID["level"] != "fatal" {
		t.Errorf("by event_id = %v", byEID)
	}
	e.get("/api/projects/demo/events/123", 404)
	e.get("/api/projects/demo/events/nope", 404)

	// releases
	rl := e.get("/api/projects/demo/releases", 200)
	rels := rl["releases"].([]any)
	if len(rels) != 2 {
		t.Fatalf("releases = %v", rels)
	}
	byName := map[string]map[string]any{}
	for _, r := range rels {
		m := r.(map[string]any)
		byName[m["release"].(string)] = m
	}
	r241 := byName["2.4.1"]
	if r241["events"] != float64(3) || r241["crashes"] != float64(2) || r241["new_issues"] != float64(1) {
		t.Errorf("2.4.1 = %v", r241)
	}
	if s := r241["sessions"].(map[string]any); s["total"] != float64(11) || s["crashed"] != float64(1) {
		t.Errorf("2.4.1 sessions = %v", s)
	}
	if rate, _ := r241["crash_free_rate"].(float64); rate < 0.9 || rate > 0.91 {
		t.Errorf("2.4.1 crash_free_rate = %v", r241["crash_free_rate"])
	}
	if byName["2.4.0"]["platforms"].([]any)[0] != "android" {
		t.Errorf("2.4.0 platforms = %v", byName["2.4.0"]["platforms"])
	}
	rd := e.get("/api/projects/demo/releases/2.4.1", 200)
	if len(rd["issues_introduced"].([]any)) != 1 || len(rd["issues_present"].([]any)) != 1 || len(rd["daily_health"].([]any)) != 1 {
		t.Errorf("release detail = %v", rd)
	}
	if rel := rd["release"].(map[string]any); rel["events"] != float64(3) || rel["new_issues"] != float64(1) {
		t.Errorf("release detail stats = %v", rel)
	}
	var tsum float64
	for _, pt := range rd["timeline"].([]any) {
		tsum += pt.(map[string]any)["crashes"].(float64)
	}
	if tsum != 2 {
		t.Errorf("release timeline crashes = %v", tsum)
	}
	e.get("/api/projects/demo/releases/9.9.9", 404)
}

func TestAlerts(t *testing.T) {
	e := newEnv(t)
	e.createProject("demo")
	rec, out := e.do("PATCH", "/api/projects/demo/alerts/new_issue", map[string]any{"enabled": false, "cooldown_minutes": 5})
	if rec.Code != 200 || out["enabled"] != false || out["cooldown_minutes"] != float64(5) {
		t.Errorf("patch rule: %d %v", rec.Code, out)
	}
	if rec, _ := e.do("PATCH", "/api/projects/demo/alerts/bogus", map[string]any{"enabled": true}); rec.Code != 404 {
		t.Errorf("bogus type: %d", rec.Code)
	}
	if rec, _ := e.do("PATCH", "/api/projects/demo/alerts/regression", map[string]any{"cooldown_minutes": -1}); rec.Code != 400 {
		t.Errorf("negative cooldown: %d", rec.Code)
	}
	rec, out = e.do("POST", "/api/projects/demo/alerts/channels", map[string]any{"kind": "webhook", "config": map[string]any{"url": "https://hooks.example.com/x"}})
	if rec.Code != 201 || out["kind"] != "webhook" {
		t.Fatalf("create channel: %d %v", rec.Code, out)
	}
	id := int64(out["id"].(float64))
	if rec, _ := e.do("POST", "/api/projects/demo/alerts/channels", map[string]any{"kind": "telegram", "config": map[string]any{}}); rec.Code != 400 {
		t.Errorf("telegram without chat_id: %d", rec.Code)
	}
	if rec, _ := e.do("POST", "/api/projects/demo/alerts/channels", map[string]any{"kind": "sms", "config": map[string]any{}}); rec.Code != 400 {
		t.Errorf("bad kind: %d", rec.Code)
	}
	if rec, _ := e.do("POST", "/api/projects/demo/alerts/channels", map[string]any{"kind": "telegram", "config": map[string]any{"chat_id": 12345}}); rec.Code != 201 {
		t.Errorf("telegram numeric chat_id: %d", rec.Code)
	}
	al := e.get("/api/projects/demo/alerts", 200)
	if len(al["channels"].([]any)) != 2 || len(al["rules"].([]any)) != 3 {
		t.Errorf("alerts = %v", al)
	}
	if rec, _ := e.do("DELETE", fmt.Sprintf("/api/projects/demo/alerts/channels/%d", id), nil); rec.Code != 204 {
		t.Errorf("delete channel: %d", rec.Code)
	}
	if rec, _ := e.do("DELETE", fmt.Sprintf("/api/projects/demo/alerts/channels/%d", id), nil); rec.Code != 404 {
		t.Errorf("delete channel again: %d", rec.Code)
	}
}

func zipOf(t *testing.T, files map[string]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for name, content := range files {
		w, err := zw.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		w.Write([]byte(content))
	}
	zw.Close()
	return buf.Bytes()
}

func TestSymbols(t *testing.T) {
	e := newEnv(t)
	p := e.createProject("demo")
	ctx := context.Background()
	jobsBefore, _ := e.st.CountJobs(ctx)

	mapping := "# compiler: R8\ncom.example.Foo -> a.b:\n    void bar() -> c\n"
	rec, body := e.upload("/api/projects/demo/symbols", "mapping.txt", []byte(mapping), map[string]string{"release": "2.4.1"})
	if rec.Code != 201 || !strings.Contains(string(body), `"kind":"proguard"`) {
		t.Fatalf("upload: %d %s", rec.Code, body)
	}
	if n, _ := e.st.CountJobs(ctx); n != jobsBefore+1 {
		t.Errorf("resymbolicate job not enqueued: %d -> %d", jobsBefore, n)
	}
	rec, body = e.upload("/api/projects/demo/symbols", "app.js.map", []byte(`{"version":3,"mappings":"AAAA"}`), map[string]string{"release": "web-1"})
	if rec.Code != 201 || !strings.Contains(string(body), `"kind":"sourcemap"`) {
		t.Errorf("sourcemap upload: %d %s", rec.Code, body)
	}
	rec, body = e.upload("/api/projects/demo/symbols", "x.bin", []byte("data"), map[string]string{"release": "1", "kind": "nope"})
	if rec.Code != 400 {
		t.Errorf("bad kind: %d %s", rec.Code, body)
	}
	if rec, body := e.upload("/api/projects/demo/symbols", "x.bin", nil, nil); rec.Code != 400 {
		t.Errorf("empty file: %d %s", rec.Code, body)
	}
	z := zipOf(t, map[string]string{"App.dSYM/Contents/Resources/DWARF/App": minimalMachO, "__MACOSX/._x": "junk", "mapping.txt": mapping})
	rec, body = e.upload("/api/projects/demo/symbols", "bundle.zip", z, map[string]string{"release": "3.0.0"})
	if rec.Code != 201 {
		t.Fatalf("zip upload: %d %s", rec.Code, body)
	}
	var zr struct{ Symbols []sqlc.UpsertSymbolFileRow }
	json.Unmarshal(body, &zr)
	kinds := map[string]string{}
	for _, s := range zr.Symbols {
		kinds[s.Filename] = s.Kind
	}
	if len(zr.Symbols) != 2 || kinds["App.dSYM/Contents/Resources/DWARF/App"] != "dsym" || kinds["mapping.txt"] != "proguard" {
		t.Errorf("zip entries = %v", kinds)
	}
	ls := e.get("/api/projects/demo/symbols", 200)
	syms := ls["symbols"].([]any)
	if len(syms) != 4 {
		t.Fatalf("list = %d", len(syms))
	}
	id := int64(syms[0].(map[string]any)["id"].(float64))
	if rec, _ := e.do("DELETE", fmt.Sprintf("/api/projects/demo/symbols/%d", id), nil); rec.Code != 204 {
		t.Errorf("delete: %d", rec.Code)
	}
	if rec, _ := e.do("DELETE", fmt.Sprintf("/api/projects/demo/symbols/%d", id), nil); rec.Code != 404 {
		t.Errorf("delete again: %d", rec.Code)
	}

	// sentry-cli compatibility
	e.get("/api/0/organizations/any/chunk-upload/", 200) // chunked protocol: see chunks_test.go
	uuid := "8f1a2b3c-4d5e-6f70-8192-a3b4c5d6e7f8"
	z = zipOf(t, map[string]string{"proguard/" + uuid + ".txt": mapping})
	rec, body = e.upload(fmt.Sprintf("/api/0/projects/org/%d/files/dsyms/", p.ID), "upload.zip", z, nil)
	if rec.Code != 201 || !strings.HasPrefix(string(body), "[{") {
		t.Fatalf("sentry-cli upload: %d %s", rec.Code, body)
	}
	var files []map[string]any
	json.Unmarshal(body, &files)
	if len(files) != 1 || files[0]["debugId"] != uuid || files[0]["symbolType"] != "proguard" || files[0]["id"] == "" {
		t.Errorf("sentry-cli response = %v", files)
	}
	rec, body = e.upload("/api/0/projects/org/demo/files/dsyms/", "mapping.txt", []byte(mapping), map[string]string{"release": "4.0"})
	if rec.Code != 201 {
		t.Errorf("sentry-cli by slug: %d %s", rec.Code, body)
	}
	if rec, body := e.upload("/api/0/projects/org/unknown/files/dsyms/", "mapping.txt", []byte(mapping), nil); rec.Code != 404 {
		t.Errorf("sentry-cli unknown project: %d %s", rec.Code, body)
	}
	req := httptest.NewRequest("GET", "/api/0/projects/org/demo/files/dsyms/?debug_id="+uuid, nil)
	req.Header.Set("Authorization", "Bearer "+apiKey)
	rr := httptest.NewRecorder()
	e.mux.ServeHTTP(rr, req)
	files = nil
	json.Unmarshal(rr.Body.Bytes(), &files)
	if rr.Code != 200 || len(files) != 1 || files[0]["uuid"] != uuid {
		t.Errorf("sentry-cli list by debug_id: %d %s", rr.Code, rr.Body.String())
	}
	req = httptest.NewRequest("GET", "/api/0/projects/org/demo/files/dsyms/?debug_id=00000000-0000-0000-0000-000000000000", nil)
	req.Header.Set("Authorization", "Bearer "+apiKey)
	rr = httptest.NewRecorder()
	e.mux.ServeHTTP(rr, req)
	if rr.Code != 200 || strings.TrimSpace(rr.Body.String()) != "[]" {
		t.Errorf("sentry-cli list miss: %d %s", rr.Code, rr.Body.String())
	}
}

// minimalMachO is a valid 64-bit little-endian Mach-O header with no load
// commands: enough for debug/macho to parse, no LC_UUID.
const minimalMachO = "\xcf\xfa\xed\xfe\x0c\x00\x00\x01\x00\x00\x00\x00\x02\x00\x00\x00" +
	"\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00"
