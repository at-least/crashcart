package ingest

// Tests that pin the documented ingest claims (ARCHITECTURE.md /
// CLAUDE.md) that no other test could falsify: the whole-envelope quota
// rollback and what the process remembers of it, id derivation and
// in-envelope duplicates, sampling of handled events and of ungrouped
// events at rate 1, the dirty marks an envelope leaves, an ignored issue
// on a new event, the exact clamped time, unstored item types, the
// attachment partition, the redacted payload and the legacy /store/
// endpoint.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/at-least/crashcart/internal/config"
	"github.com/at-least/crashcart/internal/db/sqlc"
	"github.com/at-least/crashcart/internal/retention"
	"github.com/at-least/crashcart/internal/sentry"
	"github.com/at-least/crashcart/internal/store"
	"github.com/at-least/crashcart/internal/testdb"
)

func newProject(t *testing.T, st *store.Store, key string) sqlc.Project {
	t.Helper()
	p, err := st.CreateProject(context.Background(), sqlc.CreateProjectParams{Slug: "app-" + key, Name: "App", PublicKey: key})
	if err != nil {
		t.Fatal(err)
	}
	return p
}

// handled is a handled exception event (mechanism.handled = true), so
// sample_keep_first applies without UnhandledKeepFactor.
func handled(id, ts, typ string) string {
	return fmt.Sprintf(`{"event_id":%q,"timestamp":%q,"level":"error","platform":"android","release":"1.0",
	 "exception":{"values":[{"type":%q,"value":"v","mechanism":{"handled":true},"stacktrace":{"frames":[{"filename":"A.java","function":"a","lineno":1,"in_app":true}]}}]}}`, id, ts, typ)
}

// hexID is a valid 32-hex event id for a name (the SDK's ids are UUIDs;
// a short name would be replaced by one derived from the body).
func hexID(name string) string { return string(sentry.DerivedID([]byte(name))) }

func compact(item string) string {
	var c bytes.Buffer
	if err := json.Compact(&c, []byte(item)); err != nil {
		panic(err)
	}
	return c.String()
}

func count(t *testing.T, st *store.Store, q string, args ...any) int64 {
	t.Helper()
	var n int64
	if err := st.Pool.QueryRow(context.Background(), q, args...).Scan(&n); err != nil {
		t.Fatalf("%s: %v", q, err)
	}
	return n
}

// TestIngestQuotaRollbackIsWhole: the envelope that crosses the quota
// leaves nothing behind — no event, no issue, no dirty hour, no job, no
// usage — and the process then refuses the project's envelopes without
// touching the database, until the quota is changed or the UTC day ends.
func TestIngestQuotaRollbackIsWhole(t *testing.T) {
	st := testdb.New(t)
	ctx := context.Background()
	p := newProject(t, st, "q")
	in := &Ingester{Store: st, Cfg: config.Config{}, Log: slog.Default()}
	now := time.Now().UTC()
	ts := now.Add(-time.Minute).Format(time.RFC3339)
	day := now.Truncate(24 * time.Hour)
	nothing := func(step string) {
		t.Helper()
		for _, q := range []string{
			"SELECT count(*) FROM events WHERE project_id = $1",
			"SELECT count(*) FROM issues WHERE project_id = $1",
			"SELECT count(*) FROM event_stats_dirty WHERE project_id = $1",
			"SELECT count(*) FROM jobs WHERE project_id = $1",
			"SELECT count(*) FROM releases WHERE project_id = $1",
		} {
			if n := count(t, st, q, p.ID); n != 0 {
				t.Fatalf("%s: %s = %d, want 0", step, q, n)
			}
		}
		if used, _ := st.ProjectUsage(ctx, sqlc.ProjectUsageParams{ProjectID: p.ID, Day: day}); used != 0 {
			t.Fatalf("%s: usage = %d, want 0", step, used)
		}
	}

	p.DailyQuota = 1
	res, err := in.Ingest(ctx, p, sentry.Parse(envelope(handled("a1", ts, "E"), handled("a2", ts, "E")), now), now)
	if err != ErrQuota || res.Stored != 0 || res.Received != 2 {
		t.Fatalf("two events into a quota of one: res=%+v err=%v", res, err)
	}
	nothing("after rollback")
	// One event would fit the quota by the database's count (0 so far),
	// but the process remembers the exhaustion and refuses before any work.
	if _, err := in.Ingest(ctx, p, sentry.Parse(envelope(handled("a3", ts, "E")), now), now); err != ErrQuota {
		t.Fatalf("remembered exhaustion: err=%v", err)
	}
	nothing("after short-circuit")
	// A changed quota forgets it: the same single event is accepted.
	p.DailyQuota = 2
	if res, err := in.Ingest(ctx, p, sentry.Parse(envelope(handled("a3", ts, "E")), now), now); err != nil || res.Stored != 1 {
		t.Fatalf("after quota change: res=%+v err=%v", res, err)
	}
	// Exhaust the new quota (1 counted + 2 > 2), then the next UTC day is
	// a fresh count: the process no longer refuses.
	if _, err := in.Ingest(ctx, p, sentry.Parse(envelope(handled("a4", ts, "E"), handled("a5", ts, "E")), now), now); err != ErrQuota {
		t.Fatalf("second exhaustion: err=%v", err)
	}
	if _, err := in.Ingest(ctx, p, sentry.Parse(envelope(handled("a6", ts, "E")), now), now); err != ErrQuota {
		t.Fatalf("still exhausted today: err=%v", err)
	}
	tomorrow := nextUTCDay(now).Add(time.Second)
	if res, err := in.Ingest(ctx, p, sentry.Parse(envelope(handled("a6", ts, "E")), tomorrow), tomorrow); err != nil || res.Stored != 1 {
		t.Fatalf("next UTC day: res=%+v err=%v", res, err)
	}
	if used, _ := st.ProjectUsage(ctx, sqlc.ProjectUsageParams{ProjectID: p.ID, Day: day}); used != 1 {
		t.Fatalf("today's usage = %d, want 1 (only the accepted event)", used)
	}
}

