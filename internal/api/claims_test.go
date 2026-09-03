package api

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/at-least/crashcart/internal/config"
	"github.com/at-least/crashcart/internal/sentry"
	"github.com/at-least/crashcart/internal/symbolicate"
	"github.com/at-least/crashcart/internal/testdb"
)

// TestRateLimitBeforeKeyCheck: the limiter runs before the key lookup, so
// a flood of bad keys is answered 429 after the budget, not 401 forever
// (each 401 would cost a database lookup).
func TestRateLimitBeforeKeyCheck(t *testing.T) {
	st := testdb.New(t)
	mux := http.NewServeMux()
	(&Handler{Store: st, Cfg: config.Config{RateLimit: 1}, Log: slog.Default(), Symbols: &symbolicate.Service{Store: st, DSYM: symbolicate.NewDSYMClient("")}}).Register(mux)
	call := func(key string) int {
		req := httptest.NewRequest("GET", "/api/projects", nil)
		req.Header.Set("Authorization", "Bearer "+key)
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)
		return rec.Code
	}
	if c := call("cc_bogus"); c != 401 {
		t.Fatalf("first bad key: %d", c)
	}
	if c := call("cc_bogus"); c != 429 {
		t.Fatalf("second bad key must be rate limited before the key check: %d", c)
	}
	// Another credential has its own budget: still 401, not 429.
	if c := call("cc_other"); c != 401 {
		t.Fatalf("other credential: %d", c)
	}
}

// TestStatusByAndIgnoreBounds: a status change through the API records
// the key's name as the actor; ignore_minutes lands ignore_until at now +
// minutes (RFC3339 UTC) and until-escalating records the baseline.
func TestStatusByAndIgnoreBounds(t *testing.T) {
	e := newEnv(t)
	p := e.createProject("demo")
	e.seed(p)
	fp := e.get("/api/projects/demo/issues", 200)["issues"].([]any)[0].(map[string]any)["fingerprint"].(string)

	before := time.Now().UTC()
	rec, out := e.do("PATCH", "/api/projects/demo/issues/"+fp, map[string]any{"status": "ignored", "ignore_minutes": 60, "ignore_until_escalating": true})
	if rec.Code != 200 || out["status_by"] != "test" {
		t.Fatalf("patch: %d %v (status_by must be the API key's name)", rec.Code, out)
	}
	untilS, _ := out["ignore_until"].(string)
	until, err := time.Parse(time.RFC3339, untilS)
	if err != nil || !strings.HasSuffix(untilS, "Z") {
		t.Fatalf("ignore_until = %q: %v (want RFC3339 UTC)", untilS, err)
	}
	if lo, hi := before.Add(59*time.Minute), time.Now().UTC().Add(61*time.Minute); until.Before(lo) || until.After(hi) {
		t.Errorf("ignore_until = %v, want ≈ now + 60 min", until)
	}
	var baseline *int64
	var statusBy string
	if err := e.st.Pool.QueryRow(context.Background(), "SELECT ignore_baseline, status_by FROM issues WHERE project_id = $1 AND fingerprint = $2", p.ID, sentry.ID(fp)).Scan(&baseline, &statusBy); err != nil {
		t.Fatal(err)
	}
	if baseline == nil || statusBy != "test" {
		t.Errorf("baseline = %v status_by = %q (until-escalating must record the 24 h baseline)", baseline, statusBy)
	}
	// Ignoring without escalating leaves no baseline behind.
	if rec, _ := e.do("POST", "/api/projects/demo/issues/bulk", map[string]any{"fingerprints": []string{fp}, "status": "ignored", "ignore_events": 3}); rec.Code != 200 {
		t.Fatal(rec.Code)
	}
	if err := e.st.Pool.QueryRow(context.Background(), "SELECT ignore_baseline, status_by FROM issues WHERE project_id = $1 AND fingerprint = $2", p.ID, sentry.ID(fp)).Scan(&baseline, &statusBy); err != nil {
		t.Fatal(err)
	}
	if baseline != nil || statusBy != "test" {
		t.Errorf("after bulk ignore_events: baseline = %v status_by = %q", baseline, statusBy)
	}
	// A key with another name is another actor.
	var name string
	if err := e.st.Pool.QueryRow(context.Background(), "UPDATE api_keys SET name = 'deploy-bot' RETURNING name").Scan(&name); err != nil {
		t.Fatal(err)
	}
	if _, out := e.do("PATCH", "/api/projects/demo/issues/"+fp, map[string]any{"status": "resolved"}); out["status_by"] != "deploy-bot" {
		t.Errorf("status_by = %v, want deploy-bot", out["status_by"])
	}
}

