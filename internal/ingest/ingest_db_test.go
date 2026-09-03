package ingest

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/at-least/crashcart/internal/config"
	"github.com/at-least/crashcart/internal/sentry"
	"github.com/at-least/crashcart/internal/store"
	"github.com/at-least/crashcart/internal/testdb"
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
	p, err := store.CreateProject(ctx, st.Pool, "app", "App", nil, "k")
	if err != nil {
		t.Fatal(err)
	}
	p.SampleKeepFirst, p.SampleRate = 2, 0 // keep first 2 per issue (× UnhandledKeepFactor for unhandled), then nothing
	in := &Ingester{Store: st, Cfg: config.Config{}, Log: slog.Default()}
	now := time.Now().UTC()
	ts := now.Add(-time.Minute).Format(time.RFC3339)

	// keep+1 unhandled events of one issue on 1.0: keep stored, 1 sampled out; count exact.
	const keep = 2 * UnhandledKeepFactor
	var items []string
	for i := 1; i <= keep+1; i++ {
		items = append(items, crash("1.0", ts, i))
	}
	env := sentry.Parse(envelope(items...), now)
	res, err := in.Ingest(ctx, p, env, now)
	if err != nil {
		t.Fatal(err)
	}
	if res.Stored != keep || res.Sampled != 1 || len(res.NewIssues) != 1 {
		t.Fatalf("result = %+v", res)
	}
	fp := res.NewIssues[0]
	iss, err := store.GetIssue(ctx, st.Pool, p.ID, fp)
	if err != nil {
		t.Fatal(err)
	}
	if iss.EventCount != keep+1 || iss.StoredCount != keep || iss.Status != "unresolved" || *iss.LastRelease != "1.0" || iss.Title != "NullPointerException: boom" {
		t.Fatalf("issue = %+v", iss)
	}
	var n int
	st.Pool.QueryRow(ctx, "SELECT count(*) FROM events WHERE fingerprint = $1", fp).Scan(&n)
	if n != keep {
		t.Fatalf("stored events = %d", n)
	}
	if n, _ := store.CountJobs(ctx, st.Pool); n != 1 { // one new_issue alert job
		t.Fatalf("jobs = %d", n)
	}

	// An older build reports it too before it is resolved: both releases
	// are in the issue's set.
	if _, err := in.Ingest(ctx, p, sentry.Parse(envelope(crash("0.9", ts, 6)), now), now); err != nil {
		t.Fatal(err)
	}
	iss, _ = store.GetIssue(ctx, st.Pool, p.ID, fp)
	if len(iss.Releases) != 2 || iss.Releases[0] != "0.9" || iss.Releases[1] != "1.0" {
		t.Fatalf("releases = %v", iss.Releases)
	}
	// Resolve, then see it again on releases it was already known on
	// (old builds in the field): stays resolved.
	if _, err := store.SetIssueStatus(ctx, st.Pool, store.SetIssueStatusParams{ProjectID: p.ID, Fingerprint: fp, Status: "resolved"}); err != nil {
		t.Fatal(err)
	}
	for _, rel := range []string{"1.0", "0.9"} {
		if _, err := in.Ingest(ctx, p, sentry.Parse(envelope(crash(rel, ts, 4)), now), now); err != nil {
			t.Fatal(err)
		}
		iss, _ = store.GetIssue(ctx, st.Pool, p.ID, fp)
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
	iss, _ = store.GetIssue(ctx, st.Pool, p.ID, fp)
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
	before, _ := store.GetIssue(ctx, st.Pool, p.ID, fp)
	res, err = in.Ingest(ctx, p, sentry.Parse(envelope(crash("1.0", ts, 1)), now), now)
	if err != nil || res.Duplicates != 1 || res.Stored != 0 {
		t.Fatalf("resend: %+v %v", res, err)
	}
	after, _ := store.GetIssue(ctx, st.Pool, p.ID, fp)
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

	// Daily quota: uncounted while unlimited (nothing reads it), exact
	// from the moment a quota is set, and the envelope that would cross
	// it is rejected whole.
	day := now.Truncate(24 * time.Hour)
	if used, _ := store.ProjectUsage(ctx, st.Pool, p.ID, day); used != 0 {
		t.Fatalf("usage counted while unlimited: %d", used)
	}
	p.DailyQuota = 4
	res, err = in.Ingest(ctx, p, sentry.Parse(envelope(crash("1.1", now.Format(time.RFC3339), 20)), now), now)
	if err != nil || res.Received != 1 {
		t.Fatalf("first event against a fresh quota: res=%+v err=%v", res, err)
	}
	usedBefore, _ := store.ProjectUsage(ctx, st.Pool, p.ID, day)
	if usedBefore != 1 {
		t.Fatalf("usage = %d, want 1 (counting starts when the quota is set, not the day's true total)", usedBefore)
	}
	items = []string{crash("1.1", now.Format(time.RFC3339), 21), crash("1.1", now.Format(time.RFC3339), 22),
		crash("1.1", now.Format(time.RFC3339), 23), crash("1.1", now.Format(time.RFC3339), 24)} // 1 + 4 > quota of 4
	res, err = in.Ingest(ctx, p, sentry.Parse(envelope(items...), now), now)
	if !errors.Is(err, ErrQuota) || res.Stored != 0 {
		t.Fatalf("quota: res=%+v err=%v", res, err)
	}
	// The rollback left the day's count where it was, and the next
	// envelope is refused before any work (the count does not move).
	usedAfter, _ := store.ProjectUsage(ctx, st.Pool, p.ID, day)
	if usedAfter != usedBefore {
		t.Fatalf("usage after rejected envelope = %d, want %d", usedAfter, usedBefore)
	}
	if _, err := in.Ingest(ctx, p, sentry.Parse(envelope(crash("1.1", now.Format(time.RFC3339), 25)), now), now); !errors.Is(err, ErrQuota) {
		t.Fatalf("exhausted quota: %v", err)
	}
	if usedAfter, _ = store.ProjectUsage(ctx, st.Pool, p.ID, day); usedAfter != usedBefore {
		t.Fatalf("usage after short-circuit = %d, want %d", usedAfter, usedBefore)
	}
	// Lifting the quota stops counting again: accepted, usage untouched.
	p.DailyQuota = 0
	if _, err := in.Ingest(ctx, p, sentry.Parse(envelope(crash("1.1", now.Format(time.RFC3339), 26)), now), now); err != nil {
		t.Fatalf("unlimited quota: %v", err)
	}
	if usedAfter, _ = store.ProjectUsage(ctx, st.Pool, p.ID, day); usedAfter != usedBefore {
		t.Fatalf("usage after unlimited accept = %d, want %d (writes skipped again)", usedAfter, usedBefore)
	}

	// Every release the envelopes mentioned is on record — those seen only
	// through sessions (1.2) too — with the platforms of its events (a
	// session names none) and the earliest time it was seen.
	rels, err := store.ListReleases(ctx, st.Pool, p.ID, 10)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]store.Release{}
	for _, r := range rels {
		got[r.Release] = r
	}
	if len(got) != 4 || len(got["1.0"].Platforms) != 1 || got["1.0"].Platforms[0] != "android" || len(got["1.2"].Platforms) != 0 {
		t.Fatalf("releases = %+v", rels)
	}
	if tsAt, _ := time.Parse(time.RFC3339, ts); !got["0.9"].FirstSeen.Equal(tsAt) || !got["1.0"].FirstSeen.Equal(tsAt) {
		t.Fatalf("first_seen 0.9=%v 1.0=%v, want %v", got["0.9"].FirstSeen, got["1.0"].FirstSeen, tsAt)
	}

	// Hourly stats via the view (the hour is dirty, so computed live).
	var unhandled int64
	st.Pool.QueryRow(ctx, "SELECT sum(unhandled) FROM event_stats_hourly WHERE project_id=$1", p.ID).Scan(&unhandled)
	if unhandled != keep { // only the first keep were stored (rate 0); later ones were counted but sampled out
		t.Fatalf("unhandled = %d", unhandled)
	}
}