// TestIngestDerivedIDAndInEnvelopeDuplicates: an event with no usable id
// (neither its own nor the header's) gets one derived from its body, so
// the same body twice is one event; a different body is another. The
// same id twice in one envelope is counted once.
func TestIngestDerivedIDAndInEnvelopeDuplicates(t *testing.T) {
	st := testdb.New(t)
	ctx := context.Background()
	p := newProject(t, st, "d")
	in := &Ingester{Store: st, Cfg: config.Config{}, Log: slog.Default()}
	now := time.Now().UTC()
	ts := now.Add(-time.Minute).Format(time.RFC3339)
	noID := func(msg string) []byte {
		body := fmt.Sprintf(`{"timestamp":%q,"level":"error","platform":"android","message":%q,"exception":{"values":[{"type":"E","value":"v","stacktrace":{"frames":[{"filename":"A.java","function":"a","lineno":1,"in_app":true}]}}]}}`, ts, msg)
		return []byte("{}\n{\"type\":\"event\"}\n" + body + "\n")
	}
	if res, err := in.Ingest(ctx, p, sentry.Parse(noID("one"), now), now); err != nil || res.Stored != 1 {
		t.Fatalf("first: %+v %v", res, err)
	}
	if res, err := in.Ingest(ctx, p, sentry.Parse(noID("one"), now), now); err != nil || res.Duplicates != 1 || res.Stored != 0 {
		t.Fatalf("same body again: %+v %v", res, err)
	}
	if res, err := in.Ingest(ctx, p, sentry.Parse(noID("two"), now), now); err != nil || res.Stored != 1 || res.Duplicates != 0 {
		t.Fatalf("different body: %+v %v", res, err)
	}
	if n := count(t, st, "SELECT count(*) FROM events WHERE project_id = $1", p.ID); n != 2 {
		t.Fatalf("events = %d, want 2", n)
	}
	if n := count(t, st, "SELECT sum(event_count) FROM issues WHERE project_id = $1", p.ID); n != 2 {
		t.Fatalf("event_count = %d, want 2 (the resend not counted)", n)
	}
	want := sentry.DerivedID([]byte(strings.TrimSpace(strings.SplitN(string(noID("one")), "\n", 3)[2])))
	if n := count(t, st, "SELECT count(*) FROM events WHERE project_id = $1 AND event_id = $2", p.ID, want); n != 1 {
		t.Fatalf("derived id %s not the stored one", want)
	}

	// The same event twice in one envelope.
	res, err := in.Ingest(ctx, p, sentry.Parse(envelope(handled("b1", ts, "F"), handled("b1", ts, "F")), now), now)
	if err != nil || res.Received != 2 || res.Duplicates != 1 || res.Stored != 1 {
		t.Fatalf("twice in one envelope: %+v %v", res, err)
	}
	iss, err := st.GetIssue(ctx, sqlc.GetIssueParams{ProjectID: p.ID, Fingerprint: res.NewIssues[0]})
	if err != nil || iss.EventCount != 1 || iss.StoredCount != 1 {
		t.Fatalf("issue counts = %d/%d %v, want 1/1", iss.EventCount, iss.StoredCount, err)
	}
}

// TestIngestSamplingHandledAndRateOne: handled events keep exactly
// sample_keep_first per issue (no factor) across envelopes; sampled-out
// events count in event_count but not stored_count; at sample_rate 1
// everything is stored again; an ungrouped event at rate 1 is stored.
func TestIngestSamplingHandledAndRateOne(t *testing.T) {
	st := testdb.New(t)
	ctx := context.Background()
	p := newProject(t, st, "s")
	p.SampleKeepFirst, p.SampleRate = 2, 0
	in := &Ingester{Store: st, Cfg: config.Config{}, Log: slog.Default()}
	now := time.Now().UTC()
	ts := now.Add(-time.Minute).Format(time.RFC3339)
	var fp sentry.ID
	for i := 1; i <= 5; i++ {
		res, err := in.Ingest(ctx, p, sentry.Parse(envelope(handled(fmt.Sprintf("h%d", i), ts, "E")), now), now)
		if err != nil {
			t.Fatal(err)
		}
		if i == 1 {
			fp = res.NewIssues[0]
		}
		if wantStored := i <= 2; (res.Stored == 1) != wantStored || res.Sampled != 1-res.Stored {
			t.Fatalf("event %d: %+v, want stored=%v", i, res, wantStored)
		}
	}
	iss, _ := st.GetIssue(ctx, sqlc.GetIssueParams{ProjectID: p.ID, Fingerprint: fp})
	if iss.EventCount != 5 || iss.StoredCount != 2 {
		t.Fatalf("counts = %d/%d, want 5/2", iss.EventCount, iss.StoredCount)
	}
	if n := count(t, st, "SELECT count(*) FROM events WHERE project_id = $1 AND fingerprint = $2", p.ID, fp); n != 2 {
		t.Fatalf("stored rows = %d, want 2", n)
	}
	p.SampleRate = 1
	if res, err := in.Ingest(ctx, p, sentry.Parse(envelope(handled("h6", ts, "E"), handled("h7", ts, "E")), now), now); err != nil || res.Stored != 2 || res.Sampled != 0 {
		t.Fatalf("rate 1: %+v %v", res, err)
	}
	iss, _ = st.GetIssue(ctx, sqlc.GetIssueParams{ProjectID: p.ID, Fingerprint: fp})
	if iss.EventCount != 7 || iss.StoredCount != 4 {
		t.Fatalf("counts after rate 1 = %d/%d, want 7/4", iss.EventCount, iss.StoredCount)
	}
	info := fmt.Sprintf(`{"event_id":"i1","timestamp":%q,"level":"info","platform":"android","message":"opened cart"}`, ts)
	res, err := in.Ingest(ctx, p, sentry.Parse(envelope(info), now), now)
	if err != nil || res.Stored != 1 || len(res.NewIssues) != 0 {
		t.Fatalf("ungrouped at rate 1: %+v %v", res, err)
	}
	if n := count(t, st, "SELECT count(*) FROM events WHERE project_id = $1 AND fingerprint IS NULL", p.ID); n != 1 {
		t.Fatalf("ungrouped rows = %d, want 1", n)
	}
}

