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

	"github.com/crashcartapp/crashcart/internal/config"
	"github.com/crashcartapp/crashcart/internal/db/sqlc"
	"github.com/crashcartapp/crashcart/internal/symbolicate"
	"github.com/crashcartapp/crashcart/internal/testdb"
)

func crashItem(release string, n int) string {
	ev := fmt.Sprintf(`{"event_id":"e%d","timestamp":%q,"level":"error","platform":"android","release":%q,"environment":"production",
	 "user":{"id":"u%d"},"tags":{"device_id":"d%d","build":"42"},"contexts":{"os":{"version":"14"},"device":{"model":"Pixel 8"}},
	 "exception":{"values":[{"type":"NullPointerException","value":"boom","mechanism":{"handled":false},
	   "stacktrace":{"frames":[{"filename":"Main.java","function":"main","lineno":1,"in_app":false},
	                           {"filename":"CartFragment.java","function":"load","lineno":142,"in_app":true}]}}]}}`,
		n, time.Now().UTC().Add(-time.Minute).Format(time.RFC3339), release, n%3, n%2)
	var c bytes.Buffer
	if err := json.Compact(&c, []byte(ev)); err != nil {
		panic(err)
	}
	return `{"type":"event"}` + "\n" + c.String() + "\n"
}

// TestEndToEnd drives the real HTTP surface: Sentry ingest → JSON API → viewer.
func TestEndToEnd(t *testing.T) {
	st := testdb.New(t)
	ctx := context.Background()
	cfg := config.Config{APIKeys: []string{"apikey"}, CORSOrigin: "*", RetentionDays: 30}
	p, err := st.CreateProject(ctx, sqlc.CreateProjectParams{Slug: "shop", Name: "Shop", PublicKey: "dsnkey"})
	if err != nil {
		t.Fatal(err)
	}
	h := New(Deps{Store: st, Cfg: cfg, Log: slog.Default(), Symbols: &symbolicate.Service{Store: st, DSYM: symbolicate.NewDSYMClient("")}})
	srv := httptest.NewServer(h)
	defer srv.Close()

	do := func(method, path string, body io.Reader, hdr map[string]string) (*http.Response, string) {
		req, _ := http.NewRequest(method, srv.URL+path, body)
		for k, v := range hdr {
			req.Header.Set(k, v)
		}
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		b, _ := io.ReadAll(res.Body)
		res.Body.Close()
		return res, string(b)
	}

	// Ingest like an SDK: three events of one issue + a sessions aggregate.
	env := `{"event_id":"h","sent_at":"` + time.Now().UTC().Format(time.RFC3339) + `"}` + "\n" +
		crashItem("2.4.0", 1) + crashItem("2.4.0", 2) + crashItem("2.4.0", 3) +
		`{"type":"sessions"}` + "\n" + `{"attrs":{"release":"2.4.0"},"aggregates":[{"started":"` + time.Now().UTC().Format(time.RFC3339) + `","exited":97,"crashed":3}]}` + "\n"
	res, body := do("POST", fmt.Sprintf("/api/%d/envelope/", p.ID), strings.NewReader(env), map[string]string{"X-Sentry-Auth": "Sentry sentry_key=dsnkey, sentry_version=7"})
	if res.StatusCode != 200 || !strings.Contains(body, `"stored":3`) {
		t.Fatalf("ingest: %d %s", res.StatusCode, body)
	}
	if res, _ := do("POST", fmt.Sprintf("/api/%d/envelope/", p.ID), strings.NewReader(env), nil); res.StatusCode != 401 {
		t.Fatalf("ingest without key: %d", res.StatusCode)
	}

	// Browser SDKs preflight when they add headers: the SDK endpoint must answer.
	res, _ = do("OPTIONS", fmt.Sprintf("/api/%d/envelope/", p.ID), nil, map[string]string{"Origin": "https://app.example", "Access-Control-Request-Method": "POST"})
	if res.StatusCode != 204 || res.Header.Get("Access-Control-Allow-Origin") != "*" || !strings.Contains(res.Header.Get("Access-Control-Allow-Headers"), "X-Sentry-Auth") {
		t.Fatalf("preflight: %d %v", res.StatusCode, res.Header)
	}

	// JSON API: auth, then the issue-centric reads.
	if res, _ := do("GET", "/api/projects/shop/issues", nil, nil); res.StatusCode != 401 {
		t.Fatalf("api without bearer: %d", res.StatusCode)
	}
	auth := map[string]string{"Authorization": "Bearer apikey"}
	res, body = do("GET", "/api/projects/shop/issues", nil, auth)
	if res.StatusCode != 200 || !strings.Contains(body, "NullPointerException") || !strings.Contains(body, `"event_count":3`) {
		t.Fatalf("issues: %d %s", res.StatusCode, body)
	}
	var issues struct {
		Issues []struct {
			Fingerprint string `json:"fingerprint"`
			Users       int64  `json:"users"`
		} `json:"issues"`
	}
	json.Unmarshal([]byte(body), &issues)
	if len(issues.Issues) != 1 || issues.Issues[0].Users != 3 {
		t.Fatalf("issues parsed: %+v", issues)
	}
	fp := issues.Issues[0].Fingerprint
	res, body = do("GET", "/api/projects/shop/issues/"+fp, nil, auth)
	if res.StatusCode != 200 || !strings.Contains(body, "Pixel 8") {
		t.Fatalf("issue detail: %d %s", res.StatusCode, body)
	}
	res, body = do("GET", "/api/projects/shop/overview", nil, auth)
	if res.StatusCode != 200 || !strings.Contains(body, `"crashes":3`) || !strings.Contains(body, `"new_issues":1`) {
		t.Fatalf("overview: %d %s", res.StatusCode, body)
	}
	res, body = do("GET", "/api/projects/shop/releases", nil, auth)
	if res.StatusCode != 200 || !strings.Contains(body, "2.4.0") || !strings.Contains(body, "0.97") {
		t.Fatalf("releases: %d %s", res.StatusCode, body)
	}
	res, body = do("GET", "/api/projects/shop/events?level=error&tag.build=42", nil, auth)
	if res.StatusCode != 200 || strings.Count(body, `"event_id"`) != 3 {
		t.Fatalf("events: %d %s", res.StatusCode, body)
	}
	res, body = do("PATCH", "/api/projects/shop/issues/"+fp, strings.NewReader(`{"status":"resolved"}`), auth)
	if res.StatusCode != 200 || !strings.Contains(body, `"resolved"`) {
		t.Fatalf("patch: %d %s", res.StatusCode, body)
	}
	// Same issue on a new release → regression, visible everywhere.
	env2 := `{"event_id":"h2"}` + "\n" + crashItem("2.4.1", 9)
	do("POST", fmt.Sprintf("/api/%d/envelope/", p.ID), strings.NewReader(env2), map[string]string{"X-Sentry-Auth": "Sentry sentry_key=dsnkey"})
	res, body = do("GET", "/api/projects/shop/issues/"+fp, nil, auth)
	if !strings.Contains(body, `"status":"regression"`) {
		t.Fatalf("regression: %s", body)
	}

	// Viewer: every page renders, mutations need HX-Request.
	for _, path := range []string{"/", "/p/shop", "/p/shop/issues", "/p/shop/issues/" + fp, "/p/shop/events", "/p/shop/releases", "/p/shop/releases/2.4.0", "/p/shop/settings", "/static/app.css", "/static/htmx.min.js"} {
		res, body := do("GET", path, nil, nil)
		if res.StatusCode != 200 {
			t.Errorf("GET %s → %d: %.200s", path, res.StatusCode, body)
		}
	}
	res, body = do("GET", "/p/shop/issues/"+fp, nil, nil)
	if !strings.Contains(body, "CartFragment.java") || !strings.Contains(body, "Pixel 8") {
		t.Errorf("issue page lacks stack/breakdown: %.300s", body)
	}
	form := strings.NewReader("fp=" + fp + "&status=ignored")
	if res, _ := do("POST", "/p/shop/issues/bulk", form, map[string]string{"Content-Type": "application/x-www-form-urlencoded"}); res.StatusCode != 403 {
		t.Errorf("bulk without HX-Request → %d", res.StatusCode)
	}
	form = strings.NewReader("fp=" + fp + "&status=ignored")
	if res, body := do("POST", "/p/shop/issues/bulk", form, map[string]string{"Content-Type": "application/x-www-form-urlencoded", "HX-Request": "true"}); res.StatusCode != 200 {
		t.Errorf("bulk → %d %.200s", res.StatusCode, body)
	}
	iss, _ := st.GetIssue(ctx, sqlc.GetIssueParams{ProjectID: p.ID, Fingerprint: fp})
	if iss.Status != "ignored" {
		t.Errorf("bulk did not apply: %s", iss.Status)
	}
	if res, _ := do("GET", "/health", nil, nil); res.StatusCode != 200 {
		t.Errorf("health → %d", res.StatusCode)
	}
}
