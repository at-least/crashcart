package symbolicate

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/at-least/crashcart/internal/config"
	"github.com/at-least/crashcart/internal/db/sqlc"
	"github.com/at-least/crashcart/internal/ingest"
	"github.com/at-least/crashcart/internal/sentry"
	"github.com/at-least/crashcart/internal/testdb"
)

// TestEventWrongTimeIsNoop: the symbolicate job addresses the event by its
// primary key including occurred_at; a job whose `at` does not match finds
// nothing and returns nil (as for a dropped event) — the event stays as is.
func TestEventWrongTimeIsNoop(t *testing.T) {
	st := testdb.New(t)
	ctx := context.Background()
	p := newProject(t, st)
	svc := &Service{Store: st}
	uuid := "11111111-2222-3333-4444-555555555555"
	upload(t, st, p, KindProGuard, "1.0", uuid, "mapping.txt", []byte(mappingTxt))
	id, _, at := storeEvent(t, st, p, fmt.Sprintf(androidEvent, time.Now().Unix(), uuid, uuid))
	if err := svc.Event(ctx, p.ID, id, at.Add(time.Second)); err != nil {
		t.Fatalf("wrong at: %v", err)
	}
	if row, _ := st.GetEvent(ctx, sqlc.GetEventParams{ProjectID: p.ID, EventID: id}); row.Symbolicated {
		t.Fatal("symbolicated through a job with the wrong time")
	}
	if err := svc.Event(ctx, p.ID, id, at); err != nil {
		t.Fatal(err)
	}
	if row, _ := st.GetEvent(ctx, sqlc.GetEventParams{ProjectID: p.ID, EventID: id}); !row.Symbolicated {
		t.Fatal("not symbolicated with the right time")
	}
}

// TestSymbolicationWritesOnlySmallColumns: the payload bytes are untouched,
// the hour is re-marked dirty, and the event's counts move from the old
// issue to the new one (the old issue keeps its other event).
func TestSymbolicationWritesOnlySmallColumns(t *testing.T) {
	st := testdb.New(t)
	ctx := context.Background()
	p := newProject(t, st)
	svc := &Service{Store: st}
	uuid := "11111111-2222-3333-4444-555555555555"
	upload(t, st, p, KindProGuard, "1.0", uuid, "mapping.txt", []byte(mappingTxt))
	ts := time.Now().Unix()
	id1, oldFP, at1 := storeEvent(t, st, p, fmt.Sprintf(androidEvent, ts, uuid, uuid))
	id2, fp2, _ := storeEvent(t, st, p, strings.Replace(fmt.Sprintf(androidEvent, ts, uuid, uuid), `"event_id":"a1"`, `"event_id":"a2"`, 1))
	if fp2 != oldFP {
		t.Fatalf("same stack, different fingerprints: %s %s", fp2, oldFP)
	}
	var before []byte
	if err := st.Pool.QueryRow(ctx, "SELECT payload FROM events WHERE project_id = $1 AND event_id = $2", p.ID, id1).Scan(&before); err != nil {
		t.Fatal(err)
	}
	if _, err := st.Pool.Exec(ctx, "DELETE FROM event_stats_dirty WHERE project_id = $1", p.ID); err != nil {
		t.Fatal(err)
	}
	if err := svc.Event(ctx, p.ID, id1, at1); err != nil {
		t.Fatal(err)
	}
	var after []byte
	var symbolicated bool
	if err := st.Pool.QueryRow(ctx, "SELECT payload, symbolicated FROM events WHERE project_id = $1 AND event_id = $2", p.ID, id1).Scan(&after, &symbolicated); err != nil {
		t.Fatal(err)
	}
	if !symbolicated || !bytes.Equal(before, after) {
		t.Fatalf("payload rewritten (symbolicated=%v, same bytes=%v)", symbolicated, bytes.Equal(before, after))
	}
	var dirty int
	if err := st.Pool.QueryRow(ctx, "SELECT count(*) FROM event_stats_dirty WHERE project_id = $1 AND bucket = $2", p.ID, at1.UTC().Truncate(time.Hour)).Scan(&dirty); err != nil || dirty != 1 {
		t.Fatalf("hour not re-marked dirty: %d %v", dirty, err)
	}
	old, err := st.GetIssue(ctx, sqlc.GetIssueParams{ProjectID: p.ID, Fingerprint: oldFP})
	if err != nil || old.EventCount != 1 || old.StoredCount != 1 {
		t.Fatalf("old issue after the move: %+v %v (want 1/1, it still has %s)", old, err, id2)
	}
	row, _ := st.GetEvent(ctx, sqlc.GetEventParams{ProjectID: p.ID, EventID: id1})
	nw, err := st.GetIssue(ctx, sqlc.GetIssueParams{ProjectID: p.ID, Fingerprint: *row.Fingerprint})
	if err != nil || nw.EventCount != 1 || nw.StoredCount != 1 {
		t.Fatalf("new issue: %+v %v", nw, err)
	}
}

