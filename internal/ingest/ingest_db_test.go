package ingest

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/newlix/crashcart/internal/config"
	"github.com/newlix/crashcart/internal/db/sqlc"
	"github.com/newlix/crashcart/internal/sentry"
	"github.com/newlix/crashcart/internal/testdb"
)

func crash(release, ts string, n int) string {
	return fmt.Sprintf(`{"event_id":"e%d","timestamp":%q,"level":"error","platform":"android","release":%q,
	 "user":{"id":"u%d"},"tags":{"device_id":"d1","build":"42"},
	 "exception":{"values":[{"type":"NullPointerException","value":"boom","mechanism":{"handled":false},
	   "stacktrace":{"frames":[{"filename":"Main.java","function":"main","lineno":1,"in_app":false},
	                           {"filename":"CartFragment.java","function":"load","lineno":%d,"in_app":true}]}}]}}`,
		n, ts, release, n%3, 100+n)
}

func envelope(items ...string) []byte {
	var b strings.Builder
	b.WriteString(`{"event_id":"h"}` + "\n")
	for _, it := range items {
		var c bytes.Buffer
		if err := json.Compact(&c, []byte(it)); err != nil {
			panic(err)
		}
		b.WriteString(`{"type":"event"}` + "\n" + c.String() + "\n")
	}
	return []byte(b.String())
}

func TestIngestLifecycle(t *testing.T) {
	st := testdb.New(t)
	ctx := context.Background()
	p, err := st.CreateProject(ctx, sqlc.CreateProjectParams{Slug: "app", Name: "App", PublicKey: "k"})
	if err != nil {
		t.Fatal(err)
	}
	p.SampleKeepFirst, p.SampleRate = 2, 0 // keep first 2 per issue, then nothing (fatal always)
	in := &Ingester{Store: st, Cfg: config.Config{}, Log: slog.Default()}
	now := time.Now().UTC()
	ts := now.Add(-time.Minute).Format(time.RFC3339)

	// Three events of one issue on 1.0: 2 stored, 1 sampled out; count exact.
	env := sentry.Parse(envelope(crash("1.0", ts, 1), crash("1.0", ts, 2), crash("1.0", ts, 3)), now)
	res, err := in.Ingest(ctx, p, env, now)
	if err != nil {
		t.Fatal(err)
	}
	if res.Stored != 2 || res.Sampled != 1 || len(res.NewIssues) != 1 {
		t.Fatalf("result = %+v", res)
	}
	fp := res.NewIssues[0]
	iss, err := st.GetIssue(ctx, sqlc.GetIssueParams{ProjectID: p.ID, Fingerprint: fp})
	if err != nil {
		t.Fatal(err)
	}
	if iss.EventCount != 3 || iss.StoredCount != 2 || iss.Status != "unresolved" || *iss.LastRelease != "1.0" || iss.Title != "NullPointerException" {
		t.Fatalf("issue = %+v", iss)
	}
	var n int
	st.Pool.QueryRow(ctx, "SELECT count(*) FROM events WHERE fingerprint = $1", fp).Scan(&n)
	if n != 2 {
		t.Fatalf("stored events = %d", n)
	}
	if n, _ := st.CountJobs(ctx); n != 1 { // one new_issue alert job
		t.Fatalf("jobs = %d", n)
	}

	// Resolve, then see it again on the same release: stays resolved.
	if _, err := st.SetIssueStatus(ctx, sqlc.SetIssueStatusParams{ProjectID: p.ID, Fingerprint: fp, Status: "resolved"}); err != nil {
		t.Fatal(err)
	}
	if _, err := in.Ingest(ctx, p, sentry.Parse(envelope(crash("1.0", ts, 4)), now), now); err != nil {
		t.Fatal(err)
	}
	iss, _ = st.GetIssue(ctx, sqlc.GetIssueParams{ProjectID: p.ID, Fingerprint: fp})
	if iss.Status != "resolved" {
		t.Fatalf("same-release event should not regress: %s", iss.Status)
	}
	// A newer release → regression.
	res, err = in.Ingest(ctx, p, sentry.Parse(envelope(crash("1.1", now.Format(time.RFC3339), 5)), now), now)
	if err != nil {
		t.Fatal(err)
	}
	iss, _ = st.GetIssue(ctx, sqlc.GetIssueParams{ProjectID: p.ID, Fingerprint: fp})
	if iss.Status != "regression" || len(res.Regressions) != 1 || *iss.LastRelease != "1.1" {
		t.Fatalf("expected regression: status=%s res=%+v", iss.Status, res)
	}

	// Sessions land and the release-health aggregate sees them in real time.
	sess := []byte(`{}` + "\n" + `{"type":"sessions"}` + "\n" +
		`{"attrs":{"release":"1.1"},"aggregates":[{"started":"` + now.Format(time.RFC3339) + `","exited":9,"crashed":1}]}` + "\n")
	res, err = in.Ingest(ctx, p, sentry.Parse(sess, now), now)
	if err != nil || res.Sessions != 2 {
		t.Fatalf("sessions: %+v %v", res, err)
	}
	var total, crashed int64
	st.Pool.QueryRow(ctx, "SELECT sum(total), sum(crashed) FROM release_health_daily WHERE release='1.1'").Scan(&total, &crashed)
	if total != 10 || crashed != 1 {
		t.Fatalf("release health = %d/%d", crashed, total)
	}

	// Hourly stats via the continuous aggregate (real-time).
	var crashes int64
	st.Pool.QueryRow(ctx, "SELECT sum(crashes) FROM event_stats_hourly WHERE project_id=$1", p.ID).Scan(&crashes)
	if crashes != 2 { // only the first 2 were stored (keep_first=2, rate 0); later ones were counted but sampled out
		t.Fatalf("crashes = %d", crashes)
	}
}

func TestIngestHTTP(t *testing.T) {
	st := testdb.New(t)
	ctx := context.Background()
	p, _ := st.CreateProject(ctx, sqlc.CreateProjectParams{Slug: "app", Name: "App", PublicKey: "secretkey"})
	in := &Ingester{Store: st, Cfg: config.Config{RateLimit: 0}, Log: slog.Default()}
	h := in.Handler()
	now := time.Now().UTC().Format(time.RFC3339)
	body := envelope(crash("1.0", now, 1))

	do := func(path, auth string) int {
		req := newRequest("POST", path, body)
		if auth != "" {
			req.Header.Set("X-Sentry-Auth", auth)
		}
		rec := newRecorder()
		h.ServeHTTP(rec, req)
		return rec.Code
	}
	if c := do(fmt.Sprintf("/api/%d/envelope/", p.ID), ""); c != 401 {
		t.Fatalf("no key → %d", c)
	}
	if c := do(fmt.Sprintf("/api/%d/envelope/", p.ID), "Sentry sentry_key=wrong, sentry_version=7"); c != 401 {
		t.Fatalf("wrong key → %d", c)
	}
	if c := do("/api/999/envelope/", "Sentry sentry_key=secretkey"); c != 401 {
		t.Fatalf("wrong project id → %d", c)
	}
	if c := do(fmt.Sprintf("/api/%d/envelope/", p.ID), "Sentry sentry_key=secretkey, sentry_version=7"); c != 200 {
		t.Fatalf("valid → %d", c)
	}
	if c := do(fmt.Sprintf("/api/%d/store/?sentry_key=secretkey", p.ID), ""); c != 200 { // store accepts envelope? no: a bare event
		t.Fatalf("store → %d", c)
	}
}
