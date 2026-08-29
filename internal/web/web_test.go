package web

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/crashcartapp/crashcart/internal/auth"
	"github.com/crashcartapp/crashcart/internal/config"
	"github.com/crashcartapp/crashcart/internal/db/sqlc"
	"github.com/crashcartapp/crashcart/internal/ingest"
	"github.com/crashcartapp/crashcart/internal/sentry"
	"github.com/crashcartapp/crashcart/internal/symbolicate"
	"github.com/crashcartapp/crashcart/internal/testdb"
)

const crashEvent = `{"event_id":"a1b2c3d4e5f6a1b2c3d4e5f6a1b2c3d4","timestamp":"%s","level":"fatal","platform":"android","environment":"production","transaction":"CartFragment","tags":{"device_id":"did-1","build":"42"},"user":{"id":"user-001","email":"u@example.com"},"sdk":{"name":"sentry.java.android"},"contexts":{"device":{"model":"Pixel 8","arch":"arm64"},"os":{"name":"Android","version":"14"},"app":{"app_version":"2.4.1"}},"exception":{"values":[{"type":"NullPointerException","value":"Attempt to invoke virtual method","mechanism":{"type":"UncaughtExceptionHandler","handled":false},"stacktrace":{"frames":[{"filename":"Looper.java","function":"loop","in_app":false,"lineno":10},{"filename":"com/example/CartFragment.java","function":"onCreateView","in_app":true,"lineno":142},{"instruction_addr":"0xdeadbeef","in_app":false}]}}]},"breadcrumbs":{"values":[{"timestamp":"2026-08-29T10:15:00Z","category":"navigation","message":"cart","level":"info"},{"timestamp":"2026-08-29T10:15:29Z","category":"http","message":"GET /api/cart 500","level":"error"}]}}`

const sessionItem = `{"started":"%s","status":"crashed","attrs":{"release":"2.4.1","environment":"production"}}`

// sessionCookie signs the test requests in (set by setup).
var sessionCookie *http.Cookie

func setup(t *testing.T) (*Web, sqlc.Project, *http.ServeMux) {
	t.Helper()
	st := testdb.New(t)
	ctx := context.Background()
	plat := "android"
	p, err := st.CreateProject(ctx, sqlc.CreateProjectParams{Slug: "shop", Name: "Shop App", Platform: &plat, PublicKey: "0123456789abcdef0123456789abcdef"})
	if err != nil {
		t.Fatal(err)
	}
	n := time.Now().UTC()
	ts := n.Add(-10 * time.Minute).Format(time.RFC3339)
	body := "{\"event_id\":\"h1\"}\n{\"type\":\"event\"}\n" + strings.ReplaceAll(crashEvent, "%s", ts) + "\n{\"type\":\"session\"}\n" + strings.ReplaceAll(sessionItem, "%s", ts) + "\n"
	in := &ingest.Ingester{Store: st, Cfg: config.Config{}, Log: slog.Default()}
	res, err := in.Ingest(ctx, p, sentry.Parse([]byte(body), n), n)
	if err != nil || res.Stored != 1 || res.Sessions != 1 {
		t.Fatalf("ingest: %+v %v", res, err)
	}
	w := &Web{Store: st, Cfg: config.Config{CustomTags: []string{"build"}}, Log: slog.Default(), Symbols: &symbolicate.Service{Store: st, DSYM: symbolicate.NewDSYMClient("")}}
	mux := http.NewServeMux()
	w.Register(mux)
	hash, _ := auth.HashPassword("correct horse battery")
	user, err := st.CreateUser(ctx, sqlc.CreateUserParams{Email: "dev@example.com", PasswordHash: hash})
	if err != nil {
		t.Fatal(err)
	}
	if sessionCookie, err = w.access.Login(ctx, httptest.NewRequest("GET", "/", nil), user.ID); err != nil {
		t.Fatal(err)
	}
	return w, p, mux
}

func get(t *testing.T, mux *http.ServeMux, path string, hx bool) (int, string) {
	t.Helper()
	req := httptest.NewRequest("GET", path, nil)
	req.AddCookie(sessionCookie)
	if hx {
		req.Header.Set("HX-Request", "true")
	}
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	return rec.Code, rec.Body.String()
}

func assertPage(t *testing.T, mux *http.ServeMux, path string, wants ...string) string {
	t.Helper()
	code, body := get(t, mux, path, false)
	if code != 200 {
		t.Fatalf("GET %s = %d: %s", path, code, body)
	}
	for _, s := range wants {
		if !strings.Contains(body, s) {
			t.Errorf("GET %s: missing %q", path, s)
		}
	}
	return body
}