func TestIngestHTTP(t *testing.T) {
	st := testdb.New(t)
	ctx := context.Background()
	p, _ := store.CreateProject(ctx, st.Pool, "app", "App", nil, "secretkey")
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
	// Over the daily quota: counting starts fresh when the quota is set
	// (nothing was counted while unlimited), so an envelope with more
	// events than the quota crosses it right away. 429 with Sentry's
	// rate-limit header, so the SDK stops sending (all categories) until
	// the next UTC day.
	if _, err := st.Pool.Exec(ctx, "UPDATE projects SET daily_quota = 1 WHERE id = $1", p.ID); err != nil {
		t.Fatal(err)
	}
	in.byKey = nil // the DSN-key cache holds the project for a minute
	over := envelope(crash("1.0", now, 2), crash("1.0", now, 3))
	req = newRequest("POST", fmt.Sprintf("/api/%d/envelope/", p.ID), over)
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
	p, err := store.CreateProject(ctx, st.Pool, "app", "App", nil, "k")
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
	p, err := store.CreateProject(ctx, st.Pool, "app", "App", nil, "k")
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
	iss, err := store.GetIssue(ctx, st.Pool, p.ID, res.NewIssues[0])
	if err != nil {
		t.Fatal(err)
	}
	if !iss.FirstSeen.Equal(late) {
		t.Fatalf("first_seen = %v, want the late (in-window) event %v", iss.FirstSeen, late)
	}
	// (The events' exact clamped time is TestIngestClampedTimesAreNow's.)
	var at time.Time
	if err := st.Pool.QueryRow(ctx, `SELECT started_at FROM sessions WHERE sid = 's1'`).Scan(&at); err != nil || !at.Equal(now.Truncate(time.Microsecond)) {
		t.Fatalf("session started_at = %v %v, want now %v", at, err, now)
	}
	if err := st.Pool.QueryRow(ctx, `SELECT first_seen FROM releases WHERE project_id = $1 AND release = '1.0'`, p.ID).Scan(&at); err != nil || !at.Equal(late) {
		t.Fatalf("releases.first_seen = %v %v, want %v", at, err, late)
	}
}