// TestCursorPagingEqualTimestamps: events sharing one occurred_at are
// paged by (occurred_at, event_id) — every event exactly once across
// pages, the filter kept on every page, nothing skipped at the boundary.
func TestCursorPagingEqualTimestamps(t *testing.T) {
	e := newEnv(t)
	p := e.createProject("demo")
	now := time.Now().UTC()
	ts := now.Add(-time.Hour).Format(time.RFC3339)
	var items []string
	for i := 0; i < 5; i++ {
		id := fmt.Sprintf("%031x%d", 0xabc, i)
		items = append(items, `{"type":"event"}`, event(id, ts, "fatal", "1.0", "u", "NullPointerException", false, 1, ""))
	}
	// One handled error at the same instant: excluded by the filter on every page.
	items = append(items, `{"type":"event"}`, event(strings.Repeat("d", 32), ts, "error", "1.0", "u", "IOException", true, 2, ""))
	body := []byte("{\"event_id\":\"h\"}\n" + strings.Join(items, "\n") + "\n")
	if res, err := e.in.Ingest(context.Background(), p, sentry.Parse(body, now), now); err != nil || res.Stored != 6 {
		t.Fatalf("ingest: %+v %v", res, err)
	}
	seen := map[string]int{}
	pages := 0
	next := ""
	for {
		q := "/api/projects/demo/events?limit=2&handled=false"
		if next != "" {
			q += "&before=" + url.QueryEscape(next)
		}
		page := e.get(q, 200)
		pages++
		evs := page["events"].([]any)
		for _, ev := range evs {
			m := ev.(map[string]any)
			if m["level"] != "fatal" {
				t.Errorf("page %d: filter lost: %v", pages, m["event_id"])
			}
			seen[m["event_id"].(string)]++
		}
		if page["more"] != true {
			if page["next_before"] != nil {
				t.Errorf("last page carries a cursor: %v", page["next_before"])
			}
			break
		}
		if len(evs) != 2 {
			t.Fatalf("page %d: %d rows with more=true", pages, len(evs))
		}
		next = page["next_before"].(string)
		if pages > 10 {
			t.Fatal("paging does not terminate")
		}
	}
	if pages != 3 || len(seen) != 5 {
		t.Errorf("pages = %d, distinct = %d (want 3 pages, 5 events)", pages, len(seen))
	}
	for id, n := range seen {
		if n != 1 {
			t.Errorf("%s returned %d times", id, n)
		}
	}
}

