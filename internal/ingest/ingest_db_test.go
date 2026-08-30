package ingest

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/crashcartapp/crashcart/internal/config"
	"github.com/crashcartapp/crashcart/internal/db/sqlc"
	"github.com/crashcartapp/crashcart/internal/sentry"
	"github.com/crashcartapp/crashcart/internal/testdb"
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
	p.SampleKeepFirst, p.SampleRate = 2, 0 // keep first 2 per issue, then nothing
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
	if iss.EventCount != 3 || iss.StoredCount != 2 || iss.Status != "unresolved" || *iss.LastRelease != "1.0" || iss.Title != "NullPointerException: boom" {
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

	// An older build reports it too before it is resolved: both releases
	// are in the issue's set.
	if _, err := in.Ingest(ctx, p, sentry.Parse(envelope(crash("0.9", ts, 6)), now), now); err != nil {
		t.Fatal(err)
	}
	iss, _ = st.GetIssue(ctx, sqlc.GetIssueParams{ProjectID: p.ID, Fingerprint: fp})
	if len(iss.Releases) != 2 || iss.Releases[0] != "0.9" || iss.Releases[1] != "1.0" {
		t.Fatalf("releases = %v", iss.Releases)
	}
	// Resolve, then see it again on releases it was already known on
	// (old builds in the field): stays resolved.
	if _, err := st.SetIssueStatus(ctx, sqlc.SetIssueStatusParams{ProjectID: p.ID, Fingerprint: fp, Status: "resolved"}); err != nil {
		t.Fatal(err)
	}
	for _, rel := range []string{"1.0", "0.9"} {
		if _, err := in.Ingest(ctx, p, sentry.Parse(envelope(crash(rel, ts, 4)), now), now); err != nil {
			t.Fatal(err)
		}
		iss, _ = st.GetIssue(ctx, sqlc.GetIssueParams{ProjectID: p.ID, Fingerprint: fp})
		if iss.Status != "resolved" {
			t.Fatalf("event on known release %s should not regress: %s", rel, iss.Status)
		}
	}
	if len(iss.ResolvedReleases) != 2 {
		t.Fatalf("resolved_releases = %v", iss.ResolvedReleases)
	}
	// A release it was never seen on → regression.
	res, err = in.Ingest(ctx, p, sentry.Parse(envelope(crash("1.1", now.Format(time.RFC3339), 5)), now), now)
	if err != nil {
		t.Fatal(err)
	}
	iss, _ = st.GetIssue(ctx, sqlc.GetIssueParams{ProjectID: p.ID, Fingerprint: fp})
	if iss.Status != "regression" || len(res.Regressions) != 1 || *iss.LastRelease != "1.1" {
		t.Fatalf("expected regression: status=%s res=%+v", iss.Status, res)
	}

	// Sessions land and the release-health view sees them at once.
	sess := []byte(`{}` + "\n" + `{"type":"sessions"}` + "\n" +
		`{"attrs":{"release":"1.1"},"aggregates":[{"started":"` + now.Format(time.RFC3339) + `","exited":9,"crashed":1}]}` + "\n")
	res, err = in.Ingest(ctx, p, sentry.Parse(sess, now), now)
	if err != nil || res.Sessions != 2 {
		t.Fatalf("sessions: %+v %v", res, err)
	}
	var total, crashed int64
	st.Pool.QueryRow(ctx, "SELECT sum(total), sum(crashed) FROM release_health_hourly WHERE release='1.1'").Scan(&total, &crashed)
	if total != 10 || crashed != 1 {
		t.Fatalf("release health = %d/%d", crashed, total)
	}
	// One session reported twice (ok, then crashed) is one row with the final status.
	started := now.Format(time.RFC3339)
	one := func(status string) []byte {
		return []byte(`{}` + "\n" + `{"type":"session"}` + "\n" +
			`{"sid":"abc-123","status":"` + status + `","started":"` + started + `","attrs":{"release":"1.2"}}` + "\n")
	}
	in.Ingest(ctx, p, sentry.Parse(one("ok"), now), now)
	in.Ingest(ctx, p, sentry.Parse(one("crashed"), now), now)
	in.Ingest(ctx, p, sentry.Parse(one("ok"), now), now) // late/out-of-order update must not downgrade
	var rows int64
	var status string
	st.Pool.QueryRow(ctx, "SELECT count(*), min(status) FROM sessions WHERE release='1.2'").Scan(&rows, &status)
	if rows != 1 || status != "crashed" {
		t.Fatalf("session dedupe: rows=%d status=%s", rows, status)
	}

	// A resent envelope (SDK timeout / crash cache) is idempotent for stored
	// events: same ids, nothing re-counted. (Sampled-out events left no row
	// to match, so a resend of those is counted again — accepted.)
	before, _ := st.GetIssue(ctx, sqlc.GetIssueParams{ProjectID: p.ID, Fingerprint: fp})
	res, err = in.Ingest(ctx, p, sentry.Parse(envelope(crash("1.0", ts, 1)), now), now)
	if err != nil || res.Duplicates != 1 || res.Stored != 0 {
		t.Fatalf("resend: %+v %v", res, err)
	}
	after, _ := st.GetIssue(ctx, sqlc.GetIssueParams{ProjectID: p.ID, Fingerprint: fp})
	if after.EventCount != before.EventCount {
		t.Fatalf("resend counted: %d → %d", before.EventCount, after.EventCount)
	}

	// A declared platform flags foreign events without dropping them.
	ios := "ios"
	p.Platform = &ios
	res, err = in.Ingest(ctx, p, sentry.Parse(envelope(crash("1.1", now.Format(time.RFC3339), 7)), now), now)
	if err != nil || res.Mismatched != 1 || res.Received != 1 {
		t.Fatalf("android event into an ios project: %+v %v", res, err)
	}
	p.Platform = nil

	// Daily quota: the envelope that would cross it is rejected whole.
	day := now.Truncate(24 * time.Hour)
	usedBefore, _ := st.ProjectUsage(ctx, sqlc.ProjectUsageParams{ProjectID: p.ID, Day: day})
	if usedBefore == 0 {
		t.Fatal("no usage counted")
	}
	p.DailyQuota = 4 // more than that is already counted today
	res, err = in.Ingest(ctx, p, sentry.Parse(envelope(crash("1.1", now.Format(time.RFC3339), 8)), now), now)
	if !errors.Is(err, ErrQuota) || res.Stored != 0 {
		t.Fatalf("quota: res=%+v err=%v", res, err)
	}
	// The rollback left the day's count where it was, and the next
	// envelope is refused usedBefore any work (the count does not move).
	usedAfter, _ := st.ProjectUsage(ctx, sqlc.ProjectUsageParams{ProjectID: p.ID, Day: day})
	if usedAfter != usedBefore {
		t.Fatalf("usage usedAfter rejected envelope = %d, want %d", usedAfter, usedBefore)
	}
	if _, err := in.Ingest(ctx, p, sentry.Parse(envelope(crash("1.1", now.Format(time.RFC3339), 8)), now), now); !errors.Is(err, ErrQuota) {
		t.Fatalf("exhausted quota: %v", err)
	}
	if usedAfter, _ = st.ProjectUsage(ctx, sqlc.ProjectUsageParams{ProjectID: p.ID, Day: day}); usedAfter != usedBefore {
		t.Fatalf("usage usedAfter short-circuit = %d, want %d", usedAfter, usedBefore)
	}
	p.DailyQuota = 0
	if _, err := in.Ingest(ctx, p, sentry.Parse(envelope(crash("1.1", now.Format(time.RFC3339), 9)), now), now); err != nil {
		t.Fatalf("unlimited quota: %v", err)
	}

	// Every release the envelopes mentioned is on record, with its platforms.
	rels, err := st.ListReleases(ctx, sqlc.ListReleasesParams{ProjectID: p.ID, Limit: 10})
	if err != nil || len(rels) == 0 {
		t.Fatalf("releases: %d %v", len(rels), err)
	}
	for _, r := range rels {
		if r.Release == "" || r.FirstSeen.IsZero() {
			t.Errorf("release row %+v", r)
		}
	}

	// Hourly stats via the view (the hour is dirty, so computed live).
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
	if c := do(fmt.Sprintf("/api/%d/store/?sentry_key=secretkey", p.ID), ""); c != 200 {
		t.Fatalf("store → %d", c)
	}
	// sentry-python gzips every envelope.
	var gz bytes.Buffer
	zw := gzip.NewWriter(&gz)
	zw.Write(body)
	zw.Close()
	req := newRequest("POST", fmt.Sprintf("/api/%d/envelope/", p.ID), gz.Bytes())
	req.Header.Set("X-Sentry-Auth", "Sentry sentry_key=secretkey")
	req.Header.Set("Content-Encoding", "gzip")
	rec := newRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), `"received":1`) {
		t.Fatalf("gzip envelope → %d %s", rec.Code, rec.Body.String())
	}
	req = newRequest("POST", fmt.Sprintf("/api/%d/envelope/", p.ID), []byte("not gzip"))
	req.Header.Set("X-Sentry-Auth", "Sentry sentry_key=secretkey")
	req.Header.Set("Content-Encoding", "gzip")
	rec = newRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 413 {
		t.Fatalf("corrupt gzip → %d", rec.Code)
	}
	// Over the daily quota: 429 with Sentry's rate-limit header, so the SDK
	// stops sending (all categories) until the next UTC day.
	if _, err := st.Pool.Exec(ctx, "UPDATE projects SET daily_quota = 1 WHERE id = $1", p.ID); err != nil {
		t.Fatal(err)
	}
	in.byKey = nil // the DSN-key cache holds the project for a minute
	req = newRequest("POST", fmt.Sprintf("/api/%d/envelope/", p.ID), body)
	req.Header.Set("X-Sentry-Auth", "Sentry sentry_key=secretkey")
	rec = newRecorder()
	h.ServeHTTP(rec, req)
	limits := rec.Header().Get("X-Sentry-Rate-Limits")
	secs, _, _ := strings.Cut(limits, ":")
	n, _ := strconv.Atoi(secs)
	if rec.Code != 429 || !strings.HasSuffix(limits, ":error;transaction;session:project:quota") || n < 1 || n > 24*3600+1 || rec.Header().Get("Retry-After") != secs {
		t.Fatalf("over quota → %d %q retry-after=%q", rec.Code, limits, rec.Header().Get("Retry-After"))
	}
}