// TestIngestDirtyMarksPerHour: one envelope touching two hours leaves
// exactly those two (project, hour) keys in event_stats_dirty, and its
// session marks session_stats_dirty for its own hour.
func TestIngestDirtyMarksPerHour(t *testing.T) {
	st := testdb.New(t)
	ctx := context.Background()
	p := newProject(t, st, "m")
	in := &Ingester{Store: st, Cfg: config.Config{}, Log: slog.Default()}
	now := time.Now().UTC()
	h1 := now.Add(-3 * time.Hour).Truncate(time.Hour)
	h2 := now.Add(-2 * time.Hour).Truncate(time.Hour)
	sessionHour := now.Add(-5 * time.Hour).Truncate(time.Hour)
	body := envelope(handled("x1", h1.Add(5*time.Minute).Format(time.RFC3339), "E"), handled("x2", h2.Add(7*time.Minute).Format(time.RFC3339), "E"), handled("x3", h2.Add(9*time.Minute).Format(time.RFC3339), "E"))
	env := sentry.Parse(body, now)
	env.Sessions = append(env.Sessions, sentry.Session{SID: "s1", Release: "1.0", Status: "ok", StartedAt: sessionHour.Add(time.Minute), Count: 1})
	if res, err := in.Ingest(ctx, p, env, now); err != nil || res.Stored != 3 || res.Sessions != 1 {
		t.Fatalf("res=%+v err=%v", res, err)
	}
	rows, err := st.Pool.Query(ctx, "SELECT bucket FROM event_stats_dirty WHERE project_id = $1 ORDER BY bucket", p.ID)
	if err != nil {
		t.Fatal(err)
	}
	var got []time.Time
	for rows.Next() {
		var b time.Time
		rows.Scan(&b)
		got = append(got, b.UTC())
	}
	rows.Close()
	if len(got) != 2 || !got[0].Equal(h1) || !got[1].Equal(h2) {
		t.Fatalf("event dirty hours = %v, want [%v %v]", got, h1, h2)
	}
	var sb time.Time
	if err := st.Pool.QueryRow(ctx, "SELECT bucket FROM session_stats_dirty WHERE project_id = $1", p.ID).Scan(&sb); err != nil || !sb.Equal(sessionHour) {
		t.Fatalf("session dirty hour = %v %v, want %v", sb, err, sessionHour)
	}
}

// TestIngestIgnoredIssueStaysIgnored: only alerts.CheckIgnored lifts an
// ignore — a new event on a new release leaves the status alone (no
// regression, no alert job), while the counts move.
func TestIngestIgnoredIssueStaysIgnored(t *testing.T) {
	st := testdb.New(t)
	ctx := context.Background()
	p := newProject(t, st, "i")
	in := &Ingester{Store: st, Cfg: config.Config{}, Log: slog.Default()}
	now := time.Now().UTC()
	ts := now.Add(-time.Minute).Format(time.RFC3339)
	res, err := in.Ingest(ctx, p, sentry.Parse(envelope(crash("1.0", ts, 1)), now), now)
	if err != nil || len(res.NewIssues) != 1 {
		t.Fatalf("first: %+v %v", res, err)
	}
	fp := res.NewIssues[0]
	if _, err := st.SetIssueStatus(ctx, sqlc.SetIssueStatusParams{ProjectID: p.ID, Fingerprint: fp, Status: "ignored"}); err != nil {
		t.Fatal(err)
	}
	st.Pool.Exec(ctx, "DELETE FROM jobs")
	res, err = in.Ingest(ctx, p, sentry.Parse(envelope(crash("2.0", ts, 2)), now), now)
	if err != nil || len(res.Regressions) != 0 || len(res.NewIssues) != 0 {
		t.Fatalf("event on an ignored issue: %+v %v", res, err)
	}
	iss, _ := st.GetIssue(ctx, sqlc.GetIssueParams{ProjectID: p.ID, Fingerprint: fp})
	if iss.Status != "ignored" || iss.EventCount != 2 || *iss.LastRelease != "2.0" {
		t.Fatalf("issue = status %s count %d last %v", iss.Status, iss.EventCount, iss.LastRelease)
	}
	if n := count(t, st, "SELECT count(*) FROM jobs WHERE kind = 'alert'"); n != 0 {
		t.Fatalf("alert jobs = %d, want 0", n)
	}
}

// TestIngestClampedTimesAreNow: a clock more than a minute ahead, or
// before the retention window, is replaced by the server's time exactly
// (microseconds); one within the bounds is kept.
func TestIngestClampedTimesAreNow(t *testing.T) {
	st := testdb.New(t)
	ctx := context.Background()
	p := newProject(t, st, "c")
	in := &Ingester{Store: st, Cfg: config.Config{RetentionDays: 30}, Log: slog.Default()}
	now := time.Now().UTC().Truncate(time.Microsecond)
	ahead := now.Add(30 * time.Second).Truncate(time.Second)
	env := sentry.Parse(envelope(
		handled(hexID("f1"), now.Add(3*time.Hour).Format(time.RFC3339), "E"),
		handled(hexID("f2"), ahead.Format(time.RFC3339), "E"),
		handled(hexID("f3"), now.Add(-31*24*time.Hour).Format(time.RFC3339), "E"),
	), now)
	if res, err := in.Ingest(ctx, p, env, now); err != nil || res.Stored != 3 {
		t.Fatalf("res=%+v err=%v", res, err)
	}
	at := func(name string) time.Time {
		var t0 time.Time
		if err := st.Pool.QueryRow(ctx, "SELECT occurred_at FROM events WHERE project_id = $1 AND event_id = $2", p.ID, sentry.DerivedID([]byte(name))).Scan(&t0); err != nil {
			t.Fatal(name, err)
		}
		return t0.UTC()
	}
	if got := at("f1"); !got.Equal(now) {
		t.Errorf("future clock: occurred_at = %v, want now %v", got, now)
	}
	if got := at("f2"); !got.Equal(ahead) {
		t.Errorf("30 s ahead: occurred_at = %v, want kept %v", got, ahead)
	}
	if got := at("f3"); !got.Equal(now) {
		t.Errorf("past the window: occurred_at = %v, want now %v", got, now)
	}
}

