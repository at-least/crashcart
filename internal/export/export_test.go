package export

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/at-least/crashcart/internal/auth"
	"github.com/at-least/crashcart/internal/blob"
	"github.com/at-least/crashcart/internal/config"
	"github.com/at-least/crashcart/internal/ingest"
	"github.com/at-least/crashcart/internal/sentry"
	"github.com/at-least/crashcart/internal/store"
	"github.com/at-least/crashcart/internal/symbolicate"
	"github.com/at-least/crashcart/internal/testdb"
)

const envelope = `{"event_id":"a1b2","sent_at":"2026-08-20T10:00:00Z"}
{"type":"event"}
{"event_id":"e1","timestamp":"2026-08-20T09:59:00Z","level":"fatal","platform":"android","release":"2.4.0","transaction":"CartFragment","tags":{"device_id":"did-1","locale":"en"},"user":{"id":"u1"},"exception":{"values":[{"type":"NullPointerException","value":"null","mechanism":{"type":"uncaught","handled":false},"stacktrace":{"frames":[{"module":"com.example.CartFragment","function":"render","filename":"CartFragment.kt","lineno":42,"in_app":true}]}}]},"breadcrumbs":{"values":[{"category":"ui","message":"tap","level":"info"}]}}
{"type":"event"}
{"event_id":"e2","timestamp":"2026-08-20T09:58:00Z","level":"info","platform":"cocoa","release":"2.4.0","logentry":{"formatted":"Checkout started <b>"}}
{"type":"sessions"}
{"aggregates":[{"started":"2026-08-20T09:00:00Z","exited":90,"crashed":1,"errored":2}],"attrs":{"release":"2.4.0","environment":"production"}}
`

// withAttachment is an event envelope carrying a (fake) PNG screenshot and
// a user_report; the header's event_id names the event both belong to.
const withAttachment = `{"event_id":"e3e3e3e3e3e3e3e3e3e3e3e3e3e3e3e3","sent_at":"2026-08-20T10:00:00Z"}
{"type":"event"}
{"event_id":"e3e3e3e3e3e3e3e3e3e3e3e3e3e3e3e3","timestamp":"2026-08-20T09:57:00Z","level":"error","platform":"android","release":"2.4.0","exception":{"values":[{"type":"IllegalStateException","value":"closed","stacktrace":{"frames":[{"module":"com.example.Cart","function":"pay","in_app":true}]}}]}}
{"type":"attachment","length":12,"filename":"screenshot.png","content_type":"image/png","attachment_type":"event.attachment"}
PNGPNGPNGPNG
{"type":"user_report"}
{"event_id":"e3e3e3e3e3e3e3e3e3e3e3e3e3e3e3e3","name":"Alex","email":"alex@example.com","comments":"payment closed unexpectedly"}
`

// checkInEnvelope upserts one monitor (a monitor_config on its first,
// in_progress check-in) — a durable config fact, so it round-trips.
const checkInEnvelope = `{}
{"type":"check_in"}
{"check_in_id":"c1c1c1c1c1c1c1c1c1c1c1c1c1c1c1c1","monitor_slug":"nightly-backup","status":"in_progress","monitor_config":{"schedule":{"type":"crontab","value":"0 * * * *"},"checkin_margin":5}}
`