func TestPages(t *testing.T) {
	w, p, mux := setup(t)
	ctx := context.Background()
	is, _, err := w.Store.ListIssues(ctx, storeIssueFilter(p.ID))
	if err != nil || len(is) != 1 {
		t.Fatalf("issues: %v %d", err, len(is))
	}
	fp := string(is[0].Fingerprint)

	assertPage(t, mux, "/", "Shop App", "Create project", "/p/shop", "Crashes 24h")
	assertPage(t, mux, "/p/shop", "Overview", "Crash-free sessions", "NullPointerException in CartFragment", "Crashes by release", "data-stream=\"/p/shop/stream?since=")
	body := assertPage(t, mux, "/p/shop/issues", "Unresolved", "NullPointerException in CartFragment", "/p/shop/issues/"+fp, "<svg class=\"spark\"", "id=\"bulk-form\"")
	if !strings.Contains(body, `name="fp" value="`+fp+`"`) {
		t.Error("issue row must carry a checkbox")
	}
	assertPage(t, mux, "/p/shop/issues?status=resolved", "No issues")
	assertPage(t, mux, "/p/shop/issues?q=NullPointer&win=24h", "/p/shop/issues/"+fp)
	assertPage(t, mux, "/p/shop/issues/"+fp, "NullPointerException", "onCreateView", "CartFragment.java:142", "system frames", "0xdeadbeef", "Upload symbols", "Pixel 8", "unhandled", "hx-patch=\"/p/shop/issues/"+fp+"/status\"", "Events over 7d", "OS version")
	evBody := assertPage(t, mux, "/p/shop/events", "Attempt to invoke virtual method", "CartFragment.java:142", "user-001", "tag build", "/p/shop/events/")
	i := strings.Index(evBody, "/p/shop/events/")
	id := evBody[i+len("/p/shop/events/"):]
	id = id[:strings.IndexAny(id, "\"?")]
	assertPage(t, mux, "/p/shop/events?level=fatal&device_model=Pixel+8&tag.build=42&crash=1", "/p/shop/events/"+id, "chip-key")
	assertPage(t, mux, "/p/shop/events?level=info", "No events")
	assertPage(t, mux, "/p/shop/events/"+id, "Breadcrumbs", "GET /api/cart 500", "Contexts", "arm64", "build:42", "u@example.com", "Raw event JSON", "NullPointerException in CartFragment", "onCreateView")
	if code, frag := get(t, mux, "/p/shop/events/"+id, true); code != 200 || strings.Contains(frag, "<html") || !strings.Contains(frag, "id=\"event-body\"") {
		t.Errorf("HX fragment = %d %.80s", code, frag)
	}
	assertPage(t, mux, "/p/shop/releases", "2.4.1", "Crash-free", "Adoption", "/p/shop/releases/2.4.1")
	assertPage(t, mux, "/p/shop/releases/2.4.1", "Crash-free sessions per day", "Issues introduced in this release", "NullPointerException in CartFragment", "1 crashed session")
	assertPage(t, mux, "/p/shop/settings", p.PublicKey, "/api/"+itoa(int(p.ID))+"/envelope/", "Crash spike", "Symbol files", "Sampling", "Add channel")
	if code, _ := get(t, mux, "/p/nope", false); code != 404 {
		t.Errorf("unknown project = %d", code)
	}
	if code, _ := get(t, mux, "/p/shop/issues/deadbeef", false); code != 404 {
		t.Errorf("unknown issue = %d", code)
	}
	if code, _ := get(t, mux, "/static/app.js", false); code != 200 {
		t.Errorf("static = %d", code)
	}
}