// TestReleaseQueuesUnsymbolicatedNewestFirst: the fan-out queues one job per
// unsymbolicated event of the release only — not symbolicated ones, not
// other releases — newest first and bounded.
func TestReleaseQueuesUnsymbolicatedNewestFirst(t *testing.T) {
	st := testdb.New(t)
	ctx := context.Background()
	p := newProject(t, st)
	base := time.Now().Unix() - 100
	ev := func(id, release string, ts int64) string {
		return fmt.Sprintf(`{"event_id":"%s","platform":"javascript","release":"%s","timestamp":%d,"exception":{"values":[{"type":"TypeError","value":"x","stacktrace":{"frames":[{"filename":"https://a/b.js","function":"t","lineno":1,"colno":13,"in_app":true}]}}]}}`, id, release, ts)
	}
	ids := []sentry.ID{}
	for i, id := range []string{"e1e1e1e1e1e1e1e1e1e1e1e1e1e1e1e1", "e2e2e2e2e2e2e2e2e2e2e2e2e2e2e2e2", "e3e3e3e3e3e3e3e3e3e3e3e3e3e3e3e3"} {
		got, _, _ := storeEvent(t, st, p, ev(id, "web-1", base+int64(i)))
		ids = append(ids, got)
	}
	other, _, _ := storeEvent(t, st, p, ev("e4e4e4e4e4e4e4e4e4e4e4e4e4e4e4e4", "web-2", base+10))
	if _, err := st.Pool.Exec(ctx, "UPDATE events SET symbolicated = true WHERE project_id = $1 AND event_id = $2", p.ID, ids[2]); err != nil {
		t.Fatal(err)
	}
	rel := "web-1"
	n, err := st.EnqueueSymbolicateRelease(ctx, sqlc.EnqueueSymbolicateReleaseParams{ProjectID: p.ID, Release: &rel, Limit: 1})
	if err != nil || n != 1 {
		t.Fatalf("limit 1: %d %v", n, err)
	}
	queued := func() []string {
		var out []string
		rows, _ := st.Pool.Query(ctx, "SELECT args->>'event' FROM jobs WHERE project_id = $1 AND kind = 'symbolicate' ORDER BY id", p.ID)
		for rows.Next() {
			var e string
			rows.Scan(&e)
			out = append(out, e)
		}
		rows.Close()
		return out
	}
	if got := queued(); len(got) != 1 || got[0] != string(ids[1]) {
		t.Fatalf("limit 1 queued %v, want the newest unsymbolicated %s", got, ids[1])
	}
	n, err = st.EnqueueSymbolicateRelease(ctx, sqlc.EnqueueSymbolicateReleaseParams{ProjectID: p.ID, Release: &rel, Limit: ReleaseMax})
	if err != nil {
		t.Fatal(err)
	}
	got := queued()
	if len(got) != 2 || got[0] != string(ids[1]) || got[1] != string(ids[0]) {
		t.Fatalf("queued %v, want [%s %s] (symbolicated %s and other release %s excluded)", got, ids[1], ids[0], ids[2], other)
	}
	// The queued 'at' addresses the event's row exactly.
	var at time.Time
	if err := st.Pool.QueryRow(ctx, "SELECT (args->>'at')::timestamptz FROM jobs WHERE project_id = $1 AND args->>'event' = $2", p.ID, string(ids[0])).Scan(&at); err != nil {
		t.Fatal(err)
	}
	if _, err := st.GetEventAt(ctx, sqlc.GetEventAtParams{ProjectID: p.ID, EventID: ids[0], OccurredAt: at}); err != nil {
		t.Fatalf("job 'at' does not address the event: %v", err)
	}
}