// TestIngestDropsUnstoredItemTypes: transactions, profiles and replays
// are accepted (200) and leave no row. A client_report with no
// discarded_events is accepted too and leaves no count row (nothing to
// add) — it is parsed, not in the unstored-types drop list, but an empty
// report has nothing worth storing either.
func TestIngestDropsUnstoredItemTypes(t *testing.T) {
	st := testdb.New(t)
	p := newProject(t, st, "drop")
	in := &Ingester{Store: st, Cfg: config.Config{}, Log: slog.Default()}
	body := []byte("{}\n" +
		`{"type":"transaction"}` + "\n" + `{"event_id":"t1","type":"transaction","transaction":"/x","timestamp":1787998530}` + "\n" +
		`{"type":"profile"}` + "\n" + `{"profile":1}` + "\n" +
		`{"type":"replay_event"}` + "\n" + `{"replay":1}` + "\n" +
		`{"type":"client_report"}` + "\n" + `{"discarded_events":[]}` + "\n")
	env := sentry.Parse(body, time.Now())
	if env.Dropped != 3 || len(env.Events) != 0 || len(env.Sessions) != 0 || len(env.ClientReportCounts) != 0 {
		t.Fatalf("parsed: dropped=%d events=%d sessions=%d counts=%d", env.Dropped, len(env.Events), len(env.Sessions), len(env.ClientReportCounts))
	}
	req := newRequest("POST", fmt.Sprintf("/api/%d/envelope/", p.ID), body)
	req.Header.Set("X-Sentry-Auth", "Sentry sentry_key=drop")
	rec := newRecorder()
	in.Handler().ServeHTTP(rec, req)
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), `"received":0`) {
		t.Fatalf("→ %d %s", rec.Code, rec.Body.String())
	}
	for _, tbl := range []string{"events", "sessions", "issues", "event_stats_dirty", "project_usage", "client_report_counts"} {
		if n := count(t, st, "SELECT count(*) FROM "+tbl+" WHERE project_id = $1", p.ID); n != 0 {
			t.Errorf("%s rows = %d, want 0", tbl, n)
		}
	}
}

// TestIngestAttachmentPartitionFollowsEvent: an attachment row lives in
// the attachments partition of its event's week, keyed by the event's
// occurred_at — so retention drops them together.
func TestIngestAttachmentPartitionFollowsEvent(t *testing.T) {
	st := testdb.New(t)
	ctx := context.Background()
	p := newProject(t, st, "att")
	cfg := config.Config{RetentionDays: 30}
	now := time.Now().UTC()
	if err := retention.EnsurePartitions(ctx, st, cfg, now); err != nil {
		t.Fatal(err)
	}
	in := &Ingester{Store: st, Cfg: cfg, Log: slog.Default()}
	at := now.Add(-9 * 24 * time.Hour).Truncate(time.Second) // last week's partition, not this week's
	id := "abababababababababababababababab"
	body := "{\"event_id\":\"" + id + "\"}\n{\"type\":\"event\"}\n" + compact(handled(id, at.Format(time.RFC3339), "E")) + "\n" +
		"{\"type\":\"attachment\",\"length\":3,\"filename\":\"s.png\",\"content_type\":\"image/png\"}\nPNG\n"
	if res, err := in.Ingest(ctx, p, sentry.Parse([]byte(body), now), now); err != nil || res.Stored != 1 || res.Attachments != 1 {
		t.Fatalf("res=%+v err=%v", res, err)
	}
	var evPart, attPart string
	var attAt time.Time
	if err := st.Pool.QueryRow(ctx, "SELECT tableoid::regclass::text FROM events WHERE project_id = $1", p.ID).Scan(&evPart); err != nil {
		t.Fatal(err)
	}
	if err := st.Pool.QueryRow(ctx, "SELECT tableoid::regclass::text, occurred_at FROM attachments WHERE project_id = $1", p.ID).Scan(&attPart, &attAt); err != nil {
		t.Fatal(err)
	}
	evWeek, ok1 := strings.CutPrefix(evPart, "events_p")
	attWeek, ok2 := strings.CutPrefix(attPart, "attachments_p")
	if !ok1 || !ok2 || evWeek != attWeek {
		t.Fatalf("event in %q, attachment in %q: want the same week's real partitions", evPart, attPart)
	}
	if !attAt.Equal(at) {
		t.Fatalf("attachment occurred_at = %v, want the event's %v", attAt, at)
	}
	var week time.Time
	if week, _ = time.Parse("20060102", evWeek); week.Weekday() != time.Monday || at.Before(week) || !at.Before(week.Add(7*24*time.Hour)) {
		t.Fatalf("partition %s does not hold %v", evPart, at)
	}
}

// TestIngestRedactsStoredPayload: with PII_REDACT the columns, the tags,
// the issue title and the stored payload (as store.Payload returns it)
// carry none of the raw email / IP / user id.
func TestIngestRedactsStoredPayload(t *testing.T) {
	st := testdb.New(t)
	ctx := context.Background()
	p := newProject(t, st, "pii")
	in := &Ingester{Store: st, Cfg: config.Config{PIIRedact: true}, Log: slog.Default()}
	now := time.Now().UTC()
	body := fmt.Sprintf(`{"event_id":%q,"timestamp":%q,"level":"error","platform":"android","release":"1.0",
	 "message":"charge failed for carol@example.com","transaction":"/users/bob@example.com/cart",
	 "user":{"id":"customer-00012345","email":"dave@example.com","ip_address":"203.0.113.9"},
	 "tags":{"email":"erin@example.com","note":"call 555-123-4567","build":"42"},
	 "exception":{"values":[{"type":"PaymentError","value":"declined for frank@example.com","stacktrace":{"frames":[{"filename":"Pay.java","function":"charge","lineno":3,"in_app":true}]}}]}}`,
		hexID("p1"), now.Add(-time.Minute).Format(time.RFC3339))
	res, err := in.Ingest(ctx, p, sentry.Parse(envelope(body), now), now)
	if err != nil || res.Stored != 1 {
		t.Fatalf("res=%+v err=%v", res, err)
	}
	e, err := st.GetEvent(ctx, sqlc.GetEventParams{ProjectID: p.ID, EventID: sentry.DerivedID([]byte("p1"))})
	if err != nil {
		t.Fatal(err)
	}
	raw, err := store.Payload(e)
	if err != nil || len(raw) == 0 {
		t.Fatalf("payload: %d bytes %v", len(raw), err)
	}
	iss, _ := st.GetIssue(ctx, sqlc.GetIssueParams{ProjectID: p.ID, Fingerprint: *e.Fingerprint})
	var tags map[string]string
	json.Unmarshal(e.Tags, &tags)
	for name, text := range map[string]string{"payload": string(raw), "message": e.Message, "transaction": *e.Transaction, "user_id": *e.UserID, "title": iss.Title, "tags": string(e.Tags)} {
		for _, secret := range []string{"@example.com", "203.0.113.9", "customer-00012345", "555-123-4567"} {
			if strings.Contains(text, secret) {
				t.Errorf("%s still carries %q: %s", name, secret, text)
			}
		}
	}
	if *e.UserID != RedactUserID("customer-00012345") || tags["email"] != "[REDACTED]" || tags["build"] != "42" {
		t.Errorf("user_id=%q tags=%v", *e.UserID, tags)
	}
	var js map[string]any
	if err := json.Unmarshal(raw, &js); err != nil {
		t.Errorf("stored payload is not JSON: %v", err)
	}
}