func TestBulkAndMutations(t *testing.T) {
	w, p, mux := setup(t)
	ctx := context.Background()
	is, _, _ := w.Store.ListIssues(ctx, storeIssueFilter(p.ID))
	fp := string(is[0].Fingerprint)
	form := url.Values{"fp": {fp}, "status": {"resolved"}}

	// without HX-Request: 403, nothing changes
	req := httptest.NewRequest("POST", "/p/shop/issues/bulk", strings.NewReader(form.Encode()))
	req.AddCookie(sessionCookie)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != 403 {
		t.Fatalf("bulk without HX-Request = %d", rec.Code)
	}
	if got, _ := w.Store.GetIssue(ctx, sqlc.GetIssueParams{ProjectID: p.ID, Fingerprint: sentry.ID(fp)}); got.Status != "unresolved" {
		t.Errorf("status changed without htmx: %s", got.Status)
	}

	// with HX-Request: table fragment for the (now empty) unresolved tab
	req = httptest.NewRequest("POST", "/p/shop/issues/bulk", strings.NewReader(form.Encode()))
	req.AddCookie(sessionCookie)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("HX-Request", "true")
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != 200 || !strings.HasPrefix(strings.TrimSpace(rec.Body.String()), "<div id=\"issues-table\"") || !strings.Contains(rec.Body.String(), "No issues") {
		t.Fatalf("bulk = %d %.120s", rec.Code, rec.Body.String())
	}
	if got, _ := w.Store.GetIssue(ctx, sqlc.GetIssueParams{ProjectID: p.ID, Fingerprint: sentry.ID(fp)}); got.Status != "resolved" || got.ResolvedRelease == nil {
		t.Errorf("bulk resolve: %+v", got)
	}
	assertPage(t, mux, "/p/shop/issues?status=resolved", "/p/shop/issues/"+fp)

	// status select on the issue page → redirect
	req = httptest.NewRequest("PATCH", "/p/shop/issues/"+fp+"/status", strings.NewReader("status=triaged"))
	req.AddCookie(sessionCookie)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("HX-Request", "true")
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != 303 || rec.Header().Get("Location") != "/p/shop/issues/"+fp {
		t.Errorf("status = %d %s", rec.Code, rec.Header().Get("Location"))
	}

	// settings mutations
	hx := func(method, path, body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		req.AddCookie(sessionCookie)
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.Header.Set("HX-Request", "true")
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)
		return rec
	}
	if r := hx("PATCH", "/p/shop/settings/sampling", "keep_first=10&rate=0.5"); r.Code != 303 {
		t.Errorf("sampling = %d %s", r.Code, r.Body)
	}
	if got, _ := w.Store.GetProject(ctx, "shop"); got.SampleKeepFirst != 10 || got.SampleRate != 0.5 {
		t.Errorf("sampling not saved: %+v", got)
	}
	if r := hx("PATCH", "/p/shop/settings/sampling", "keep_first=10&rate=0.5&daily_quota=2000"); r.Code != 303 {
		t.Errorf("quota = %d %s", r.Code, r.Body)
	}
	if got, _ := w.Store.GetProject(ctx, "shop"); got.DailyQuota != 2000 {
		t.Errorf("quota not saved: %d", got.DailyQuota)
	}
	oldKey := ""
	if got, _ := w.Store.GetProject(ctx, "shop"); true {
		oldKey = got.PublicKey
	}
	if r := hx("POST", "/p/shop/settings/rotate-key", ""); r.Code != 303 {
		t.Errorf("rotate = %d %s", r.Code, r.Body)
	}
	if got, _ := w.Store.GetProject(ctx, "shop"); got.PublicKey == oldKey {
		t.Errorf("key not rotated")
	}
	if r := hx("PATCH", "/p/shop/settings/platform", "platform=android"); r.Code != 303 {
		t.Errorf("platform = %d %s", r.Code, r.Body)
	}
	if got, _ := w.Store.GetProject(ctx, "shop"); got.Platform == nil || *got.Platform != "android" {
		t.Errorf("platform not saved: %+v", got.Platform)
	}
	if r := hx("PATCH", "/p/shop/settings/platform", "platform=windows"); r.Code != 400 {
		t.Errorf("bad platform accepted: %d", r.Code)
	}
	if r := hx("PATCH", "/p/shop/settings/alerts/crash_spike", "cooldown=30"); r.Code != 303 {
		t.Errorf("alert = %d", r.Code)
	}
	if ru, err := w.Store.GetAlertRule(ctx, sqlc.GetAlertRuleParams{ProjectID: p.ID, Type: "crash_spike"}); err != nil || ru.Enabled || ru.CooldownMinutes != 30 {
		t.Errorf("alert rule = %+v %v", ru, err)
	}
	if r := hx("POST", "/p/shop/settings/channels", "kind=webhook&url=https://hooks.example.com/x"); r.Code != 303 {
		t.Errorf("channel = %d %s", r.Code, r.Body)
	}
	if r := hx("POST", "/p/shop/settings/channels", "kind=webhook&url=ftp://nope"); r.Code != 400 {
		t.Errorf("bad channel = %d", r.Code)
	}
	assertPage(t, mux, "/p/shop/settings", "hooks.example.com/x")
	chans, _ := w.Store.ListAlertChannels(ctx, p.ID)
	if r := hx("DELETE", "/p/shop/settings/channels/"+itoa(int(chans[0].ID)), ""); r.Code != 303 {
		t.Errorf("delete channel = %d", r.Code)
	}

	// symbol upload (multipart) → file stored + resymbolicate job
	var mp strings.Builder
	mp.WriteString("--b\r\nContent-Disposition: form-data; name=\"release\"\r\n\r\n2.4.1\r\n")
	mp.WriteString("--b\r\nContent-Disposition: form-data; name=\"file\"; filename=\"mapping.txt\"\r\nContent-Type: text/plain\r\n\r\ncom.a.B -> a.b:\r\n--b--\r\n")
	req = httptest.NewRequest("POST", "/p/shop/settings/symbols", strings.NewReader(mp.String()))
	req.AddCookie(sessionCookie)
	req.Header.Set("Content-Type", "multipart/form-data; boundary=b")
	req.Header.Set("HX-Request", "true")
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != 303 {
		t.Fatalf("upload = %d %s", rec.Code, rec.Body)
	}
	files, _ := w.Store.ListSymbolFiles(ctx, p.ID)
	if len(files) != 1 || files[0].Kind != "proguard" || derefStr(files[0].Release) != "2.4.1" || files[0].Filename != "mapping.txt" {
		t.Errorf("symbol files = %+v", files)
	}
	var kinds []string
	rows, _ := w.Store.Pool.Query(ctx, "SELECT kind FROM jobs WHERE project_id = $1 AND kind = 'resymbolicate'", p.ID)
	for rows.Next() {
		var k string
		_ = rows.Scan(&k)
		kinds = append(kinds, k)
	}
	rows.Close()
	if len(kinds) != 1 {
		t.Errorf("resymbolicate jobs = %v", kinds)
	}
	assertPage(t, mux, "/p/shop/settings", "mapping.txt")
	if r := hx("DELETE", "/p/shop/settings/symbols/"+itoa(int(files[0].ID)), ""); r.Code != 303 {
		t.Errorf("delete symbol = %d", r.Code)
	}

	// create project → HX-Redirect to the settings page showing the DSN
	if r := hx("POST", "/projects", "slug=ios-app&name=iOS+App&platform=ios"); r.Code != 200 || r.Header().Get("HX-Redirect") != "/p/ios-app/settings" {
		t.Errorf("create = %d %s", r.Code, r.Header().Get("HX-Redirect"))
	}
	np, err := w.Store.GetProject(ctx, "ios-app")
	if err != nil || len(np.PublicKey) != 32 {
		t.Fatalf("new project: %+v %v", np, err)
	}
	if rules, _ := w.Store.ListAlertRules(ctx, np.ID); len(rules) != 3 {
		t.Errorf("default alert rules = %d", len(rules))
	}
	assertPage(t, mux, "/p/ios-app/settings", np.PublicKey+"@")
	if r := hx("POST", "/projects", "slug=ios-app&name=dup"); r.Code != 409 {
		t.Errorf("dup slug = %d", r.Code)
	}
	if r := hx("POST", "/projects", "slug=Bad Slug&name=x"); r.Code != 400 {
		t.Errorf("bad slug = %d", r.Code)
	}
}