// TestResolveAtIngestBudget: a sidecar slower than SymbolicateBudget leaves
// the event stored unsymbolicated with a symbolicate job, and the ingest
// does not wait for it.
func TestResolveAtIngestBudget(t *testing.T) {
	st := testdb.New(t)
	ctx := context.Background()
	p := newProject(t, st)
	fake := &fakeSidecar{warm: true, answer: func() any {
		time.Sleep(ingest.SymbolicateBudget + 2*time.Second)
		return []map[string]any{{"function": "-[Cart load]", "filename": "/src/Cart.m", "lineno": 7}}
	}}
	sidecar := fake.server(t)
	svc := &Service{Store: st, DSYM: NewDSYMClient(sidecar.URL)}
	in := &ingest.Ingester{Store: st, Cfg: config.Config{}, Symbols: svc, Log: slog.Default()}
	upload(t, st, p, KindDSYM, "2.0", "", "App.dSYM", []byte("x"))
	body := fmt.Sprintf(`{"event_id":"f1f1f1f1f1f1f1f1f1f1f1f1f1f1f1f1","platform":"cocoa","release":"2.0","timestamp":%d,"level":"fatal","exception":{"values":[{"type":"SIGSEGV","mechanism":{"handled":false},"stacktrace":{"frames":[{"instruction_addr":"0x104900010","in_app":true}]}}]},"debug_meta":{"images":[{"type":"macho","code_file":"App","image_addr":"0x104900000","image_size":4096}]}}`, time.Now().Unix())
	envelope := []byte(fmt.Sprintf("{}\n{\"type\":\"event\",\"length\":%d}\n%s\n", len(body), body))
	now := time.Now().UTC()
	start := time.Now()
	res, err := in.Ingest(ctx, p, sentry.Parse(envelope, now), now)
	if err != nil {
		t.Fatal(err)
	}
	if took := time.Since(start); took > ingest.SymbolicateBudget+time.Second {
		t.Fatalf("ingest waited %v for the sidecar (budget %v)", took, ingest.SymbolicateBudget)
	}
	row, err := st.GetEvent(ctx, sqlc.GetEventParams{ProjectID: p.ID, EventID: "f1f1f1f1f1f1f1f1f1f1f1f1f1f1f1f1"})
	if err != nil || row.Symbolicated {
		t.Fatalf("stored as-is: %+v %v", row, err)
	}
	var kinds []string
	rows, _ := st.Pool.Query(ctx, "SELECT kind FROM jobs WHERE project_id = $1 ORDER BY id", p.ID)
	for rows.Next() {
		var k string
		rows.Scan(&k)
		kinds = append(kinds, k)
	}
	rows.Close()
	if res.Jobs != 2 || len(kinds) != 2 || kinds[1] != "symbolicate" {
		t.Fatalf("jobs = %v (res %d), want [alert symbolicate]", kinds, res.Jobs)
	}
}

