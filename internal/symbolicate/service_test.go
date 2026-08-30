package symbolicate

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/crashcartapp/crashcart/internal/config"
	"github.com/crashcartapp/crashcart/internal/db/sqlc"
	"github.com/crashcartapp/crashcart/internal/ingest"
	"github.com/crashcartapp/crashcart/internal/jobs"
	"github.com/crashcartapp/crashcart/internal/sentry"
	"github.com/crashcartapp/crashcart/internal/store"
	"github.com/crashcartapp/crashcart/internal/testdb"
)

// fakeSidecar implements the sidecar protocol in memory: PUT stores by
// key, POST answers 404 until the key is stored, then `answer`. warm
// pre-populates every key (a sidecar that already has the file).
type fakeSidecar struct {
	answer func() any
	warm   bool
	stored map[string][]byte
	posts  []string // symbol keys asked for
	frames string   // the last POST's frames, as JSON
	puts   int
}

func (f *fakeSidecar) server(t *testing.T) *httptest.Server {
	t.Helper()
	f.stored = map[string][]byte{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPut && strings.HasPrefix(r.URL.Path, "/symbols/"):
			body, _ := io.ReadAll(r.Body)
			f.stored[strings.TrimPrefix(r.URL.Path, "/symbols/")] = body
			f.puts++
			w.WriteHeader(http.StatusNoContent)
		case r.Method == http.MethodPost && r.URL.Path == "/symbolicate":
			var req struct {
				Symbol string          `json:"symbol"`
				Frames json.RawMessage `json:"frames"`
			}
			json.NewDecoder(r.Body).Decode(&req)
			f.posts = append(f.posts, req.Symbol)
			f.frames = string(req.Frames)
			if _, ok := f.stored[req.Symbol]; !ok && !f.warm {
				http.Error(w, `{"error":"unknown symbol"}`, http.StatusNotFound)
				return
			}
			switch a := f.answer().(type) {
			case int:
				http.Error(w, "sidecar error", a)
			default:
				json.NewEncoder(w).Encode(map[string]any{"frames": a})
			}
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func newProject(t *testing.T, st *store.Store) sqlc.Project {
	t.Helper()
	p, err := st.CreateProject(context.Background(), sqlc.CreateProjectParams{Slug: "p" + fmt.Sprint(time.Now().UnixNano()), Name: "P", PublicKey: fmt.Sprint(time.Now().UnixNano())})
	if err != nil {
		t.Fatal(err)
	}
	return p
}

// storeEvent writes one unsymbolicated event plus its issue the way ingest
// does, from a raw Sentry payload. Returns the event_id and fingerprint.
func storeEvent(t *testing.T, st *store.Store, p sqlc.Project, raw string) (id, fp sentry.ID, at time.Time) {
	t.Helper()
	ctx := context.Background()
	now := time.Now().UTC()
	ev := sentry.ParseEvent("", now, []byte(raw), now)
	if ev == nil {
		t.Fatal("bad payload")
	}
	fp = sentry.Fingerprint(ev, ev.Frames())
	id, at = ev.EventID, ev.Timestamp
	err := st.Tx(ctx, func(ctx context.Context, tx pgx.Tx, q *sqlc.Queries) error {
		if _, err := q.UpsertIssue(ctx, sqlc.UpsertIssueParams{
			ProjectID: p.ID, Fingerprint: fp, Title: ev.IssueTitle(), Level: sqlc.EventLevel(ev.Level), EventCount: 1, StoredCount: 1,
			FirstSeen: at, LastSeen: at, FirstRelease: nilIfEmpty(ev.Release), Platform: nilIfEmpty(ev.Platform),
		}); err != nil {
			return err
		}
		return store.InsertEvents(ctx, tx, []store.EventInsert{{
			OccurredAt: at, ProjectID: p.ID, EventID: ev.EventID, Level: ev.Level, Message: ev.Message, Platform: nilIfEmpty(ev.Platform),
			Release: nilIfEmpty(ev.Release), ErrorType: nilIfEmpty(ev.ErrorType), Fingerprint: &fp,
			Tags: []byte("{}"), Payload: store.Gzip(ev.Raw),
		}})
	})
	if err != nil {
		t.Fatal(err)
	}
	return id, fp, at
}

func upload(t *testing.T, st *store.Store, p sqlc.Project, kind, release, debugID, filename string, data []byte) {
	t.Helper()
	var did *string
	if debugID != "" {
		did = &debugID
	}
	_, err := st.UpsertSymbolFile(context.Background(), sqlc.UpsertSymbolFileParams{
		ProjectID: p.ID, Kind: sqlc.SymbolKind(kind), Release: nilIfEmpty(release), DebugID: did, Filename: filename, Size: int64(len(data)), Data: data,
	})
	if err != nil {
		t.Fatal(err)
	}
}

const androidEvent = `{"event_id":"a1","platform":"android","release":"1.0","timestamp":%d,"level":"error",
"exception":{"values":[{"type":"java.lang.NullPointerException","value":"boom","stacktrace":{"frames":[
 {"module":"android.os.Handler","function":"dispatchMessage","filename":"Handler.java","lineno":106},
 {"module":"a.b.c","function":"b","filename":"SourceFile","lineno":13,"in_app":true}]}}]},
"debug_meta":{"images":[{"type":"proguard","uuid":"%s","debug_id":"%s"}]}}`

func TestEventProGuard(t *testing.T) {
	st := testdb.New(t)
	ctx := context.Background()
	p := newProject(t, st)
	svc := &Service{Store: st}
	uuid := "11111111-2222-3333-4444-555555555555"
	raw := fmt.Sprintf(androidEvent, time.Now().Unix(), uuid, uuid)
	id, oldFP, at := storeEvent(t, st, p, raw)

	// No mapping yet: not an error, event stays unsymbolicated.
	if err := svc.Event(ctx, p.ID, id, at); err != nil {
		t.Fatal(err)
	}
	row, _ := st.GetEvent(ctx, sqlc.GetEventParams{ProjectID: p.ID, EventID: id})
	if row.Symbolicated {
		t.Fatal("should not be symbolicated without a mapping")
	}

	// Negative cache: the upload is invisible until Invalidate (or 60 s).
	upload(t, st, p, KindProGuard, "1.0", uuid, "mapping.txt", []byte(mappingTxt))
	now := time.Now().UTC()
	ev := sentry.ParseEvent("", now, []byte(raw), now)
	if _, ok := svc.Inline(ctx, p.ID, ev); ok {
		t.Fatal("negative cache should still hide the mapping")
	}
	svc.Invalidate(p.ID, "1.0")
	frames, ok := svc.Inline(ctx, p.ID, ev)
	if !ok || frames[1].Module != "com.example.CartFragment" || frames[1].Function != "loadCart" || frames[1].Lineno != 46 || !frames[1].IsInApp() {
		t.Fatalf("inline = %v %+v", ok, frames)
	}

	if err := svc.Event(ctx, p.ID, id, at); err != nil {
		t.Fatal(err)
	}
	row, err := st.GetEvent(ctx, sqlc.GetEventParams{ProjectID: p.ID, EventID: id})
	if err != nil {
		t.Fatal(err)
	}
	if !row.Symbolicated || row.Fingerprint == nil || *row.Fingerprint == oldFP {
		t.Fatalf("event after symbolication: symbolicated=%v fp=%v old=%s", row.Symbolicated, row.Fingerprint, oldFP)
	}
	if row.ErrorLocation == nil || *row.ErrorLocation != "CartFragment.java:46" {
		t.Errorf("error_location = %v", deref(row.ErrorLocation))
	}
	var syms []sentry.Frame
	if err := json.Unmarshal(row.Symbols, &syms); err != nil || len(syms) != 2 || syms[1].Module != "com.example.CartFragment" {
		t.Errorf("symbols = %s (%v)", row.Symbols, err)
	}
	// Issue moved: new one exists with count 1, old one is gone.
	ni, err := st.GetIssue(ctx, sqlc.GetIssueParams{ProjectID: p.ID, Fingerprint: *row.Fingerprint})
	if err != nil || ni.EventCount != 1 || ni.StoredCount != 1 || !ni.FirstSeen.Equal(row.OccurredAt) {
		t.Errorf("new issue = %+v %v", ni, err)
	}
	if _, err := st.GetIssue(ctx, sqlc.GetIssueParams{ProjectID: p.ID, Fingerprint: oldFP}); !errors.Is(err, pgx.ErrNoRows) {
		t.Errorf("old issue should be deleted: %v", err)
	}
	// Idempotent.
	if err := svc.Event(ctx, p.ID, id, at); err != nil {
		t.Fatal(err)
	}
	// Missing event is not an error.
	if err := svc.Event(ctx, p.ID, sentry.DerivedID([]byte("missing")), at); err != nil {
		t.Fatal(err)
	}
}

func TestReleaseSourceMap(t *testing.T) {
	st := testdb.New(t)
	ctx := context.Background()
	p := newProject(t, st)
	svc := &Service{Store: st}
	raw := fmt.Sprintf(`{"event_id":"%s","platform":"javascript","release":"web-7","timestamp":%d,"exception":{"values":[{"type":"TypeError","value":"x is undefined","stacktrace":{"frames":[{"filename":"https://app.example.com/static/bundle.min.js","function":"t","lineno":1,"colno":13,"in_app":true}]}}]}}`, "%s", time.Now().Unix())
	id1, _, _ := storeEvent(t, st, p, fmt.Sprintf(raw, "b1b1b1b1b1b1b1b1b1b1b1b1b1b1b1b1"))
	id2, _, _ := storeEvent(t, st, p, fmt.Sprintf(raw, "b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2"))
	raw = fmt.Sprintf(raw, "b3b3b3b3b3b3b3b3b3b3b3b3b3b3b3b3")
	// Negative-cache the release first, as ingest would have.
	now := time.Now().UTC()
	svc.Inline(ctx, p.ID, sentry.ParseEvent("", now, []byte(raw), now))
	upload(t, st, p, KindSourceMap, "web-7", "", "bundle.min.js.map", []byte(`{"version":3,"sources":["src/a.js"],"names":["foo"],"mappings":"AAAAA,UAEI"}`))

	if err := svc.Release(ctx, p.ID, "web-7"); err != nil {
		t.Fatal(err)
	}
	// Release queues one symbolicate job per event; run them.
	w := &jobs.Worker{Store: st, Handlers: map[string]jobs.Handler{
		"symbolicate": func(ctx context.Context, j sqlc.Job, args json.RawMessage) error {
			var a struct {
				Event sentry.ID `json:"event"`
				At    time.Time `json:"at"`
			}
			if err := json.Unmarshal(args, &a); err != nil {
				return err
			}
			return svc.Event(ctx, j.ProjectID, a.Event, a.At)
		},
	}}
	if n, err := w.RunOnce(ctx); err != nil || n != 2 {
		t.Fatalf("symbolicate jobs: %d %v", n, err)
	}
	for _, id := range []sentry.ID{id1, id2} {
		row, err := st.GetEvent(ctx, sqlc.GetEventParams{ProjectID: p.ID, EventID: id})
		if err != nil {
			t.Fatal(err)
		}
		if !row.Symbolicated || row.ErrorLocation == nil || *row.ErrorLocation != "a.js:3" {
			t.Errorf("event %s: symbolicated=%v location=%v", id, row.Symbolicated, row.ErrorLocation)
		}
	}
	var left int
	st.Pool.QueryRow(ctx, "SELECT count(*) FROM events WHERE project_id = $1 AND symbolicated = false", p.ID).Scan(&left)
	if left != 0 {
		t.Errorf("unsymbolicated left: %d", left)
	}
	// Both events share one fingerprint now: one issue with count 2.
	rows, _ := st.CountIssuesByStatus(ctx, p.ID)
	var total int64
	for _, r := range rows {
		total += r.N
	}
	if total != 1 {
		t.Errorf("issues = %+v", rows)
	}
}

func TestEventDSYM(t *testing.T) {
	st := testdb.New(t)
	ctx := context.Background()
	p := newProject(t, st)

	fake := &fakeSidecar{answer: func() any {
		return []map[string]any{
			{"function": "-[CartViewController loadCart:]", "filename": "/Users/dev/App/CartViewController.m", "lineno": 88},
			{"function": "??", "filename": "??", "lineno": 0},
		}
	}}
	sidecar := fake.server(t)
	svc := &Service{Store: st, DSYM: NewDSYMClient(sidecar.URL)}

	debugID := "AAAAAAAA-BBBB-CCCC-DDDD-EEEEEEEEEEEE"
	upload(t, st, p, KindDSYM, "2.0", debugID, "App.dSYM", []byte("MACHO-BYTES"))
	raw := fmt.Sprintf(`{"platform":"cocoa","release":"2.0","timestamp":%d,"level":"fatal",
"exception":{"values":[{"type":"EXC_BAD_ACCESS","value":"KERN_INVALID_ADDRESS","mechanism":{"type":"mach","handled":false},"stacktrace":{"frames":[
 {"instruction_addr":"0x1049e2b50","image_addr":"0x104900000","in_app":true},
 {"instruction_addr":"0x1049e3000","in_app":false}]}}]},
"debug_meta":{"images":[{"type":"macho","debug_id":"%s","code_file":"/private/var/containers/App.app/App","image_addr":"0x104900000","image_size":1048576}]}}`, time.Now().Unix(), debugID)
	id, oldFP, at := storeEvent(t, st, p, raw)

	if err := svc.Event(ctx, p.ID, id, at); err != nil {
		t.Fatal(err)
	}
	// Cold sidecar: asked, missed, sent the bytes once, asked again.
	if fake.puts != 1 || len(fake.posts) != 2 || fake.posts[0] != fake.posts[1] {
		t.Errorf("sidecar traffic: puts=%d posts=%v", fake.puts, fake.posts)
	}
	if got := fake.stored[fake.posts[0]]; string(got) != "MACHO-BYTES" {
		t.Errorf("sidecar received %q", got)
	}
	var addrs []struct{ Address, Module string }
	if err := json.Unmarshal([]byte(fake.frames), &addrs); err != nil || len(addrs) != 2 {
		t.Fatalf("frames = %s (%v)", fake.frames, err)
	}
	if addrs[0].Address != "0xe2b50" || addrs[0].Module != "App" || addrs[1].Address != "0xe3000" {
		t.Errorf("addresses = %+v", addrs)
	}
	row, err := st.GetEvent(ctx, sqlc.GetEventParams{ProjectID: p.ID, EventID: id})
	if err != nil {
		t.Fatal(err)
	}
	if !row.Symbolicated || row.Fingerprint == nil || *row.Fingerprint == oldFP {
		t.Fatalf("symbolicated=%v fp=%v", row.Symbolicated, row.Fingerprint)
	}
	var syms []sentry.Frame
	json.Unmarshal(row.Symbols, &syms)
	if len(syms) != 2 || syms[0].Function != "-[CartViewController loadCart:]" || syms[0].Filename != "CartViewController.m" || syms[0].Lineno != 88 || !syms[0].IsInApp() {
		t.Errorf("symbols[0] = %+v", syms)
	}
	if syms[1].Function != "" || syms[1].InstrAddr != "0x1049e3000" {
		t.Errorf("unresolved frame should be kept as-is: %+v", syms[1])
	}
	if row.ErrorLocation == nil || *row.ErrorLocation != "CartViewController.m:88" {
		t.Errorf("error_location = %v", deref(row.ErrorLocation))
	}
}

func TestEventDSYMSidecarErrorRetries(t *testing.T) {
	st := testdb.New(t)
	ctx := context.Background()
	p := newProject(t, st)
	sidecar := (&fakeSidecar{warm: true, answer: func() any { return http.StatusGatewayTimeout }}).server(t)
	svc := &Service{Store: st, DSYM: NewDSYMClient(sidecar.URL)}
	upload(t, st, p, KindDSYM, "2.0", "", "App.dSYM", []byte("x"))
	raw := fmt.Sprintf(`{"platform":"cocoa","release":"2.0","timestamp":%d,"exception":{"values":[{"type":"SIGSEGV","stacktrace":{"frames":[{"instruction_addr":"0x104900010"}]}}]},"debug_meta":{"images":[{"type":"macho","code_file":"App","image_addr":"0x104900000","image_size":4096}]}}`, time.Now().Unix())
	id, _, at := storeEvent(t, st, p, raw)
	if err := svc.Event(ctx, p.ID, id, at); err == nil {
		t.Fatal("sidecar failure must surface as an error so the job retries")
	}
	row, _ := st.GetEvent(ctx, sqlc.GetEventParams{ProjectID: p.ID, EventID: id})
	if row.Symbolicated {
		t.Fatal("must stay unsymbolicated")
	}
}

func ptr(s string) *string { return &s }

func deref(s *string) string {
	if s == nil {
		return "<nil>"
	}
	return *s
}

// TestResolveAtIngest: with a sidecar configured, native events are
// symbolicated inside Ingest (issue created on the resolved fingerprint, no
// symbolicate job); when the sidecar fails, the event is stored as-is and a
// symbolicate job is queued to retry.
func TestResolveAtIngest(t *testing.T) {
	st := testdb.New(t)
	ctx := context.Background()
	p := newProject(t, st)
	fail := false
	// warm: the sidecar already has the file (the cold case is TestResolveAtIngestColdSidecar).
	fake := &fakeSidecar{warm: true, answer: func() any {
		if fail {
			return http.StatusBadGateway
		}
		return []map[string]any{{"function": "-[Cart load]", "filename": "/src/Cart.m", "lineno": 7}}
	}}
	sidecar := fake.server(t)
	svc := &Service{Store: st, DSYM: NewDSYMClient(sidecar.URL)}
	in := &ingest.Ingester{Store: st, Cfg: config.Config{}, Symbols: svc, Log: slog.Default()}
	upload(t, st, p, KindDSYM, "2.0", "", "App.dSYM", []byte("x"))
	event := func(eid string) string {
		return fmt.Sprintf(`{"event_id":%q,"platform":"cocoa","release":"2.0","timestamp":%d,"level":"fatal","exception":{"values":[{"type":"SIGSEGV","mechanism":{"handled":false},"stacktrace":{"frames":[{"instruction_addr":"0x104900010","in_app":true}]}}]},"debug_meta":{"images":[{"type":"macho","code_file":"App","image_addr":"0x104900000","image_size":4096}]}}`, eid, time.Now().Unix())
	}
	envelope := func(body string) []byte {
		return []byte(fmt.Sprintf("{}\n{\"type\":\"event\",\"length\":%d}\n%s\n", len(body), body))
	}
	now := time.Now().UTC()
	res, err := in.Ingest(ctx, p, sentry.Parse(envelope(event("c1c1c1c1c1c1c1c1c1c1c1c1c1c1c1c1")), now), now)
	if err != nil {
		t.Fatal(err)
	}
	row, err := st.GetEvent(ctx, sqlc.GetEventParams{ProjectID: p.ID, EventID: "c1c1c1c1c1c1c1c1c1c1c1c1c1c1c1c1"})
	if err != nil || !row.Symbolicated || row.ErrorLocation == nil || *row.ErrorLocation != "Cart.m:7" {
		t.Fatalf("stored symbolicated at ingest: %+v %v", row, err)
	}
	if res.Jobs != 1 { // the new_issue alert only
		t.Errorf("jobs enqueued = %d, want 1 (alert)", res.Jobs)
	}
	is, err := st.GetIssue(ctx, sqlc.GetIssueParams{ProjectID: p.ID, Fingerprint: *row.Fingerprint})
	if err != nil || is.EventCount != 1 {
		t.Errorf("issue on the symbolicated fingerprint: %+v %v", is, err)
	}

	fail = true
	res, err = in.Ingest(ctx, p, sentry.Parse(envelope(event("c2c2c2c2c2c2c2c2c2c2c2c2c2c2c2c2")), now), now)
	if err != nil {
		t.Fatal(err)
	}
	row, _ = st.GetEvent(ctx, sqlc.GetEventParams{ProjectID: p.ID, EventID: "c2c2c2c2c2c2c2c2c2c2c2c2c2c2c2c2"})
	if row.Symbolicated {
		t.Fatal("must be stored unsymbolicated when the sidecar fails")
	}
	var kinds []string
	rows, _ := st.Pool.Query(ctx, "SELECT kind FROM jobs WHERE project_id = $1 ORDER BY id", p.ID)
	for rows.Next() {
		var k string
		rows.Scan(&k)
		kinds = append(kinds, k)
	}
	rows.Close()
	if len(kinds) != 3 || kinds[2] != "symbolicate" {
		t.Errorf("jobs = %v, want [alert alert symbolicate]", kinds)
	}
}

// TestResolveAtIngestColdSidecar: when the sidecar does not have the
// symbol file yet, ingest does not read the bytes from the database inside
// the request — the event is stored as-is with a symbolicate job, and the
// job sends the file once and resolves.
func TestResolveAtIngestColdSidecar(t *testing.T) {
	st := testdb.New(t)
	ctx := context.Background()
	p := newProject(t, st)
	fake := &fakeSidecar{answer: func() any {
		return []map[string]any{{"function": "-[Cart load]", "filename": "/src/Cart.m", "lineno": 7}}
	}}
	sidecar := fake.server(t)
	svc := &Service{Store: st, DSYM: NewDSYMClient(sidecar.URL)}
	in := &ingest.Ingester{Store: st, Cfg: config.Config{}, Symbols: svc, Log: slog.Default()}
	upload(t, st, p, KindDSYM, "2.0", "", "App.dSYM", []byte("MACHO"))
	body := fmt.Sprintf(`{"event_id":"d1d1d1d1d1d1d1d1d1d1d1d1d1d1d1d1","platform":"cocoa","release":"2.0","timestamp":%d,"level":"fatal","exception":{"values":[{"type":"SIGSEGV","mechanism":{"handled":false},"stacktrace":{"frames":[{"instruction_addr":"0x104900010","in_app":true}]}}]},"debug_meta":{"images":[{"type":"macho","code_file":"App","image_addr":"0x104900000","image_size":4096}]}}`, time.Now().Unix())
	envelope := []byte(fmt.Sprintf("{}\n{\"type\":\"event\",\"length\":%d}\n%s\n", len(body), body))
	now := time.Now().UTC()
	res, err := in.Ingest(ctx, p, sentry.Parse(envelope, now), now)
	if err != nil {
		t.Fatal(err)
	}
	if fake.puts != 0 || len(fake.posts) != 1 {
		t.Fatalf("ingest must only ask, not upload: puts=%d posts=%v", fake.puts, fake.posts)
	}
	row, err := st.GetEvent(ctx, sqlc.GetEventParams{ProjectID: p.ID, EventID: "d1d1d1d1d1d1d1d1d1d1d1d1d1d1d1d1"})
	if err != nil || row.Symbolicated {
		t.Fatalf("stored as-is: %+v %v", row, err)
	}
	if res.Jobs != 2 { // new_issue alert + symbolicate
		t.Fatalf("jobs = %d, want 2", res.Jobs)
	}
	// The job sends the file and resolves.
	if err := svc.Event(ctx, p.ID, row.EventID, row.OccurredAt); err != nil {
		t.Fatal(err)
	}
	if fake.puts != 1 || string(fake.stored[fake.posts[len(fake.posts)-1]]) != "MACHO" {
		t.Fatalf("job upload: puts=%d stored=%v", fake.puts, fake.stored)
	}
	row, _ = st.GetEvent(ctx, sqlc.GetEventParams{ProjectID: p.ID, EventID: row.EventID})
	if !row.Symbolicated || row.ErrorLocation == nil || *row.ErrorLocation != "Cart.m:7" {
		t.Fatalf("after job: %+v", row)
	}
}