// TestStoreEndpointMatchesEnvelope: the legacy /store/ body (a bare
// event) lands as the same issue — fingerprint, title, culprit — as the
// event sent in an envelope.
func TestStoreEndpointMatchesEnvelope(t *testing.T) {
	st := testdb.New(t)
	ctx := context.Background()
	p := newProject(t, st, "legacy")
	in := &Ingester{Store: st, Cfg: config.Config{}, Log: slog.Default()}
	h := in.Handler()
	ts := time.Now().UTC().Add(-time.Minute).Format(time.RFC3339)
	post := func(path string, body []byte) {
		t.Helper()
		req := newRequest("POST", path, body)
		req.Header.Set("X-Sentry-Auth", "Sentry sentry_key=legacy")
		rec := newRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != 200 || !strings.Contains(rec.Body.String(), `"stored":1`) {
			t.Fatalf("%s → %d %s", path, rec.Code, rec.Body.String())
		}
	}
	post(fmt.Sprintf("/api/%d/envelope/", p.ID), envelope(crash("1.0", ts, 1)))
	post(fmt.Sprintf("/api/%d/store/", p.ID), []byte(crash("1.0", ts, 2)))
	rows, err := st.Pool.Query(ctx, "SELECT fingerprint::text, culprit, error_type FROM events WHERE project_id = $1 ORDER BY event_id", p.ID)
	if err != nil {
		t.Fatal(err)
	}
	var fps, culprits []string
	for rows.Next() {
		var fp, culprit, typ string
		if err := rows.Scan(&fp, &culprit, &typ); err != nil {
			t.Fatal(err)
		}
		fps, culprits = append(fps, fp), append(culprits, culprit)
	}
	rows.Close()
	if len(fps) != 2 || fps[0] != fps[1] || culprits[0] != culprits[1] || culprits[0] != "CartFragment.java in load" {
		t.Fatalf("fingerprints %v culprits %v", fps, culprits)
	}
	if n := count(t, st, "SELECT count(*) FROM issues WHERE project_id = $1 AND event_count = 2 AND title = 'NullPointerException: boom'", p.ID); n != 1 {
		t.Fatalf("one issue with both events: %d", n)
	}
}

// userReportBody builds a user_report envelope item.
func userReportBody(eventID, comments string) string {
	return fmt.Sprintf(`{"event_id":%q,"name":"Alex","email":"alex@example.com","comments":%q}`, eventID, comments)
}

// TestIngestUserReportKeptRegardlessOfEvent: a user_report is stored even
// when it arrives alone (no event item at all — the usual case, sent
// after the app restarts) or when its event was sampled out; a resend
// overwrites the same row rather than duplicating it.
func TestIngestUserReportKeptRegardlessOfEvent(t *testing.T) {
	st := testdb.New(t)
	ctx := context.Background()
	p := newProject(t, st, "ur1")
	in := &Ingester{Store: st, Cfg: config.Config{}, Log: slog.Default()}
	now := time.Now().UTC()

	// A report-only envelope: no event item, and this event was never
	// ingested at all. It is still stored.
	id := hexID("ur-event-1")
	body := "{\"event_id\":\"" + id + "\"}\n{\"type\":\"user_report\"}\n" + userReportBody(id, "it crashed") + "\n"
	res, err := in.Ingest(ctx, p, sentry.Parse([]byte(body), now), now)
	if err != nil || res.UserReports != 1 || res.Stored != 0 {
		t.Fatalf("report-only envelope: %+v %v", res, err)
	}
	ur, err := st.GetUserReport(ctx, sqlc.GetUserReportParams{ProjectID: p.ID, EventID: sentry.ID(id)})
	if err != nil || ur.Comments != "it crashed" || ur.Name == nil || *ur.Name != "Alex" || ur.Email == nil || *ur.Email != "alex@example.com" {
		t.Fatalf("stored report = %+v %v", ur, err)
	}

	// A resend overwrites the same row, not a duplicate.
	body = "{\"event_id\":\"" + id + "\"}\n{\"type\":\"user_report\"}\n" + userReportBody(id, "it crashed again") + "\n"
	if res, err = in.Ingest(ctx, p, sentry.Parse([]byte(body), now), now); err != nil || res.UserReports != 1 {
		t.Fatalf("resend: %+v %v", res, err)
	}
	if n := count(t, st, "SELECT count(*) FROM user_reports WHERE project_id = $1", p.ID); n != 1 {
		t.Fatalf("resend duplicated: %d rows", n)
	}
	if ur, err = st.GetUserReport(ctx, sqlc.GetUserReportParams{ProjectID: p.ID, EventID: sentry.ID(id)}); err != nil || ur.Comments != "it crashed again" {
		t.Fatalf("resend did not overwrite: %+v %v", ur, err)
	}

	// An event that per-issue sampling drops: the report on it survives.
	p.SampleKeepFirst, p.SampleRate = 0, 0
	id2 := hexID("ur-event-2")
	ts := now.Add(-time.Minute).Format(time.RFC3339)
	ev := fmt.Sprintf(`{"event_id":%q,"timestamp":%q,"level":"error","platform":"android","exception":{"values":[{"type":"E","value":"v","stacktrace":{"frames":[{"filename":"A.java","function":"a","lineno":1,"in_app":true}]}}]}}`, id2, ts)
	body = "{\"event_id\":\"" + id2 + "\"}\n{\"type\":\"event\"}\n" + ev + "\n{\"type\":\"user_report\"}\n" + userReportBody(id2, "sampled but keep the feedback") + "\n"
	if res, err = in.Ingest(ctx, p, sentry.Parse([]byte(body), now), now); err != nil || res.Sampled != 1 || res.Stored != 0 || res.UserReports != 1 {
		t.Fatalf("sampled-out event: %+v %v", res, err)
	}
	if ur, err = st.GetUserReport(ctx, sqlc.GetUserReportParams{ProjectID: p.ID, EventID: sentry.ID(id2)}); err != nil || ur.Comments != "sampled but keep the feedback" {
		t.Fatalf("sampled-out report = %+v %v", ur, err)
	}
}