// TestIngestRegressionAlertsOnce: the envelope that flips a resolved issue
// to regression reports it (alert job, SSE); the ones after it — the
// issue keeps crashing — do not, or one issue would starve every other
// regression alert for the cooldown.
func TestIngestRegressionAlertsOnce(t *testing.T) {
	st := testdb.New(t)
	ctx := context.Background()
	p, err := store.CreateProject(ctx, st.Pool, "app", "App", nil, "k")
	if err != nil {
		t.Fatal(err)
	}
	in := &Ingester{Store: st, Cfg: config.Config{}, Log: slog.Default()}
	now := time.Now().UTC()
	ts := now.Format(time.RFC3339)
	res, err := in.Ingest(ctx, p, sentry.Parse(envelope(crash("1.0", ts, 1)), now), now)
	if err != nil || len(res.NewIssues) != 1 {
		t.Fatalf("first: %+v %v", res, err)
	}
	fp := res.NewIssues[0]
	if _, err := store.SetIssueStatus(ctx, st.Pool, store.SetIssueStatusParams{ProjectID: p.ID, Fingerprint: fp, Status: "resolved"}); err != nil {
		t.Fatal(err)
	}
	st.Pool.Exec(ctx, "DELETE FROM jobs")
	for i := 2; i <= 4; i++ {
		res, err := in.Ingest(ctx, p, sentry.Parse(envelope(crash("1.1", ts, i)), now), now)
		if err != nil {
			t.Fatal(err)
		}
		if want := i == 2; (len(res.Regressions) == 1) != want {
			t.Errorf("envelope %d: regressions=%v, want reported=%v", i, res.Regressions, want)
		}
	}
	var n int
	st.Pool.QueryRow(ctx, "SELECT count(*) FROM jobs WHERE kind = 'alert' AND args->>'type' = 'regression'").Scan(&n)
	if n != 1 {
		t.Fatalf("regression alert jobs = %d, want 1", n)
	}
}