// TestSymbolFileReuploadIsOneRowNewKey: (project, kind, release, filename)
// is unique with NULL release treated as a value, so a re-upload replaces
// the row (same id, newer uploaded_at) and the sidecar key changes; a
// different release with the same filename is another row.
func TestSymbolFileReuploadIsOneRowNewKey(t *testing.T) {
	st := testdb.New(t)
	ctx := context.Background()
	p := newProject(t, st)
	first, err := st.UpsertSymbolFile(ctx, sqlc.UpsertSymbolFileParams{ProjectID: p.ID, Kind: KindDSYM, Release: nil, DebugID: nil, Filename: "App.dSYM", Size: 1, Data: []byte("a")})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.Pool.Exec(ctx, "UPDATE symbol_files SET uploaded_at = uploaded_at - interval '1 minute' WHERE id = $1", first.ID); err != nil {
		t.Fatal(err)
	}
	firstAt := first.UploadedAt
	if err := st.Pool.QueryRow(ctx, "SELECT uploaded_at FROM symbol_files WHERE id = $1", first.ID).Scan(&firstAt); err != nil {
		t.Fatal(err)
	}
	did := "AAAAAAAA-BBBB-CCCC-DDDD-EEEEEEEEEEEE"
	second, err := st.UpsertSymbolFile(ctx, sqlc.UpsertSymbolFileParams{ProjectID: p.ID, Kind: KindDSYM, Release: nil, DebugID: &did, Filename: "App.dSYM", Size: 2, Data: []byte("bb")})
	if err != nil {
		t.Fatal(err)
	}
	if second.ID != first.ID || !second.UploadedAt.After(firstAt) || second.DebugID == nil || second.Size != 2 {
		t.Fatalf("re-upload: first=%+v at %v second=%+v", first, firstAt, second)
	}
	if SymbolKey(first.ID, firstAt) == SymbolKey(second.ID, second.UploadedAt) {
		t.Fatal("re-upload kept the sidecar key")
	}
	data, _ := st.SymbolFileData(ctx, second.ID)
	if string(data) != "bb" {
		t.Fatalf("data = %q", data)
	}
	rel := "2.0"
	third, err := st.UpsertSymbolFile(ctx, sqlc.UpsertSymbolFileParams{ProjectID: p.ID, Kind: KindDSYM, Release: &rel, Filename: "App.dSYM", Size: 1, Data: []byte("c")})
	if err != nil || third.ID == second.ID {
		t.Fatalf("release-scoped upload: %+v %v", third, err)
	}
	all, _ := st.ListSymbolFiles(ctx, p.ID)
	if len(all) != 2 {
		t.Fatalf("symbol_files = %d, want 2", len(all))
	}
}

// TestInlineMappingIsCached: after the first load, a ProGuard mapping is
// served from memory — deleting the row does not change the result until
// the cache is invalidated.
func TestInlineMappingIsCached(t *testing.T) {
	st := testdb.New(t)
	ctx := context.Background()
	p := newProject(t, st)
	svc := &Service{Store: st}
	uuid := "11111111-2222-3333-4444-555555555555"
	upload(t, st, p, KindProGuard, "1.0", uuid, "mapping.txt", []byte(mappingTxt))
	now := time.Now().UTC()
	ev := sentry.ParseEvent("", now, []byte(fmt.Sprintf(androidEvent, now.Unix(), uuid, uuid)), now)
	if _, ok, err := svc.Inline(ctx, p.ID, ev); !ok || err != nil {
		t.Fatalf("first resolve: %v %v", ok, err)
	}
	if _, err := st.Pool.Exec(ctx, "DELETE FROM symbol_files WHERE project_id = $1", p.ID); err != nil {
		t.Fatal(err)
	}
	if _, ok, err := svc.Inline(ctx, p.ID, ev); !ok || err != nil {
		t.Fatalf("cached resolve after the row is gone: %v %v", ok, err)
	}
	svc.Invalidate(p.ID, "1.0")
	if _, ok, _ := svc.Inline(ctx, p.ID, ev); ok {
		t.Fatal("resolved with no mapping after Invalidate")
	}
}