// TestIngestUserReportPIIRedact: PII_REDACT nulls name/email and scrubs
// comments, the same policy the rest of ingest applies — the user's own
// free-text input is not exempt from an operator's redaction setting.
func TestIngestUserReportPIIRedact(t *testing.T) {
	st := testdb.New(t)
	ctx := context.Background()
	p := newProject(t, st, "ur-pii")
	in := &Ingester{Store: st, Cfg: config.Config{PIIRedact: true}, Log: slog.Default()}
	now := time.Now().UTC()
	id := hexID("ur-pii-event")
	body := "{\"event_id\":\"" + id + "\"}\n{\"type\":\"user_report\"}\n" +
		userReportBody(id, "contact me at dana@example.com about this") + "\n"
	res, err := in.Ingest(ctx, p, sentry.Parse([]byte(body), now), now)
	if err != nil || res.UserReports != 1 {
		t.Fatalf("ingest: %+v %v", res, err)
	}
	ur, err := st.GetUserReport(ctx, sqlc.GetUserReportParams{ProjectID: p.ID, EventID: sentry.ID(id)})
	if err != nil {
		t.Fatal(err)
	}
	if ur.Name != nil || ur.Email != nil {
		t.Errorf("name/email not nulled: name=%v email=%v", ur.Name, ur.Email)
	}
	if strings.Contains(ur.Comments, "@example.com") {
		t.Errorf("comments not redacted: %q", ur.Comments)
	}
}

// TestIngestClientReportCountsAccumulate: two separate envelopes each
// reporting the same (reason, category) add up into one bucket rather
// than overwriting it, and ingesting a client_report-only envelope does
// not touch the daily event quota.
func TestIngestClientReportCountsAccumulate(t *testing.T) {
	st := testdb.New(t)
	ctx := context.Background()
	p := newProject(t, st, "cr1")
	p.DailyQuota = 1 // any event quota consumption here would show up as an exhausted quota within this test
	in := &Ingester{Store: st, Cfg: config.Config{}, Log: slog.Default()}
	now := time.Now().UTC()

	body := "{}\n{\"type\":\"client_report\"}\n" +
		`{"discarded_events":[{"reason":"sample_rate","category":"error","quantity":3},{"reason":"before_send","category":"error","quantity":1}]}` + "\n"
	res, err := in.Ingest(ctx, p, sentry.Parse([]byte(body), now), now)
	if err != nil || res.ClientReportCounts != 2 {
		t.Fatalf("first envelope: %+v %v", res, err)
	}

	body = "{}\n{\"type\":\"client_report\"}\n" +
		`{"discarded_events":[{"reason":"sample_rate","category":"error","quantity":2}]}` + "\n"
	if res, err = in.Ingest(ctx, p, sentry.Parse([]byte(body), now), now); err != nil || res.ClientReportCounts != 1 {
		t.Fatalf("second envelope: %+v %v", res, err)
	}

	rows, err := st.ListClientReportCounts(ctx, sqlc.ListClientReportCountsParams{
		ProjectID: p.ID, Bucket: now.Add(-time.Hour), Bucket_2: now.Add(time.Hour),
	})
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]int64{}
	for _, r := range rows {
		got[r.Reason+"/"+r.Category] = r.Quantity
	}
	if got["sample_rate/error"] != 5 || got["before_send/error"] != 1 {
		t.Fatalf("counts did not accumulate: %+v", got)
	}

	// Neither envelope had an event, so nothing was counted against the
	// quota — a third envelope with an actual event still fits under 1.
	id := hexID("cr-event-1")
	ev := fmt.Sprintf(`{"event_id":%q,"timestamp":%q,"level":"error","platform":"android","exception":{"values":[{"type":"E","value":"v","stacktrace":{"frames":[{"filename":"A.java","function":"a","lineno":1,"in_app":true}]}}]}}`, id, now.Format(time.RFC3339))
	body = "{\"event_id\":\"" + id + "\"}\n{\"type\":\"event\"}\n" + ev + "\n"
	if res, err = in.Ingest(ctx, p, sentry.Parse([]byte(body), now), now); err != nil || res.Stored != 1 {
		t.Fatalf("quota was consumed by client_report-only envelopes: %+v %v", res, err)
	}
}

func checkInBody(checkInID, slug, status, monitorConfig string) string {
	b := fmt.Sprintf(`{"check_in_id":%q,"monitor_slug":%q,"status":%q`, checkInID, slug, status)
	if monitorConfig != "" {
		b += `,"monitor_config":` + monitorConfig
	}
	return b + "}"
}

const zeroCheckInID = "00000000000000000000000000000000"

// TestIngestCheckInMonitorConfigUpsert: monitor_config on the run's first
// (in_progress) check-in upserts the monitor's schedule, and the
// in_progress check-in itself does not advance the monitor's state
// (last_status, next_expected_at) — only a terminal one does.
func TestIngestCheckInMonitorConfigUpsert(t *testing.T) {
	st := testdb.New(t)
	ctx := context.Background()
	p := newProject(t, st, "ci1")
	in := &Ingester{Store: st, Cfg: config.Config{}, Log: slog.Default()}
	now := time.Now().UTC()

	cfg := `{"schedule":{"type":"crontab","value":"0 * * * *"},"checkin_margin":5,"max_runtime":15,"failure_issue_threshold":2,"recovery_threshold":1,"timezone":"UTC"}`
	body := "{}\n{\"type\":\"check_in\"}\n" + checkInBody(hexID("run-1"), "nightly-backup", "in_progress", cfg) + "\n"
	res, err := in.Ingest(ctx, p, sentry.Parse([]byte(body), now), now)
	if err != nil || res.Monitors != 1 || res.CheckIns != 1 {
		t.Fatalf("ingest: %+v %v", res, err)
	}
	m, err := st.GetMonitor(ctx, sqlc.GetMonitorParams{ProjectID: p.ID, Slug: "nightly-backup"})
	if err != nil {
		t.Fatal(err)
	}
	if m.ScheduleType != "crontab" || m.ScheduleValue != "0 * * * *" || m.CheckinMarginMin != 5 || m.MaxRuntimeMin != 15 ||
		m.FailureThreshold != 2 || m.RecoveryThreshold != 1 {
		t.Fatalf("monitor config = %+v", m)
	}
	if m.NextExpectedAt != nil || m.LastCheckinAt != nil || m.LastStatus.Valid {
		t.Fatalf("in_progress advanced monitor state: %+v", m)
	}
	rows, err := st.ListCheckIns(ctx, sqlc.ListCheckInsParams{ProjectID: p.ID, MonitorSlug: "nightly-backup", Limit: 10})
	if err != nil || len(rows) != 1 || rows[0].Status != "in_progress" {
		t.Fatalf("check-in row = %+v %v", rows, err)
	}
}