// TestIngestHostileStrings: a \u0000 character (valid JSON, refused by
// Postgres) or a string past an index row's size must not fail the
// envelope — the SDK would resend it forever; and a field of the wrong
// JSON type is dropped, not the event.
func TestIngestHostileStrings(t *testing.T) {
	st := testdb.New(t)
	ctx := context.Background()
	p, err := store.CreateProject(ctx, st.Pool, "app", "App", nil, "k")
	if err != nil {
		t.Fatal(err)
	}
	in := &Ingester{Store: st, Cfg: config.Config{}, Log: slog.Default()}
	now := time.Now().UTC()
	long := strings.Repeat("x", 3000)
	body := `{"event_id":"aa","timestamp":"` + now.Format(time.RFC3339) + `","level":"error","platform":"android",
	 "release":"` + long + `","environment":"a\u0000b","transaction":"` + long + `","user":{"id":"` + long + `"},
	 "tags":{"k\u0000":"v\u0000","` + strings.Repeat("k", 40) + `":"dropped","ok":"` + long + `"},
	 "contexts":{"device":{"model":"m\u0000"},"os":{"version":"` + long + `"}},
	 "exception":{"values":[{"type":"E\u0000rr","value":"boom\u0000","stacktrace":{"frames":[{"filename":"F\u0000.java","function":"f","lineno":"12","in_app":true}]}}]}}`
	res, err := in.Ingest(ctx, p, sentry.Parse(envelope(body), now), now)
	if err != nil || res.Stored != 1 {
		t.Fatalf("hostile strings: %+v %v", res, err)
	}
	var release, env, transaction, user, model, osv, errType string
	var tags []byte
	if err = st.Pool.QueryRow(ctx, "SELECT release, environment, transaction, user_id, device_model, os_version, error_type, tags FROM events").Scan(&release, &env, &transaction, &user, &model, &osv, &errType, &tags); err != nil {
		t.Fatal(err)
	}
	if len(release) != 200 || env != "ab" || len(transaction) != 200 || len(user) != 128 || model != "m" || len(osv) != 200 || errType != "Err" {
		t.Errorf("bounds: release=%d env=%q transaction=%d user=%d model=%q os=%d type=%q", len(release), env, len(transaction), len(user), model, len(osv), errType)
	}
	s := string(tags)
	if !strings.Contains(s, `"k": "v"`) || strings.Contains(s, "dropped") || strings.Contains(s, strings.Repeat("x", 201)) {
		t.Errorf("tags = %s", s)
	}
	var title string
	if err := st.Pool.QueryRow(ctx, "SELECT title FROM issues").Scan(&title); err != nil || strings.Contains(title, "\x00") || title != "Err: boom" {
		t.Errorf("issue title = %q %v", title, err)
	}
}

// TestIngestSessionBounds: aggregate counts past int32 are clamped (the
// column is INTEGER: it wrapped negative), and a session started in the
// far future is taken as now (it would live in the default partition for
// good and hold a dirty hour forever).
func TestIngestSessionBounds(t *testing.T) {
	st := testdb.New(t)
	ctx := context.Background()
	p, err := store.CreateProject(ctx, st.Pool, "app", "App", nil, "k")
	if err != nil {
		t.Fatal(err)
	}
	in := &Ingester{Store: st, Cfg: config.Config{}, Log: slog.Default()}
	now := time.Now().UTC()
	sess := []byte(`{}` + "\n" + `{"type":"sessions"}` + "\n" +
		`{"attrs":{"release":"1.1"},"aggregates":[{"started":"2200-01-01T00:00:00Z","exited":3000000000}]}` + "\n")
	if res, err := in.Ingest(ctx, p, sentry.Parse(sess, now), now); err != nil || res.Sessions != 1 {
		t.Fatalf("sessions: %+v %v", res, err)
	}
	var count int64
	var started time.Time
	if err := st.Pool.QueryRow(ctx, "SELECT count, started_at FROM sessions").Scan(&count, &started); err != nil {
		t.Fatal(err)
	}
	if count != math.MaxInt32 || started.After(now.Add(time.Minute)) {
		t.Fatalf("count=%d started=%v", count, started)
	}
}

// TestIngestClampedResendDedupes: an event whose clock was far off gets
// the server's time; the SDK's resend gets a different one, and must
// still be recognized as the same event (not counted twice).
func TestIngestClampedResendDedupes(t *testing.T) {
	st := testdb.New(t)
	ctx := context.Background()
	p, err := store.CreateProject(ctx, st.Pool, "app", "App", nil, "k")
	if err != nil {
		t.Fatal(err)
	}
	in := &Ingester{Store: st, Cfg: config.Config{}, Log: slog.Default()}
	now := time.Now().UTC()
	future := now.Add(3 * time.Hour).Format(time.RFC3339)
	env := envelope(crash("1.0", future, 1))
	if res, err := in.Ingest(ctx, p, sentry.Parse(env, now), now); err != nil || res.Stored != 1 {
		t.Fatalf("first: %+v %v", res, err)
	}
	later := now.Add(2 * time.Minute)
	res, err := in.Ingest(ctx, p, sentry.Parse(env, later), later)
	if err != nil || res.Duplicates != 1 || res.Stored != 0 {
		t.Fatalf("resend: %+v %v", res, err)
	}
	var n int64
	st.Pool.QueryRow(ctx, "SELECT max(event_count) FROM issues").Scan(&n)
	if n != 1 {
		t.Fatalf("event_count = %d, want 1", n)
	}
}