// fill writes a project with events, sessions, an issue, a symbol file,
// alert rules and a channel into st.
func fill(t *testing.T, st *store.Store) store.Project {
	t.Helper()
	ctx := context.Background()
	plat := "android"
	p, err := store.CreateProject(ctx, st.Pool, "shop", "Shop", &plat, "0123456789abcdef0123456789abcdef")
	if err != nil {
		t.Fatal(err)
	}
	p.SampleRate = 1 // store everything, ungrouped events included
	in := &ingest.Ingester{Store: st, Cfg: config.Config{}, Log: slog.Default()}
	now := time.Date(2026, 8, 20, 10, 0, 0, 0, time.UTC)
	res, err := in.Ingest(ctx, p, sentry.Parse([]byte(envelope), now), now)
	if err != nil {
		t.Fatal(err)
	}
	if res.Stored != 2 || res.Sessions != 3 {
		t.Fatalf("ingest: %+v", res)
	}
	// One more event in its own envelope, with a screenshot attached.
	res2, err := in.Ingest(ctx, p, sentry.Parse([]byte(withAttachment), now), now)
	if err != nil {
		t.Fatal(err)
	}
	if res2.Stored != 1 || res2.Attachments != 1 || res2.UserReports != 1 {
		t.Fatalf("ingest with attachment: %+v", res2)
	}
	res3, err := in.Ingest(ctx, p, sentry.Parse([]byte(checkInEnvelope), now), now)
	if err != nil {
		t.Fatal(err)
	}
	if res3.Monitors != 1 || res3.CheckIns != 1 {
		t.Fatalf("ingest check-in: %+v", res3)
	}
	// A rotation, so the export carries one retired-but-still-valid key.
	if _, err := st.RotateProjectKey(ctx, p.ID, "fedcba9876543210fedcba9876543210"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.SetIssueStatus(ctx, st.Pool, store.SetIssueStatusParams{ProjectID: p.ID, Fingerprint: res.NewIssues[0], Status: "resolved"}); err != nil {
		t.Fatal(err)
	}
	dbg := "abc-123"
	if _, err := store.UpsertSymbolFile(ctx, st.Pool, store.UpsertSymbolFileParams{ProjectID: p.ID, Kind: "proguard", Release: strPtr("2.4.0"), DebugID: &dbg, Filename: "mapping.txt", Size: 5, Data: []byte("a -> b")}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.UpsertAlertRule(ctx, st.Pool, p.ID, "new_issue", true, 30); err != nil {
		t.Fatal(err)
	}
	if _, err := store.CreateAlertChannel(ctx, st.Pool, p.ID, "webhook", json.RawMessage(`{"url":"https://hooks.example.com/x"}`)); err != nil {
		t.Fatal(err)
	}
	// Accounts: a user and an API key it created (full exports carry them).
	u, err := store.CreateUser(ctx, st.Pool, "ops@example.com", "Ops", "$2a$10$hash")
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := (&auth.Access{Store: st}).CreateAPIKey(ctx, "ci", &u.ID); err != nil {
		t.Fatal(err)
	}
	return p
}

// lines returns the sorted NDJSON lines without _meta.
func lines(t *testing.T, b []byte) []string {
	t.Helper()
	var out []string
	for _, l := range strings.Split(strings.TrimSpace(string(b)), "\n") {
		if strings.HasPrefix(l, `{"t":"_meta"`) {
			continue
		}
		out = append(out, l)
	}
	slices.Sort(out)
	return out
}

func TestRoundTrip(t *testing.T) {
	src := testdb.New(t)
	dst := testdb.New(t)
	ctx := context.Background()
	fill(t, src)

	var a bytes.Buffer
	if err := Export(ctx, src, &a, Options{}); err != nil {
		t.Fatal(err)
	}
	first := strings.SplitN(a.String(), "\n", 2)[0]
	var meta struct {
		T      string `json:"t"`
		Format int    `json:"format"`
		App    string `json:"app"`
	}
	if err := json.Unmarshal([]byte(first), &meta); err != nil || meta.T != "_meta" || meta.Format != Format || meta.App != "crashcart" {
		t.Fatalf("meta line: %s (%v)", first, err)
	}
	// Table order and shape.
	got := map[string]int{}
	seq := []string{}
	for _, l := range strings.Split(strings.TrimSpace(a.String()), "\n")[1:] {
		var h struct {
			T         string          `json:"t"`
			Project   string          `json:"project"`
			ProjectID json.RawMessage `json:"project_id"`
		}
		if err := json.Unmarshal([]byte(l), &h); err != nil {
			t.Fatal(err)
		}
		if h.ProjectID != nil {
			t.Fatalf("project_id leaked: %s", l)
		}
		if h.T != "projects" && h.T != "users" && h.T != "api_keys" && h.Project != "shop" {
			t.Fatalf("row without project slug: %s", l)
		}
		if len(seq) == 0 || seq[len(seq)-1] != h.T {
			seq = append(seq, h.T)
		}
		got[h.T]++
	}
	if !slices.Equal(seq, Tables) {
		t.Fatalf("table order %v, want %v", seq, Tables)
	}
	want := map[string]int{
		"users": 1, "api_keys": 1, "projects": 1, "project_keys": 1, "releases": 1, "issues": 2, "events": 3, "attachments": 1, "user_reports": 1,
		"monitors": 1, "monitor_checkins": 1, "sessions": 3, "symbol_files": 1, "alert_rules": 1, "alert_channels": 1,
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("%s: %d rows, want %d", k, got[k], v)
		}
	}
	if strings.Contains(a.String(), `"created_by":1`) || !strings.Contains(a.String(), `"created_by":"ops@example.com"`) {
		t.Error("api key created_by must be the user's email, not an id")
	}
	if !strings.Contains(a.String(), `"data":"YSAtPiBi"`) {
		t.Error("bytea not base64")
	}
	if strings.Contains(a.String(), `\u003c`) {
		t.Error("HTML escaped in export")
	}

	rep, err := Import(ctx, dst, bytes.NewReader(a.Bytes()))
	if err != nil {
		t.Fatal(err)
	}
	for k, v := range want {
		if rep.Rows[k] != int64(v) {
			t.Errorf("import %s: %d, want %d", k, rep.Rows[k], v)
		}
	}
	// The account and the key work in the new database.
	if u, err := store.GetUserByEmail(ctx, dst.Pool, "ops@example.com"); err != nil || u.PasswordHash != "$2a$10$hash" {
		t.Fatalf("imported user: %+v %v", u, err)
	}
	var keyOwner string
	if err := dst.Pool.QueryRow(ctx, "SELECT u.email FROM api_keys k JOIN users u ON u.id = k.created_by WHERE k.name = 'ci'").Scan(&keyOwner); err != nil || keyOwner != "ops@example.com" {
		t.Fatalf("imported api key owner: %q %v", keyOwner, err)
	}
	var b bytes.Buffer
	if err := Export(ctx, dst, &b, Options{}); err != nil {
		t.Fatal(err)
	}
	if la, lb := lines(t, a.Bytes()), lines(t, b.Bytes()); !slices.Equal(la, lb) {
		t.Fatalf("round trip differs:\n%s\n---\n%s", strings.Join(la, "\n"), strings.Join(lb, "\n"))
	}

	// Importing again is a no-op.
	if _, err := Import(ctx, dst, bytes.NewReader(a.Bytes())); err != nil {
		t.Fatal(err)
	}
	var c bytes.Buffer
	if err := Export(ctx, dst, &c, Options{}); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(lines(t, b.Bytes()), lines(t, c.Bytes())) {
		t.Fatal("second import changed data")
	}
	var n int
	if err := dst.Pool.QueryRow(ctx, "SELECT count(*) FROM alert_channels").Scan(&n); err != nil || n != 1 {
		t.Fatalf("alert_channels duplicated: %d %v", n, err)
	}
	// The issue's counts are not added up by a re-import…
	var cnt int64
	if err := dst.Pool.QueryRow(ctx, "SELECT event_count FROM issues").Scan(&cnt); err != nil || cnt != 1 {
		t.Fatalf("issue event_count %d %v", cnt, err)
	}
	// …and never go down: a backup restored onto a live project keeps
	// the live (higher) count.
	if _, err := dst.Pool.Exec(ctx, "UPDATE issues SET event_count = 7, stored_count = 5"); err != nil {
		t.Fatal(err)
	}
	if _, err := Import(ctx, dst, bytes.NewReader(a.Bytes())); err != nil {
		t.Fatal(err)
	}
	var stored int64
	if err := dst.Pool.QueryRow(ctx, "SELECT event_count, stored_count FROM issues").Scan(&cnt, &stored); err != nil || cnt != 7 || stored != 5 {
		t.Fatalf("counts after import onto live data = %d/%d %v (want 7/5)", cnt, stored, err)
	}
}

// TestImportCommitsInChunks: a bad line fails the import, but the chunks
// committed before it stay (the error says so), and a re-run of the
// fixed file is idempotent.
func TestImportCommitsInChunks(t *testing.T) {
	st := testdb.New(t)
	ctx := context.Background()
	old := CommitEvery
	CommitEvery = 2
	t.Cleanup(func() { CommitEvery = old })
	row := func(i int) string {
		return fmt.Sprintf(`{"t":"events","project":"p","occurred_at":"2026-08-20T10:%02d:00Z","event_id":"%032x","level":"error","message":"m","tags":{},"payload":{}}`, i, i)
	}
	in := `{"t":"_meta","format":1,"exported_at":"2026-08-20T10:00:00Z","app":"crashcart"}` + "\n" +
		row(1) + "\n" + row(2) + "\n" + row(3) + "\n" + `{"t":"events","project":"p","event_id":"bad"}` + "\n"
	rep, err := Import(ctx, st, strings.NewReader(in))
	if err == nil || !strings.Contains(err.Error(), "line 5") || !strings.Contains(err.Error(), "lines 1-4 were committed") {
		t.Fatalf("err = %v, report %+v", err, rep)
	}
	var n int
	if err := st.Pool.QueryRow(ctx, "SELECT count(*) FROM events").Scan(&n); err != nil || n != 3 {
		t.Fatalf("events after failed import = %d (want the 3 committed before the bad line)", n)
	}
	// Two chunks (lines 1-2 and 3-4) were committed, the third was not.
	if rep.Committed != 4 {
		t.Fatalf("committed lines = %d, want 4", rep.Committed)
	}
	fixed := strings.Replace(in, `{"t":"events","project":"p","event_id":"bad"}`, row(4), 1)
	rep, err = Import(ctx, st, strings.NewReader(fixed))
	if err != nil || rep.Rows["events"] != 4 || rep.Committed != 5 {
		t.Fatalf("re-run: %v %+v", err, rep)
	}
	if err := st.Pool.QueryRow(ctx, "SELECT count(*) FROM events").Scan(&n); err != nil || n != 4 {
		t.Fatalf("events after re-run = %d", n)
	}
}

func TestImportCreatesProjectAndSkipsUnknown(t *testing.T) {
	st := testdb.New(t)
	ctx := context.Background()
	in := `{"t":"_meta","format":1,"exported_at":"2026-08-20T10:00:00Z","app":"crashcart"}
{"t":"events","project":"fresh","occurred_at":"2026-08-20T10:00:00.000123Z","event_id":"abababababababababababababababab","level":"error","message":"m","tags":{},"breadcrumbs":[],"payload":{"a":1}}
{"t":"widgets","project":"fresh"}
`
	rep, err := Import(ctx, st, strings.NewReader(in))
	if err != nil {
		t.Fatal(err)
	}
	if rep.Rows["events"] != 1 || rep.Rows["skipped"] != 1 {
		t.Fatalf("report %+v", rep.Rows)
	}
	p, err := store.GetProject(ctx, st.Pool, "fresh")
	if err != nil || p.Name != "fresh" || len(p.PublicKey) != 32 {
		t.Fatalf("project: %+v %v", p, err)
	}
	var n int
	if err := st.Pool.QueryRow(ctx, "SELECT count(*) FROM events WHERE project_id = $1", p.ID).Scan(&n); err != nil || n != 1 {
		t.Fatalf("events %d %v", n, err)
	}
}

func TestExportProjectFilter(t *testing.T) {
	st := testdb.New(t)
	ctx := context.Background()
	fill(t, st)
	if _, err := store.CreateProject(ctx, st.Pool, "other", "Other", nil, "ffffffffffffffffffffffffffffffff"); err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	if err := Export(ctx, st, &buf, Options{Project: "other"}); err != nil {
		t.Fatal(err)
	}
	if got := lines(t, buf.Bytes()); len(got) != 1 || !strings.Contains(got[0], `"slug":"other"`) {
		t.Fatalf("filtered export: %v", got)
	}
	if err := Export(ctx, st, &buf, Options{Project: "nope"}); err == nil {
		t.Fatal("unknown project should fail")
	}
}

func strPtr(s string) *string { return &s }

// TestImportFormat1Names: a format-1 file (screen / error_location /
// crash_spike) still imports — the old names map onto transaction /
// culprit / unhandled_spike.
func TestImportFormat1Names(t *testing.T) {
	st := testdb.New(t)
	ctx := context.Background()
	in := `{"t":"_meta","format":1,"exported_at":"2026-08-20T10:00:00Z","app":"crashcart"}
{"t":"projects","slug":"old","name":"Old","public_key":"0123456789abcdef0123456789abcdef"}
{"t":"issues","project":"old","fingerprint":"f1f1f1f1f1f1f1f1f1f1f1f1f1f1f1f1","title":"E: boom","level":"error","screen":"CartFragment","status":"unresolved","event_count":1,"stored_count":1,"first_seen":"2026-08-20T10:00:00Z","last_seen":"2026-08-20T10:00:00Z"}
{"t":"events","project":"old","occurred_at":"2026-08-20T10:00:00.000123Z","event_id":"abababababababababababababababab","level":"error","message":"m","screen":"CartFragment","error_location":"CartFragment.java:1","tags":{},"breadcrumbs":[],"payload":{"a":1}}
{"t":"alert_rules","project":"old","type":"crash_spike","enabled":false,"cooldown_minutes":5}
`
	if _, err := Import(ctx, st, strings.NewReader(in)); err != nil {
		t.Fatal(err)
	}
	var transaction, culprit string
	if err := st.Pool.QueryRow(ctx, "SELECT transaction, culprit FROM events").Scan(&transaction, &culprit); err != nil || transaction != "CartFragment" || culprit != "CartFragment.java:1" {
		t.Fatalf("event: %q %q %v", transaction, culprit, err)
	}
	if err := st.Pool.QueryRow(ctx, "SELECT transaction FROM issues").Scan(&transaction); err != nil || transaction != "CartFragment" {
		t.Fatalf("issue: %q %v", transaction, err)
	}
	var enabled bool
	if err := st.Pool.QueryRow(ctx, "SELECT enabled FROM alert_rules WHERE type = 'unhandled_spike'").Scan(&enabled); err != nil || enabled {
		t.Fatalf("alert rule: enabled=%v %v", enabled, err)
	}
}

// TestExportShapeAndImportMarksDirty: the file carries times as RFC3339
// UTC and payloads as decoded JSON (not gzip, not base64), never the rollup
// or dirty tables; importing marks the written hours dirty so the stats
// views are right before any rollup runs.
func TestExportShapeAndImportMarksDirty(t *testing.T) {
	src := testdb.New(t)
	dst := testdb.New(t)
	ctx := context.Background()
	fill(t, src)
	var buf bytes.Buffer
	if err := Export(ctx, src, &buf, Options{}); err != nil {
		t.Fatal(err)
	}
	events := 0
	for _, l := range strings.Split(strings.TrimSpace(buf.String()), "\n") {
		var row map[string]json.RawMessage
		if err := json.Unmarshal([]byte(l), &row); err != nil {
			t.Fatal(err)
		}
		var tbl string
		json.Unmarshal(row["t"], &tbl)
		if strings.Contains(tbl, "rolled") || strings.Contains(tbl, "dirty") || strings.Contains(tbl, "usage") || strings.Contains(tbl, "jobs") {
			t.Errorf("derived table exported: %s", tbl)
		}
		for _, k := range []string{"occurred_at", "started_at", "first_seen", "last_seen", "created_at"} {
			if v, ok := row[k]; ok {
				var s string
				if json.Unmarshal(v, &s) != nil || !strings.HasSuffix(s, "Z") {
					t.Errorf("%s.%s = %s, want RFC3339 UTC", tbl, k, v)
				} else if _, err := time.Parse(time.RFC3339Nano, s); err != nil {
					t.Errorf("%s.%s = %s: %v", tbl, k, s, err)
				}
			}
		}
		if tbl == "events" {
			events++
			pl, ok := row["payload"]
			if !ok || !bytes.HasPrefix(pl, []byte("{")) || !bytes.Contains(pl, []byte(`"event_id"`)) {
				t.Errorf("events.payload must be the decoded event JSON: %.80s", pl)
			}
			if !bytes.Contains(row["event_id"], []byte(`"`)) || bytes.Contains(row["event_id"], []byte("-")) {
				t.Errorf("event_id = %s, want 32-hex", row["event_id"])
			}
		}
	}
	if events != 3 {
		t.Fatalf("events exported = %d", events)
	}

	if _, err := Import(ctx, dst, bytes.NewReader(buf.Bytes())); err != nil {
		t.Fatal(err)
	}
	var dirtyE, dirtyS, rolled int
	if err := dst.Pool.QueryRow(ctx, "SELECT (SELECT count(*) FROM event_stats_dirty), (SELECT count(*) FROM session_stats_dirty), (SELECT count(*) FROM event_stats_hourly_rolled)").Scan(&dirtyE, &dirtyS, &rolled); err != nil {
		t.Fatal(err)
	}
	// Events at 09:57–09:59 → one hour; sessions at 09:00 → one hour.
	if dirtyE != 1 || dirtyS != 1 || rolled != 0 {
		t.Errorf("after import: event dirty hours = %d, session dirty hours = %d, rolled rows = %d (want 1, 1, 0)", dirtyE, dirtyS, rolled)
	}
	var evs, unhandled, total, crashed int64
	if err := dst.Pool.QueryRow(ctx, "SELECT COALESCE(sum(events),0), COALESCE(sum(unhandled),0) FROM event_stats_hourly").Scan(&evs, &unhandled); err != nil {
		t.Fatal(err)
	}
	if err := dst.Pool.QueryRow(ctx, "SELECT COALESCE(sum(total),0), COALESCE(sum(crashed),0) FROM release_health_hourly").Scan(&total, &crashed); err != nil {
		t.Fatal(err)
	}
	if evs != 3 || unhandled != 1 || total != 93 || crashed != 1 {
		t.Errorf("stats before rollup: events %d unhandled %d sessions %d crashed %d (want 3, 1, 93, 1)", evs, unhandled, total, crashed)
	}
}

// TestRoundTripBlobStore: a symbol file in the blob store is inlined in the
// export like any other, and import writes it through the destination's
// own store — or into the data column when the destination has none. That
// is how a database moves between backends.
func TestRoundTripBlobStore(t *testing.T) {
	src, dst := testdb.New(t), testdb.New(t)
	ctx := context.Background()
	p, err := store.CreateProject(ctx, src.Pool, "app", "App", nil, "0123456789abcdef0123456789abcdef")
	if err != nil {
		t.Fatal(err)
	}
	memSrc, memDst := &blob.Memory{}, &blob.Memory{}
	src.Blobs, dst.Blobs = memSrc, memDst
	s := &symbolicate.Service{Store: src, DSYM: symbolicate.NewDSYMClient("")}
	mapping := []byte("com.example.Foo -> a.b:\n    void bar() -> c\n")
	if _, err := s.Upload(ctx, p.ID, "1.0", symbolicate.KindProGuard, "mapping.txt", mapping); err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	if err := Export(ctx, src, &buf, Options{}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(buf.String(), `"data":"`+base64.StdEncoding.EncodeToString(mapping)+`"`) {
		t.Fatalf("export must inline the object's bytes:\n%s", buf.String())
	}
	// Without the store the export refuses rather than writing a file
	// with the bytes missing.
	src.Blobs = nil
	if err := Export(ctx, src, io.Discard, Options{}); err == nil || !strings.Contains(err.Error(), "BLOB_STORE") {
		t.Fatalf("export of a blob row without a store: %v", err)
	}
	src.Blobs = memSrc

	file := buf.Bytes()
	if _, err := Import(ctx, dst, bytes.NewReader(file)); err != nil {
		t.Fatal(err)
	}
	var data []byte
	var key *string
	if err := dst.Pool.QueryRow(ctx, "SELECT data, blob_key FROM symbol_files").Scan(&data, &key); err != nil {
		t.Fatal(err)
	}
	if data != nil || key == nil {
		t.Fatalf("imported row location: data=%v key=%v", data, key)
	}
	if got, err := memDst.Get(ctx, *key); err != nil || !bytes.Equal(got, mapping) {
		t.Fatalf("imported object: %q %v", got, err)
	}
	if len(memSrc.Keys()) != 1 {
		t.Fatalf("source store touched by import: %v", memSrc.Keys())
	}
	// Importing again replaces the object and deletes the one replaced.
	if _, err := Import(ctx, dst, bytes.NewReader(file)); err != nil {
		t.Fatal(err)
	}
	if keys := memDst.Keys(); len(keys) != 1 || keys[0] == *key {
		t.Fatalf("objects after re-import: %v (first was %s)", keys, *key)
	}
	// A destination without a store keeps the bytes in the row.
	dst2 := testdb.New(t)
	if _, err := Import(ctx, dst2, bytes.NewReader(file)); err != nil {
		t.Fatal(err)
	}
	if err := dst2.Pool.QueryRow(ctx, "SELECT data, blob_key FROM symbol_files").Scan(&data, &key); err != nil || !bytes.Equal(data, mapping) || key != nil {
		t.Fatalf("import into postgres mode: data=%d bytes key=%v %v", len(data), key, err)
	}
}

// TestRoundTripPacks: events whose payloads live in packs export with the
// bytes inlined — about one GET per pack, not per event — and import into
// a store-backed destination packs them again (the import drains the
// spool before returning), or into the column without a store.
func TestRoundTripPacks(t *testing.T) {
	src, dst := testdb.New(t), testdb.New(t)
	ctx := context.Background()
	memSrc, memDst := &blob.Memory{}, &blob.Memory{}
	src.Blobs, dst.Blobs = memSrc, memDst
	p := fill(t, src) // 3 events through ingest → spooled, not in the column
	var inColumn int
	if err := src.Pool.QueryRow(ctx, "SELECT count(*) FROM events WHERE payload IS NOT NULL").Scan(&inColumn); err != nil || inColumn != 0 {
		t.Fatalf("payload column used with a store: %d %v", inColumn, err)
	}
	if n, err := src.Drain(ctx); err != nil || n != 3 {
		t.Fatalf("drain: %d %v", n, err)
	}
	packs := 0
	for _, k := range memSrc.Keys() {
		if strings.HasPrefix(k, "events/") {
			packs++
		}
	}
	if packs != 1 {
		t.Fatalf("packs for one project-week: %d (%v)", packs, memSrc.Keys())
	}
	before := memSrc.Gets()
	var buf bytes.Buffer
	if err := Export(ctx, src, &buf, Options{}); err != nil {
		t.Fatal(err)
	}
	if got := memSrc.Gets() - before; got > packs+1 { // + the symbol file
		t.Fatalf("export made %d object reads for %d packs", got, packs)
	}
	if n := strings.Count(buf.String(), `"payload":{`); n != 3 {
		t.Fatalf("payloads inlined: %d of 3\n%s", n, buf.String())
	}
	// Into a store-backed destination: spooled by import, packed before it returns.
	if _, err := Import(ctx, dst, bytes.NewReader(buf.Bytes())); err != nil {
		t.Fatal(err)
	}
	if n, _ := store.SpoolCount(ctx, dst.Pool); n != 0 {
		t.Fatalf("import left %d rows in the spool", n)
	}
	var packed int
	if err := dst.Pool.QueryRow(ctx, "SELECT count(*) FROM event_packs").Scan(&packed); err != nil || packed != 3 {
		t.Fatalf("event_packs after import = %d %v", packed, err)
	}
	ev, err := store.GetEvent(ctx, dst.Pool, mustProject(t, dst, p.Slug).ID, sentry.ID(strings.Repeat("e3", 16)))
	if err != nil {
		t.Fatal(err)
	}
	if b, err := dst.Payload(ctx, nil, ev); err != nil || !bytes.Contains(b, []byte("IllegalStateException")) {
		t.Fatalf("imported payload from a pack: %.60q %v", b, err)
	}
	// Into a destination without a store: the column.
	dst2 := testdb.New(t)
	if _, err := Import(ctx, dst2, bytes.NewReader(buf.Bytes())); err != nil {
		t.Fatal(err)
	}
	if err := dst2.Pool.QueryRow(ctx, "SELECT count(*) FROM events WHERE payload IS NOT NULL").Scan(&inColumn); err != nil || inColumn != 3 {
		t.Fatalf("import into postgres mode: %d payloads in the column %v", inColumn, err)
	}
}

func mustProject(t *testing.T, st *store.Store, slug string) store.Project {
	t.Helper()
	p, err := store.GetProject(context.Background(), st.Pool, slug)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

// TestExportReadsEachPackOnce: events arrive out of time order (a mobile
// crash lands a day late), so a week's packs interleave in occurred_at;
// the export streams in pack order and reads every pack exactly once,
// never a whole pack per event.
func TestExportReadsEachPackOnce(t *testing.T) {
	st := testdb.New(t)
	testdb.Projects(t, st, 1)
	mem := &blob.Memory{}
	st.Blobs = mem
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Second)
	insert := func(at time.Time, raw string) {
		t.Helper()
		row := store.EventInsert{OccurredAt: at, ProjectID: 1, EventID: sentry.DerivedID([]byte(raw)), Level: "error", Message: "m", Tags: []byte("{}"), Payload: store.Gzip([]byte(raw))}
		if err := st.Tx(ctx, func(ctx context.Context, tx pgx.Tx) error {
			return st.InsertEvents(ctx, tx, []store.EventInsert{row})
		}); err != nil {
			t.Fatal(err)
		}
	}
	// Three flushes, each packing events that are older than the previous
	// flush's: by occurred_at the packs interleave completely.
	for flush := 0; flush < 3; flush++ {
		for i := 0; i < 4; i++ {
			insert(now.Add(-time.Duration(flush)*time.Hour-time.Duration(i)*time.Minute), fmt.Sprintf(`{"flush":%d,"i":%d}`, flush, i))
		}
		if _, err := st.Drain(ctx); err != nil {
			t.Fatal(err)
		}
	}
	packs := len(mem.Keys())
	if packs != 3 {
		t.Fatalf("packs = %d (%v)", packs, mem.Keys())
	}
	before := mem.Gets()
	var buf bytes.Buffer
	if err := Export(ctx, st, &buf, Options{}); err != nil {
		t.Fatal(err)
	}
	if got := mem.Gets() - before; got != packs {
		t.Fatalf("export read objects %d times for %d packs", got, packs)
	}
	if n := strings.Count(buf.String(), `"t":"events"`); n != 12 {
		t.Fatalf("events exported = %d", n)
	}
}
