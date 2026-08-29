package symbolicate

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
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
func storeEvent(t *testing.T, st *store.Store, p sqlc.Project, raw string) (sentry.ID, sentry.ID) {
	t.Helper()
	ctx := context.Background()
	now := time.Now().UTC()
	ev := sentry.ParseEvent("", now, []byte(raw), now)
	if ev == nil {
		t.Fatal("bad payload")
	}
	fp := sentry.Fingerprint(ev, ev.Frames())
	id := ev.EventID
	at := ev.Timestamp.UTC()
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
			Tags: []byte("{}"), Payload: ev.Raw,
		}})
	})
	if err != nil {
		t.Fatal(err)
	}
	return id, fp
}

func storeEventAt(t *testing.T, st *store.Store, p sqlc.Project, raw string) (sentry.ID, time.Time) {
	t.Helper()
	now := time.Now().UTC()
	ev := sentry.ParseEvent("", now, []byte(raw), now)
	id, _ := storeEvent(t, st, p, raw)
	return id, ev.Timestamp.UTC()
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
	id, oldFP := storeEvent(t, st, p, raw)

	// No mapping yet: not an error, event stays unsymbolicated.
	if err := svc.Event(ctx, p.ID, id); err != nil {
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

	if err := svc.Event(ctx, p.ID, id); err != nil {
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
	if err := svc.Event(ctx, p.ID, id); err != nil {
		t.Fatal(err)
	}
	// Missing event is not an error.
	if err := svc.Event(ctx, p.ID, sentry.DerivedID([]byte("missing"))); err != nil {
		t.Fatal(err)
	}
}

func TestReleaseSourceMap(t *testing.T) {
	st := testdb.New(t)
	ctx := context.Background()
	p := newProject(t, st)
	svc := &Service{Store: st}
	raw := fmt.Sprintf(`{"event_id":"%s","platform":"javascript","release":"web-7","timestamp":%d,"exception":{"values":[{"type":"TypeError","value":"x is undefined","stacktrace":{"frames":[{"filename":"https://app.example.com/static/bundle.min.js","function":"t","lineno":1,"colno":13,"in_app":true}]}}]}}`, "%s", time.Now().Unix())
	id1, _ := storeEvent(t, st, p, fmt.Sprintf(raw, "b1b1b1b1b1b1b1b1b1b1b1b1b1b1b1b1"))
	id2, _ := storeEvent(t, st, p, fmt.Sprintf(raw, "b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2"))
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
			}
			if err := json.Unmarshal(args, &a); err != nil {
				return err
			}
			return svc.Event(ctx, j.ProjectID, a.Event)
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

	var gotFrames string
	var gotBody int
	sidecar := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotFrames = r.Header.Get("X-Frames")
		gotBody = int(r.ContentLength)
		json.NewEncoder(w).Encode(map[string]any{"frames": []map[string]any{
			{"function": "-[CartViewController loadCart:]", "filename": "/Users/dev/App/CartViewController.m", "lineno": 88},
			{"function": "??", "filename": "??", "lineno": 0},
		}})
	}))
	defer sidecar.Close()
	svc := &Service{Store: st, DSYM: NewDSYMClient(sidecar.URL)}

	debugID := "AAAAAAAA-BBBB-CCCC-DDDD-EEEEEEEEEEEE"
	upload(t, st, p, KindDSYM, "2.0", debugID, "App.dSYM", []byte("MACHO-BYTES"))
	raw := fmt.Sprintf(`{"platform":"cocoa","release":"2.0","timestamp":%d,"level":"fatal",
"exception":{"values":[{"type":"EXC_BAD_ACCESS","value":"KERN_INVALID_ADDRESS","mechanism":{"type":"mach","handled":false},"stacktrace":{"frames":[
 {"instruction_addr":"0x1049e2b50","image_addr":"0x104900000","in_app":true},
 {"instruction_addr":"0x1049e3000","in_app":false}]}}]},
"debug_meta":{"images":[{"type":"macho","debug_id":"%s","code_file":"/private/var/containers/App.app/App","image_addr":"0x104900000","image_size":1048576}]}}`, time.Now().Unix(), debugID)
	id, oldFP := storeEvent(t, st, p, raw)

	if err := svc.Event(ctx, p.ID, id); err != nil {
		t.Fatal(err)
	}
	if gotBody != len("MACHO-BYTES") {
		t.Errorf("sidecar body length = %d", gotBody)
	}
	var addrs []struct{ Address, Module string }
	if err := json.Unmarshal([]byte(gotFrames), &addrs); err != nil || len(addrs) != 2 {
		t.Fatalf("X-Frames = %s (%v)", gotFrames, err)
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
	sidecar := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "llvm-symbolizer timed out", http.StatusGatewayTimeout)
	}))
	defer sidecar.Close()
	svc := &Service{Store: st, DSYM: NewDSYMClient(sidecar.URL)}
	upload(t, st, p, KindDSYM, "2.0", "", "App.dSYM", []byte("x"))
	raw := fmt.Sprintf(`{"platform":"cocoa","release":"2.0","timestamp":%d,"exception":{"values":[{"type":"SIGSEGV","stacktrace":{"frames":[{"instruction_addr":"0x104900010"}]}}]},"debug_meta":{"images":[{"type":"macho","code_file":"App","image_addr":"0x104900000","image_size":4096}]}}`, time.Now().Unix())
	id, _ := storeEvent(t, st, p, raw)
	if err := svc.Event(ctx, p.ID, id); err == nil {
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
	sidecar := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if fail {
			http.Error(w, "down", http.StatusBadGateway)
			return
		}
		json.NewEncoder(w).Encode(map[string]any{"frames": []map[string]any{{"function": "-[Cart load]", "filename": "/src/Cart.m", "lineno": 7}}})
	}))
	defer sidecar.Close()
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