// TestIngestCheckInZeroIDCompletesLatestInProgress: the all-zero
// check_in_id shorthand updates the monitor's latest in_progress row
// rather than creating a new one, and a terminal status advances the
// monitor's state (last_status, next_expected_at, consecutive_successes).
func TestIngestCheckInZeroIDCompletesLatestInProgress(t *testing.T) {
	st := testdb.New(t)
	ctx := context.Background()
	p := newProject(t, st, "ci2")
	in := &Ingester{Store: st, Cfg: config.Config{}, Log: slog.Default()}
	now := time.Now().UTC()

	cfg := `{"schedule":{"type":"interval","value":10,"unit":"minute"},"checkin_margin":1}`
	body := "{}\n{\"type\":\"check_in\"}\n" + checkInBody(hexID("run-2"), "sync-job", "in_progress", cfg) + "\n"
	if res, err := in.Ingest(ctx, p, sentry.Parse([]byte(body), now), now); err != nil || res.CheckIns != 1 {
		t.Fatalf("first: %+v %v", res, err)
	}
	body = "{}\n{\"type\":\"check_in\"}\n" + checkInBody(zeroCheckInID, "sync-job", "ok", "") + "\n"
	res, err := in.Ingest(ctx, p, sentry.Parse([]byte(body), now), now)
	if err != nil || res.CheckIns != 1 {
		t.Fatalf("zero-id completion: %+v %v", res, err)
	}
	if n := count(t, st, "SELECT count(*) FROM monitor_checkins WHERE project_id = $1 AND monitor_slug = 'sync-job'", p.ID); n != 1 {
		t.Fatalf("zero-id created a new row instead of completing the existing one: %d rows", n)
	}
	m, err := st.GetMonitor(ctx, sqlc.GetMonitorParams{ProjectID: p.ID, Slug: "sync-job"})
	if err != nil {
		t.Fatal(err)
	}
	if !m.LastStatus.Valid || m.LastStatus.CheckinStatus != "ok" || m.ConsecutiveSuccesses != 1 || m.NextExpectedAt == nil {
		t.Fatalf("monitor state after terminal check-in = %+v", m)
	}

	// A zero id with nothing open to update (no in_progress run) is a
	// no-op, not a new bare row.
	body = "{}\n{\"type\":\"check_in\"}\n" + checkInBody(zeroCheckInID, "sync-job", "ok", "") + "\n"
	if res, err = in.Ingest(ctx, p, sentry.Parse([]byte(body), now), now); err != nil || res.CheckIns != 0 {
		t.Fatalf("zero-id no-op: %+v %v", res, err)
	}
	if n := count(t, st, "SELECT count(*) FROM monitor_checkins WHERE project_id = $1 AND monitor_slug = 'sync-job'", p.ID); n != 1 {
		t.Fatalf("zero-id no-op created a row: %d rows", n)
	}
}

// TestIngestCheckInOrphanDropped: a check-in against a monitor CrashCart
// has never seen a monitor_config for is dropped, not auto-created bare.
func TestIngestCheckInOrphanDropped(t *testing.T) {
	st := testdb.New(t)
	ctx := context.Background()
	p := newProject(t, st, "ci3")
	in := &Ingester{Store: st, Cfg: config.Config{}, Log: slog.Default()}
	now := time.Now().UTC()

	body := "{}\n{\"type\":\"check_in\"}\n" + checkInBody(hexID("orphan"), "never-configured", "ok", "") + "\n"
	res, err := in.Ingest(ctx, p, sentry.Parse([]byte(body), now), now)
	if err != nil || res.CheckIns != 0 || res.Monitors != 0 {
		t.Fatalf("orphan check-in: %+v %v", res, err)
	}
	if n := count(t, st, "SELECT count(*) FROM monitors WHERE project_id = $1", p.ID); n != 0 {
		t.Fatalf("orphan check-in created a monitor: %d rows", n)
	}
}

// TestIngestCheckInDoesNotConsumeQuota: like client_report, a check-in is
// not an event and must not count against the daily quota.
func TestIngestCheckInDoesNotConsumeQuota(t *testing.T) {
	st := testdb.New(t)
	ctx := context.Background()
	p := newProject(t, st, "ci4")
	p.DailyQuota = 1
	in := &Ingester{Store: st, Cfg: config.Config{}, Log: slog.Default()}
	now := time.Now().UTC()

	cfg := `{"schedule":{"type":"interval","value":1,"unit":"hour"}}`
	body := "{}\n{\"type\":\"check_in\"}\n" + checkInBody(hexID("q-run"), "quota-job", "ok", cfg) + "\n"
	if res, err := in.Ingest(ctx, p, sentry.Parse([]byte(body), now), now); err != nil || res.CheckIns != 1 {
		t.Fatalf("check-in: %+v %v", res, err)
	}
	id := hexID("q-event")
	ev := fmt.Sprintf(`{"event_id":%q,"timestamp":%q,"level":"error","platform":"android","exception":{"values":[{"type":"E","value":"v","stacktrace":{"frames":[{"filename":"A.java","function":"a","lineno":1,"in_app":true}]}}]}}`, id, now.Format(time.RFC3339))
	body = "{\"event_id\":\"" + id + "\"}\n{\"type\":\"event\"}\n" + ev + "\n"
	if res, err := in.Ingest(ctx, p, sentry.Parse([]byte(body), now), now); err != nil || res.Stored != 1 {
		t.Fatalf("quota was consumed by the check-in: %+v %v", res, err)
	}
}

func jobCount(t *testing.T, st *store.Store, projectID int64, typ string) int64 {
	t.Helper()
	return count(t, st, "SELECT count(*) FROM jobs WHERE project_id = $1 AND kind = 'alert' AND args->>'type' = $2", projectID, typ)
}