// TestIngestSentrySemantics pins the Sentry rules ingest follows: an
// issue's level is its latest event's (not the worst ever seen), an
// exception without a mechanism is neither handled nor unhandled, and a
// session that reports errors > 0 without crashing is an errored session
// in release health.
func TestIngestSentrySemantics(t *testing.T) {
	st := testdb.New(t)
	ctx := context.Background()
	p, err := store.CreateProject(ctx, st.Pool, "app", "App", nil, "k")
	if err != nil {
		t.Fatal(err)
	}
	in := &Ingester{Store: st, Cfg: config.Config{}, Log: slog.Default()}
	now := time.Now().UTC()
	ev := func(id, level, ts string) string {
		return fmt.Sprintf(`{"event_id":%q,"timestamp":%q,"level":%q,"platform":"android","release":"1.0",
		 "exception":{"values":[{"type":"E","value":"x","stacktrace":{"frames":[{"filename":"A.java","function":"a","lineno":1,"in_app":true}]}}]}}`, id, ts, level)
	}
	older, newer := now.Add(-2*time.Hour).Format(time.RFC3339), now.Add(-time.Minute).Format(time.RFC3339)
	res, err := in.Ingest(ctx, p, sentry.Parse(envelope(ev(hexID("a"), "fatal", older)), now), now)
	if err != nil || len(res.NewIssues) != 1 {
		t.Fatalf("first: %+v %v", res, err)
	}
	fp := res.NewIssues[0]
	// A later event at warning: the issue shows warning. An even older
	// fatal one arriving afterwards does not change it back.
	for _, e := range []string{ev(hexID("b"), "warning", newer), ev(hexID("c"), "fatal", now.Add(-3*time.Hour).Format(time.RFC3339))} {
		if _, err := in.Ingest(ctx, p, sentry.Parse(envelope(e), now), now); err != nil {
			t.Fatal(err)
		}
	}
	issue, err := store.GetIssue(ctx, st.Pool, p.ID, fp)
	if err != nil || issue.Level != "warning" {
		t.Fatalf("issue level = %q (want the latest event's), err %v", issue.Level, err)
	}
	var handled *bool
	if err := st.Pool.QueryRow(ctx, "SELECT handled FROM events WHERE event_id = $1", sentry.DerivedID([]byte("a"))).Scan(&handled); err != nil || handled != nil {
		t.Fatalf("no mechanism: handled should be NULL, got %v (err %v)", handled, err)
	}
	var n int
	if err := st.Pool.QueryRow(ctx, "SELECT count(*) FROM events WHERE handled IS NULL").Scan(&n); err != nil || n != 3 {
		t.Fatalf("events without a mechanism: %d %v", n, err)
	}

	sess := []byte(`{}` + "\n" +
		`{"type":"session"}` + "\n" + `{"sid":"s1","status":"exited","errors":2,"started":"` + newer + `","attrs":{"release":"1.0"}}` + "\n" +
		`{"type":"session"}` + "\n" + `{"sid":"s2","status":"exited","errors":0,"started":"` + newer + `","attrs":{"release":"1.0"}}` + "\n" +
		`{"type":"session"}` + "\n" + `{"sid":"s3","status":"crashed","errors":1,"started":"` + newer + `","attrs":{"release":"1.0"}}` + "\n")
	if res, err := in.Ingest(ctx, p, sentry.Parse(sess, now), now); err != nil || res.Sessions != 3 {
		t.Fatalf("sessions: %+v %v", res, err)
	}
	var total, crashed, errored int64
	if err := st.Pool.QueryRow(ctx, "SELECT sum(total), sum(crashed), sum(errored) FROM release_health_hourly WHERE project_id = $1", p.ID).Scan(&total, &crashed, &errored); err != nil {
		t.Fatal(err)
	}
	if total != 3 || crashed != 1 || errored != 1 {
		t.Fatalf("release health total=%d crashed=%d errored=%d, want 3/1/1", total, crashed, errored)
	}
}