// Ungrouped events (nothing to fingerprint) take sample_rate from the
// start — with PII redaction on as well: redaction scrubs what is stored,
// it does not decide what is stored.
func TestIngestUngroupedSampledWithRedaction(t *testing.T) {
	st := testdb.New(t)
	ctx := context.Background()
	p, err := st.CreateProject(ctx, sqlc.CreateProjectParams{Slug: "app", Name: "App", PublicKey: "k"})
	if err != nil {
		t.Fatal(err)
	}
	p.SampleRate = 0
	in := &Ingester{Store: st, Cfg: config.Config{PIIRedact: true}, Log: slog.Default()}
	now := time.Now().UTC()
	info := fmt.Sprintf(`{"event_id":"i1","timestamp":%q,"level":"info","platform":"android","message":"user opened cart"}`, now.Add(-time.Minute).Format(time.RFC3339))
	res, err := in.Ingest(ctx, p, sentry.Parse(envelope(info), now), now)
	if err != nil {
		t.Fatal(err)
	}
	if res.Received != 1 || res.Stored != 0 || res.Sampled != 1 {
		t.Fatalf("result = %+v", res)
	}
}

// TestIngestClampsFarPastClock: an event (or session) dated before the
// retention window is a wrong device clock: it is stored at now, so
// issues.first_seen / releases.first_seen are not dragged back for good.
// A late event inside the window keeps its time.
func TestIngestClampsFarPastClock(t *testing.T) {
	st := testdb.New(t)
	ctx := context.Background()
	p, err := st.CreateProject(ctx, sqlc.CreateProjectParams{Slug: "app", Name: "App", PublicKey: "k"})
	if err != nil {
		t.Fatal(err)
	}
	in := &Ingester{Store: st, Cfg: config.Config{RetentionDays: 30}, Log: slog.Default()}
	now := time.Now().UTC()
	late := now.Add(-10 * 24 * time.Hour).Truncate(time.Second)
	env := sentry.Parse(envelope(crash("1.0", "1970-01-02T00:00:00Z", 1), crash("1.0", late.Format(time.RFC3339), 2)), now)
	env.Sessions = append(env.Sessions, sentry.Session{SID: "s1", Release: "1.0", Status: "ok", StartedAt: time.Date(1970, 1, 2, 0, 0, 0, 0, time.UTC), Count: 1})
	res, err := in.Ingest(ctx, p, env, now)
	if err != nil || res.Stored != 2 || res.Sessions != 1 {
		t.Fatalf("res=%+v err=%v", res, err)
	}
	iss, err := st.GetIssue(ctx, sqlc.GetIssueParams{ProjectID: p.ID, Fingerprint: res.NewIssues[0]})
	if err != nil {
		t.Fatal(err)
	}
	if !iss.FirstSeen.Equal(late) {
		t.Fatalf("first_seen = %v, want the late (in-window) event %v", iss.FirstSeen, late)
	}
	var at time.Time
	if err := st.Pool.QueryRow(ctx, `SELECT min(occurred_at) FROM events WHERE project_id = $1`, p.ID).Scan(&at); err != nil || at.Year() == 1970 {
		t.Fatalf("oldest stored event = %v %v (1970 event stored as-is)", at, err)
	}
	if err := st.Pool.QueryRow(ctx, `SELECT started_at FROM sessions WHERE sid = 's1'`).Scan(&at); err != nil || at.Year() == 1970 {
		t.Fatalf("session started_at = %v %v", at, err)
	}
	if err := st.Pool.QueryRow(ctx, `SELECT first_seen FROM releases WHERE project_id = $1 AND release = '1.0'`, p.ID).Scan(&at); err != nil || !at.Equal(late) {
		t.Fatalf("releases.first_seen = %v %v, want %v", at, err, late)
	}
}