// TestEventWindowBoundaries: from is inclusive and to exclusive on
// occurred_at, at the second.
func TestEventWindowBoundaries(t *testing.T) {
	e := newEnv(t)
	p := e.createProject("demo")
	now := time.Now().UTC().Truncate(time.Second)
	t0, t1, t2 := now.Add(-3*time.Hour), now.Add(-2*time.Hour), now.Add(-time.Hour)
	var items []string
	for i, at := range []time.Time{t0, t1, t2} {
		items = append(items, `{"type":"event"}`, event(strings.Repeat(fmt.Sprint(i+1), 32), at.Format(time.RFC3339), "error", "1.0", "u", "E", true, 1, ""))
	}
	body := []byte("{\"event_id\":\"h\"}\n" + strings.Join(items, "\n") + "\n")
	if res, err := e.in.Ingest(context.Background(), p, sentry.Parse(body, now), now); err != nil || res.Stored != 3 {
		t.Fatalf("ingest: %+v %v", res, err)
	}
	ids := func(q string) []string {
		var out []string
		for _, ev := range e.get(q, 200)["events"].([]any) {
			out = append(out, ev.(map[string]any)["event_id"].(string))
		}
		return out
	}
	rfc := func(t time.Time) string { return url.QueryEscape(t.Format(time.RFC3339)) }
	if got := ids("/api/projects/demo/events?from=" + rfc(t0) + "&to=" + rfc(t1)); len(got) != 1 || got[0] != strings.Repeat("1", 32) {
		t.Errorf("[t0,t1) = %v, want the t0 event only (from inclusive, to exclusive)", got)
	}
	if got := ids("/api/projects/demo/events?from=" + rfc(t1) + "&to=" + rfc(t2.Add(time.Second))); len(got) != 2 {
		t.Errorf("[t1,t2+1s) = %v, want t2 and t1", got)
	}
	if got := ids("/api/projects/demo/events?from=" + rfc(t0.Add(time.Second)) + "&to=" + rfc(t2)); len(got) != 1 || got[0] != strings.Repeat("2", 32) {
		t.Errorf("(t0,t2) = %v, want t1 only", got)
	}
	// A window given in another zone is the same instant.
	if got := ids("/api/projects/demo/events?from=" + url.QueryEscape(t0.In(time.FixedZone("X", 3600)).Format(time.RFC3339)) + "&to=" + rfc(t1)); len(got) != 1 {
		t.Errorf("zoned from = %v", got)
	}
}

// TestProjectDeleteCascadesEveryTable: after DELETE nothing of the project
// remains in any table that carries a project_id — the list is taken from
// the catalog, so a new per-project table without the cascade fails here.
func TestProjectDeleteCascadesEveryTable(t *testing.T) {
	e := newEnv(t)
	p := e.createProject("demo")
	e.seed(p)
	ctx := context.Background()
	rows, err := e.st.Pool.Query(ctx, `SELECT DISTINCT c.table_name FROM information_schema.columns c
		JOIN information_schema.tables t ON t.table_schema = c.table_schema AND t.table_name = c.table_name
		WHERE c.column_name = 'project_id' AND c.table_schema = current_schema() AND t.table_type = 'BASE TABLE'`)
	if err != nil {
		t.Fatal(err)
	}
	var tables []string
	for rows.Next() {
		var n string
		rows.Scan(&n)
		if strings.Contains(n, "_p2") || strings.HasSuffix(n, "_default") { // partitions are reached through their parent
			continue
		}
		tables = append(tables, n)
	}
	rows.Close()
	if len(tables) < 8 {
		t.Fatalf("only %d per-project tables found: %v", len(tables), tables)
	}
	populated := 0
	for _, tbl := range tables {
		var n int
		if err := e.st.Pool.QueryRow(ctx, "SELECT count(*) FROM "+tbl+" WHERE project_id = $1", p.ID).Scan(&n); err != nil {
			t.Fatal(tbl, err)
		}
		if n > 0 {
			populated++
		}
	}
	if populated < 6 {
		t.Fatalf("seed populated only %d tables", populated)
	}
	if rec, _ := e.do("DELETE", "/api/projects/demo", nil); rec.Code != 204 {
		t.Fatalf("delete: %d", rec.Code)
	}
	for _, tbl := range tables {
		var n int
		if err := e.st.Pool.QueryRow(ctx, "SELECT count(*) FROM "+tbl+" WHERE project_id = $1", p.ID).Scan(&n); err != nil {
			t.Fatal(tbl, err)
		}
		if n != 0 {
			t.Errorf("%s: %d rows left after project delete", tbl, n)
		}
	}
}
