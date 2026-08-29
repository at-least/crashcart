package symbolicate

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/at-least/crashcart/internal/db/sqlc"
	"github.com/at-least/crashcart/internal/pk"
	"github.com/at-least/crashcart/internal/sentry"
	"github.com/at-least/crashcart/internal/store"
	"github.com/at-least/crashcart/internal/testdb"
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
// does, from a raw Sentry payload. Returns the event id and fingerprint.
func storeEvent(t *testing.T, st *store.Store, p sqlc.Project, raw string) (int64, string) {
	t.Helper()
	ctx := context.Background()
	now := time.Now().UTC()
	ev := sentry.ParseEvent("", now, []byte(raw), now)
	if ev == nil {
		t.Fatal("bad payload")
	}
	fp := sentry.Fingerprint(ev, ev.Frames())
	id := pk.New(ev.Timestamp)
	err := st.Tx(ctx, func(ctx context.Context, tx pgx.Tx, q *sqlc.Queries) error {
		if _, err := q.UpsertIssue(ctx, sqlc.UpsertIssueParams{
			ProjectID: p.ID, Fingerprint: fp, Title: ev.IssueTitle(), Level: ev.Level, EventCount: 1, StoredCount: 1,
			FirstSeen: id, LastSeen: id, FirstRelease: nilIfEmpty(ev.Release), Platform: nilIfEmpty(ev.Platform),
		}); err != nil {
			return err
		}
		return store.InsertEvents(ctx, tx, []store.EventInsert{{
			ID: id, ProjectID: p.ID, EventID: ev.EventID, Level: ev.Level, Message: ev.Message, Platform: nilIfEmpty(ev.Platform),
			Release: nilIfEmpty(ev.Release), ErrorType: nilIfEmpty(ev.ErrorType), Fingerprint: &fp,
			Tags: []byte("{}"), Breadcrumbs: []byte("[]"), Payload: ev.Raw,
		}})
	})
	if err != nil {
		t.Fatal(err)
	}
	return id, fp
}

func upload(t *testing.T, st *store.Store, p sqlc.Project, kind, release, debugID, filename string, data []byte) {
	t.Helper()
	var did *string
	if debugID != "" {
		did = &debugID
	}
	_, err := st.UpsertSymbolFile(context.Background(), sqlc.UpsertSymbolFileParams{
		ProjectID: p.ID, Kind: kind, Release: release, DebugID: did, Filename: filename, Size: int64(len(data)), Data: data,
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
	row, _ := st.GetEvent(ctx, sqlc.GetEventParams{ProjectID: p.ID, ID: id})
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
	row, err := st.GetEvent(ctx, sqlc.GetEventParams{ProjectID: p.ID, ID: id})
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
	if err != nil || ni.EventCount != 1 || ni.StoredCount != 1 || ni.FirstSeen != id {
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
	if err := svc.Event(ctx, p.ID, id+1); err != nil {
		t.Fatal(err)
	}
}

func TestReleaseSourceMap(t *testing.T) {
	st := testdb.New(t)
	ctx := context.Background()
	p := newProject(t, st)
	svc := &Service{Store: st}
	raw := fmt.Sprintf(`{"platform":"javascript","release":"web-7","timestamp":%d,"exception":{"values":[{"type":"TypeError","value":"x is undefined","stacktrace":{"frames":[{"filename":"https://app.example.com/static/bundle.min.js","function":"t","lineno":1,"colno":13,"in_app":true}]}}]}}`, time.Now().Unix())
	id1, _ := storeEvent(t, st, p, raw)
	id2, _ := storeEvent(t, st, p, raw)
	if id1 == id2 {
		t.Skip("id collision")
	}
	// Negative-cache the release first, as ingest would have.
	now := time.Now().UTC()
	svc.Inline(ctx, p.ID, sentry.ParseEvent("", now, []byte(raw), now))
	upload(t, st, p, KindSourceMap, "web-7", "", "bundle.min.js.map", []byte(`{"version":3,"sources":["src/a.js"],"names":["foo"],"mappings":"AAAAA,UAEI"}`))

	if err := svc.Release(ctx, p.ID, "web-7"); err != nil {
		t.Fatal(err)
	}
	for _, id := range []int64{id1, id2} {
		row, err := st.GetEvent(ctx, sqlc.GetEventParams{ProjectID: p.ID, ID: id})
		if err != nil {
			t.Fatal(err)
		}
		if !row.Symbolicated || row.ErrorLocation == nil || *row.ErrorLocation != "a.js:3" {
			t.Errorf("event %d: symbolicated=%v location=%v", id, row.Symbolicated, row.ErrorLocation)
		}
	}
	left, _ := st.UnsymbolicatedEvents(ctx, sqlc.UnsymbolicatedEventsParams{ProjectID: p.ID, Release: ptr("web-7"), ID: 0, Limit: 10})
	if len(left) != 0 {
		t.Errorf("unsymbolicated left: %v", left)
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
	row, err := st.GetEvent(ctx, sqlc.GetEventParams{ProjectID: p.ID, ID: id})
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
	row, _ := st.GetEvent(ctx, sqlc.GetEventParams{ProjectID: p.ID, ID: id})
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