func TestStream(t *testing.T) {
	w, p, mux := setup(t)
	streamPoll, streamKeepAlive = 20*time.Millisecond, 30*time.Millisecond
	defer func() { streamPoll, streamKeepAlive = 5*time.Second, 15*time.Second }()
	ctx, cancel := context.WithTimeout(context.Background(), 250*time.Millisecond)
	defer cancel()
	req := httptest.NewRequest("GET", "/p/shop/stream?since=2000-01-01T00%3A00%3A00Z", nil).WithContext(ctx)
	req.AddCookie(sessionCookie)
	rec := httptest.NewRecorder()
	done := make(chan struct{})
	go func() { mux.ServeHTTP(rec, req); close(done) }()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("stream did not exit on context cancel")
	}
	body := rec.Body.String()
	if rec.Header().Get("Content-Type") != "text/event-stream" || !strings.Contains(body, "event: issues\ndata: {\"new\":1,\"regressions\":0}\n\n") || !strings.Contains(body, ": ping") {
		t.Errorf("stream = %q", body)
	}
	if strings.Count(body, "event: issues") != 1 {
		t.Errorf("unchanged counts must not re-emit: %q", body)
	}
	_ = w
	_ = p
	_ = io.EOF
}

// TestAuthFlow: setup of the first account, sign in / out, API keys.
func TestAuthFlow(t *testing.T) {
	st := testdb.New(t)
	w := &Web{Store: st, Cfg: config.Config{}, Log: slog.Default(), Symbols: &symbolicate.Service{Store: st, DSYM: symbolicate.NewDSYMClient("")}}
	mux := http.NewServeMux()
	w.Register(mux)
	do := func(method, path, body string, cookie *http.Cookie, hx bool) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		if body != "" {
			req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		}
		if cookie != nil {
			req.AddCookie(cookie)
		}
		if hx {
			req.Header.Set("HX-Request", "true")
		}
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)
		return rec
	}
	// No users yet: everything leads to /setup.
	if rec := do("GET", "/", "", nil, false); rec.Code != 303 || rec.Header().Get("Location") != "/setup" {
		t.Fatalf("fresh install: %d %s", rec.Code, rec.Header().Get("Location"))
	}
	if rec := do("GET", "/setup", "", nil, false); rec.Code != 200 || !strings.Contains(rec.Body.String(), "Create the first account") {
		t.Fatalf("setup page: %d", rec.Code)
	}
	if rec := do("POST", "/setup", "email=me%40example.com&password=short", nil, false); rec.Code != 400 {
		t.Errorf("short password accepted: %d", rec.Code)
	}
	rec := do("POST", "/setup", "email=Me%40Example.com&name=Me&password=correct+horse+battery", nil, false)
	if rec.Code != 303 || rec.Header().Get("Location") != "/" || len(rec.Result().Cookies()) == 0 {
		t.Fatalf("setup: %d %v", rec.Code, rec.Header())
	}
	cookie := rec.Result().Cookies()[0]
	if cookie.Name != auth.SessionCookie || !cookie.HttpOnly {
		t.Errorf("cookie = %+v", cookie)
	}
	// Setup is one-time; signed in, the home page renders with the account link.
	if rec := do("GET", "/setup", "", nil, false); rec.Code != 303 || rec.Header().Get("Location") != "/login" {
		t.Errorf("setup after first user: %d", rec.Code)
	}
	if rec := do("GET", "/", "", cookie, false); rec.Code != 200 || !strings.Contains(rec.Body.String(), `href="/account"`) {
		t.Fatalf("home signed in: %d", rec.Code)
	}
	// An htmx request without a session gets the redirect header, not HTML.
	if rec := do("GET", "/", "", nil, true); rec.Code != 401 || rec.Header().Get("HX-Redirect") != "/login" {
		t.Errorf("hx signed out: %d %v", rec.Code, rec.Header())
	}
	// API keys: created on the account page, the secret shown once; revoked keys are gone.
	rec = do("POST", "/account/keys", "name=ci", cookie, true)
	body := rec.Body.String()
	i := strings.Index(body, auth.KeyPrefix)
	if rec.Code != 200 || i < 0 {
		t.Fatalf("create key: %d %.300s", rec.Code, body)
	}
	secret := body[i : i+len(auth.KeyPrefix)+64]
	if k, err := st.GetAPIKeyByHash(context.Background(), auth.HashToken(secret)); err != nil || k.Name != "ci" {
		t.Fatalf("key lookup: %v %+v", err, k)
	}
	if rec := do("GET", "/account", "", cookie, false); strings.Contains(rec.Body.String(), secret) {
		t.Error("secret shown again")
	}
	keys, _ := st.ListAPIKeys(context.Background())
	if rec := do("DELETE", "/account/keys/"+strconv.FormatInt(keys[0].ID, 10), "", cookie, true); rec.Code != 303 {
		t.Errorf("revoke: %d", rec.Code)
	}
	if _, err := st.GetAPIKeyByHash(context.Background(), auth.HashToken(secret)); err == nil {
		t.Error("revoked key still valid")
	}
	// Sign out, then in again — wrong password first.
	if rec := do("POST", "/logout", "", cookie, false); rec.Code != 303 {
		t.Errorf("logout: %d", rec.Code)
	}
	if rec := do("GET", "/", "", cookie, false); rec.Code != 303 {
		t.Errorf("after logout: %d", rec.Code)
	}
	if rec := do("POST", "/login", "email=me%40example.com&password=nope", nil, false); rec.Code != 401 {
		t.Errorf("wrong password: %d", rec.Code)
	}
	rec = do("POST", "/login", "email=me%40example.com&password=correct+horse+battery&next=%2Fp%2Fshop", nil, false)
	if rec.Code != 303 || rec.Header().Get("Location") != "/p/shop" || len(rec.Result().Cookies()) == 0 {
		t.Errorf("login: %d %v", rec.Code, rec.Header())
	}
	if rec := do("POST", "/login", "email=me%40example.com&password=correct+horse+battery&next=https%3A%2F%2Fevil.example", nil, false); rec.Header().Get("Location") != "/" {
		t.Errorf("open redirect: %s", rec.Header().Get("Location"))
	}
}
