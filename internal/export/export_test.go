package export

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/crashcartapp/crashcart/internal/auth"
	"github.com/crashcartapp/crashcart/internal/config"
	"github.com/crashcartapp/crashcart/internal/db/sqlc"
	"github.com/crashcartapp/crashcart/internal/ingest"
	"github.com/crashcartapp/crashcart/internal/sentry"
	"github.com/crashcartapp/crashcart/internal/store"
	"github.com/crashcartapp/crashcart/internal/testdb"
)

const envelope = `{"event_id":"a1b2","sent_at":"2026-08-20T10:00:00Z"}
{"type":"event"}
{"event_id":"e1","timestamp":"2026-08-20T09:59:00Z","level":"fatal","platform":"android","release":"2.4.0","transaction":"CartFragment","tags":{"device_id":"did-1","locale":"en"},"user":{"id":"u1"},"exception":{"values":[{"type":"NullPointerException","value":"null","mechanism":{"type":"uncaught","handled":false},"stacktrace":{"frames":[{"module":"com.example.CartFragment","function":"render","filename":"CartFragment.kt","lineno":42,"in_app":true}]}}]},"breadcrumbs":{"values":[{"category":"ui","message":"tap","level":"info"}]}}
{"type":"event"}
{"event_id":"e2","timestamp":"2026-08-20T09:58:00Z","level":"info","platform":"cocoa","release":"2.4.0","logentry":{"formatted":"Checkout started <b>"}}
{"type":"sessions"}
{"aggregates":[{"started":"2026-08-20T09:00:00Z","exited":90,"crashed":1,"errored":2}],"attrs":{"release":"2.4.0","environment":"production"}}
`

// fill writes a project with events, sessions, an issue, a symbol file,
// alert rules and a channel into st.
func fill(t *testing.T, st *store.Store) sqlc.Project {
	t.Helper()
	ctx := context.Background()
	plat := "android"
	p, err := st.CreateProject(ctx, sqlc.CreateProjectParams{Slug: "shop", Name: "Shop", Platform: &plat, PublicKey: "0123456789abcdef0123456789abcdef"})
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
	if _, err := st.SetIssueStatus(ctx, sqlc.SetIssueStatusParams{ProjectID: p.ID, Fingerprint: res.NewIssues[0], Status: "resolved"}); err != nil {
		t.Fatal(err)
	}
	dbg := "abc-123"
	if _, err := st.UpsertSymbolFile(ctx, sqlc.UpsertSymbolFileParams{ProjectID: p.ID, Kind: "proguard", Release: strPtr("2.4.0"), DebugID: &dbg, Filename: "mapping.txt", Size: 5, Data: []byte("a -> b")}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.UpsertAlertRule(ctx, sqlc.UpsertAlertRuleParams{ProjectID: p.ID, Type: "new_issue", Enabled: true, CooldownMinutes: 30}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.CreateAlertChannel(ctx, sqlc.CreateAlertChannelParams{ProjectID: p.ID, Kind: "webhook", Config: json.RawMessage(`{"url":"https://hooks.example.com/x"}`)}); err != nil {
		t.Fatal(err)
	}
	// Accounts: a user and an API key it created (full exports carry them).
	u, err := st.CreateUser(ctx, sqlc.CreateUserParams{Email: "ops@example.com", Name: "Ops", PasswordHash: "$2a$10$hash"})
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
	if err := json.Unmarshal([]byte(first), &meta); err != nil || meta.T != "_meta" || meta.Format != 1 || meta.App != "crashcart" {
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
	want := map[string]int{"users": 1, "api_keys": 1, "projects": 1, "releases": 1, "issues": 1, "events": 2, "sessions": 3, "symbol_files": 1, "alert_rules": 1, "alert_channels": 1}
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
	if u, err := dst.GetUserByEmail(ctx, "ops@example.com"); err != nil || u.PasswordHash != "$2a$10$hash" {
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
	// The issue's counts are replaced, not added.
	var cnt int64
	if err := dst.Pool.QueryRow(ctx, "SELECT event_count FROM issues").Scan(&cnt); err != nil || cnt != 1 {
		t.Fatalf("issue event_count %d %v", cnt, err)
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
	p, err := st.GetProject(ctx, "fresh")
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
	if _, err := st.CreateProject(ctx, sqlc.CreateProjectParams{Slug: "other", Name: "Other", PublicKey: "ffffffffffffffffffffffffffffffff"}); err != nil {
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