// TestIngestMonitorFailureAndRecoveryFireOnce: the failure threshold
// fires monitor_failed exactly on the crossing tick, not again while
// still failing, and one ok crossing the recovery threshold fires
// monitor_recovered.
func TestIngestMonitorFailureAndRecoveryFireOnce(t *testing.T) {
	st := testdb.New(t)
	ctx := context.Background()
	p := newProject(t, st, "ci5")
	in := &Ingester{Store: st, Cfg: config.Config{}, Log: slog.Default()}
	now := time.Now().UTC()

	cfg := `{"schedule":{"type":"interval","value":1,"unit":"hour"},"failure_issue_threshold":2,"recovery_threshold":1}`
	body := "{}\n{\"type\":\"check_in\"}\n" + checkInBody(hexID("f1"), "flaky-job", "error", cfg) + "\n"
	if _, err := in.Ingest(ctx, p, sentry.Parse([]byte(body), now), now); err != nil {
		t.Fatal(err)
	}
	if n := jobCount(t, st, p.ID, "monitor_failed"); n != 0 {
		t.Fatalf("fired before crossing the threshold: %d jobs", n)
	}
	body = "{}\n{\"type\":\"check_in\"}\n" + checkInBody(hexID("f2"), "flaky-job", "error", "") + "\n"
	if _, err := in.Ingest(ctx, p, sentry.Parse([]byte(body), now), now); err != nil {
		t.Fatal(err)
	}
	if n := jobCount(t, st, p.ID, "monitor_failed"); n != 1 {
		t.Fatalf("monitor_failed jobs = %d, want 1", n)
	}
	// A third failure must not refire.
	body = "{}\n{\"type\":\"check_in\"}\n" + checkInBody(hexID("f3"), "flaky-job", "error", "") + "\n"
	if _, err := in.Ingest(ctx, p, sentry.Parse([]byte(body), now), now); err != nil {
		t.Fatal(err)
	}
	if n := jobCount(t, st, p.ID, "monitor_failed"); n != 1 {
		t.Fatalf("monitor_failed refired: %d jobs", n)
	}
	// Recovery: one ok (recovery_threshold=1) fires monitor_recovered once.
	body = "{}\n{\"type\":\"check_in\"}\n" + checkInBody(hexID("f4"), "flaky-job", "ok", "") + "\n"
	if _, err := in.Ingest(ctx, p, sentry.Parse([]byte(body), now), now); err != nil {
		t.Fatal(err)
	}
	if n := jobCount(t, st, p.ID, "monitor_recovered"); n != 1 {
		t.Fatalf("monitor_recovered jobs = %d, want 1", n)
	}
}

// TestRetiredKeyKeepsAuthenticating: Rotate does not invalidate the
// outgoing key — it keeps authenticating (touched, throttled) until its
// project_keys row is explicitly deleted, at which point it stops within
// the ingest cache TTL (a fresh Ingester per step sidesteps that cache).
func TestRetiredKeyKeepsAuthenticating(t *testing.T) {
	st := testdb.New(t)
	ctx := context.Background()
	p, _ := st.CreateProject(ctx, sqlc.CreateProjectParams{Slug: "rotate-app", Name: "App", PublicKey: "oldkey"})
	now := time.Now().UTC().Format(time.RFC3339)
	body := envelope(crash("1.0", now, 1))

	do := func(key string) int {
		in := &Ingester{Store: st, Cfg: config.Config{RateLimit: 0}, Log: slog.Default()}
		req := newRequest("POST", fmt.Sprintf("/api/%d/envelope/", p.ID), body)
		req.Header.Set("X-Sentry-Auth", "Sentry sentry_key="+key+", sentry_version=7")
		rec := newRecorder()
		in.Handler().ServeHTTP(rec, req)
		return rec.Code
	}

	np, err := st.RotateProjectKey(ctx, p.ID, "newkey")
	if err != nil {
		t.Fatal(err)
	}
	if np.PublicKey != "newkey" {
		t.Fatalf("current key = %q", np.PublicKey)
	}
	if c := do("newkey"); c != 200 {
		t.Fatalf("new key → %d", c)
	}
	if c := do("oldkey"); c != 200 {
		t.Fatalf("retired key should still authenticate: %d", c)
	}

	keys, err := st.ListProjectKeys(ctx, p.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(keys) != 1 || keys[0].PublicKey != "oldkey" || keys[0].LastUsedAt == nil {
		t.Fatalf("retired keys = %+v", keys)
	}

	if n, err := st.DeleteProjectKey(ctx, sqlc.DeleteProjectKeyParams{ProjectID: p.ID, ID: keys[0].ID}); err != nil || n != 1 {
		t.Fatalf("delete: n=%d err=%v", n, err)
	}
	if c := do("oldkey"); c != 401 {
		t.Fatalf("deleted key should stop authenticating: %d", c)
	}
	if c := do("newkey"); c != 200 {
		t.Fatalf("current key still works after old one deleted: %d", c)
	}
}

// TestRotateTwiceListsBothRetiredKeys: two rotations produce two
// independently listed and independently deletable retired rows.
func TestRotateTwiceListsBothRetiredKeys(t *testing.T) {
	st := testdb.New(t)
	ctx := context.Background()
	p, _ := st.CreateProject(ctx, sqlc.CreateProjectParams{Slug: "rotate-twice", Name: "App", PublicKey: "k0"})
	if _, err := st.RotateProjectKey(ctx, p.ID, "k1"); err != nil {
		t.Fatal(err)
	}
	if _, err := st.RotateProjectKey(ctx, p.ID, "k2"); err != nil {
		t.Fatal(err)
	}
	keys, err := st.ListProjectKeys(ctx, p.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(keys) != 2 {
		t.Fatalf("retired keys = %+v, want 2 (k0 and k1)", keys)
	}
	var got []string
	for _, k := range keys {
		got = append(got, k.PublicKey)
	}
	if !strings.Contains(strings.Join(got, ","), "k0") || !strings.Contains(strings.Join(got, ","), "k1") {
		t.Fatalf("retired keys = %v, want k0 and k1", got)
	}
	if n, err := st.DeleteProjectKey(ctx, sqlc.DeleteProjectKeyParams{ProjectID: p.ID, ID: keys[0].ID}); err != nil || n != 1 {
		t.Fatalf("delete one: n=%d err=%v", n, err)
	}
	keys, err = st.ListProjectKeys(ctx, p.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(keys) != 1 {
		t.Fatalf("after deleting one, retired keys = %+v, want 1", keys)
	}
}