// TestIngestAttachments: attachments are kept with the envelope header's
// event when it is stored, and only then.
func TestIngestAttachments(t *testing.T) {
	st := testdb.New(t)
	ctx := context.Background()
	p, err := store.CreateProject(ctx, st.Pool, "app", "App", nil, "k2")
	if err != nil {
		t.Fatal(err)
	}
	in := &Ingester{Store: st, Cfg: config.Config{}, Log: slog.Default()}
	now := time.Now().UTC()
	ts := now.Add(-time.Minute).Format(time.RFC3339)
	event := func(id, typ string) string {
		return fmt.Sprintf(`{"event_id":%q,"timestamp":%q,"level":"error","platform":"android","release":"1.0","exception":{"values":[{"type":%q,"value":"v","stacktrace":{"frames":[{"filename":"A.java","function":"a","lineno":1,"in_app":true}]}}]}}`, id, ts, typ)
	}
	shot := "{\"type\":\"attachment\",\"length\":4,\"filename\":\"screenshot.png\",\"content_type\":\"image/png\"}\nPNG!\n"
	count := func() int {
		var n int
		if err := st.Pool.QueryRow(ctx, "SELECT count(*) FROM attachments WHERE project_id = $1", p.ID).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}

	id := "e1e1e1e1e1e1e1e1e1e1e1e1e1e1e1e1"
	body := "{\"event_id\":\"" + id + "\"}\n{\"type\":\"event\"}\n" + event(id, "E") + "\n" + shot +
		"{\"type\":\"attachment\",\"length\":2,\"filename\":\"view-hierarchy.json\",\"content_type\":\"application/json\",\"attachment_type\":\"event.view_hierarchy\"}\n{}\n"
	res, err := in.Ingest(ctx, p, sentry.Parse([]byte(body), now), now)
	if err != nil || res.Stored != 1 || res.Attachments != 2 {
		t.Fatalf("ingest: %+v %v", res, err)
	}
	e, err := store.GetEvent(ctx, st.Pool, p.ID, sentry.ID(id))
	if err != nil {
		t.Fatal(err)
	}
	atts, err := store.ListAttachments(ctx, st.Pool, p.ID, e.EventID, e.OccurredAt)
	if err != nil || len(atts) != 2 {
		t.Fatalf("attachments = %+v %v", atts, err)
	}
	if atts[0].N != 0 || atts[0].Filename != "screenshot.png" || atts[0].ContentType != "image/png" || atts[0].Size != 4 || atts[1].AttachmentType != "event.view_hierarchy" {
		t.Errorf("attachment rows = %+v", atts)
	}
	a, err := store.GetAttachment(ctx, st.Pool, p.ID, e.EventID, e.OccurredAt, 0)
	if err != nil || string(a.Data) != "PNG!" {
		t.Errorf("bytes = %q %v", a.Data, err)
	}
	// A resend stores nothing twice.
	if res, err = in.Ingest(ctx, p, sentry.Parse([]byte(body), now), now); err != nil || res.Duplicates != 1 || res.Attachments != 0 || count() != 2 {
		t.Fatalf("resend: %+v %v rows=%d", res, err, count())
	}
	// A sampled-out event takes its attachments with it.
	p.SampleKeepFirst, p.SampleRate = 1, 0
	id2 := "e2e2e2e2e2e2e2e2e2e2e2e2e2e2e2e2"
	body = "{\"event_id\":\"" + id2 + "\"}\n{\"type\":\"event\"}\n" + event(id2, "E") + "\n" + shot
	if res, err = in.Ingest(ctx, p, sentry.Parse([]byte(body), now), now); err != nil || res.Sampled != 1 || res.Attachments != 0 || count() != 2 {
		t.Fatalf("sampled out: %+v %v rows=%d", res, err, count())
	}
	// No header id: a lone event is the one.
	id3 := "e3e3e3e3e3e3e3e3e3e3e3e3e3e3e3e3"
	body = "{}\n{\"type\":\"event\"}\n" + event(id3, "F") + "\n" + shot
	if res, err = in.Ingest(ctx, p, sentry.Parse([]byte(body), now), now); err != nil || res.Stored != 1 || res.Attachments != 1 || count() != 3 {
		t.Fatalf("lone event: %+v %v rows=%d", res, err, count())
	}
	// Attachments without any event: nothing to attach to.
	body = "{\"event_id\":\"" + id3 + "\"}\n" + shot
	if res, err = in.Ingest(ctx, p, sentry.Parse([]byte(body), now), now); err != nil || res.Attachments != 0 || count() != 3 {
		t.Fatalf("orphan: %+v %v rows=%d", res, err, count())
	}
}
