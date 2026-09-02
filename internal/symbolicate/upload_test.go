package symbolicate

import (
	"archive/zip"
	"bytes"
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/at-least/crashcart/internal/db/sqlc"
	"github.com/at-least/crashcart/internal/testdb"
)

func TestUpload(t *testing.T) {
	st := testdb.New(t)
	ctx := context.Background()
	p, err := st.CreateProject(ctx, sqlc.CreateProjectParams{Slug: "ios", Name: "iOS", PublicKey: "k"})
	if err != nil {
		t.Fatal(err)
	}
	s := &Service{Store: st, DSYM: NewDSYMClient("")}

	// A dSYM binary gets its LC_UUID as debug_id.
	rows, err := s.Upload(ctx, p.ID, "1.0", "", "App", fakeMachO(0x0100000c, uuidA))
	if err != nil || len(rows) != 1 || rows[0].Kind != KindDSYM || rows[0].DebugID == nil || *rows[0].DebugID != "12345678-9abc-def0-1122-334455667788" {
		t.Fatalf("dsym upload: %+v %v", rows, err)
	}
	if f, err := st.SymbolFileByDebugID(ctx, sqlc.SymbolFileByDebugIDParams{ProjectID: p.ID, DebugID: rows[0].DebugID}); err != nil || f.Filename != "App" {
		t.Fatalf("lookup by debug id: %+v %v", f, err)
	}
	// Without a release (sentry-cli debug-files upload) every build's dSYM
	// is named after the binary: the rows must not replace each other.
	for _, u := range [][]byte{uuidA, uuidB, uuidB} {
		if _, err := s.Upload(ctx, p.ID, "", "", "App", fakeMachO(0x0100000c, u)); err != nil {
			t.Fatal(err)
		}
	}
	var n int
	st.Pool.QueryRow(ctx, "SELECT count(*) FROM symbol_files WHERE kind = 'dsym' AND release IS NULL").Scan(&n)
	if n != 2 {
		t.Fatalf("release-less dSYM rows = %d, want one per build", n)
	}
	// A zip that unpacks past the total bound is refused before anything is stored.
	var zb bytes.Buffer
	bigZip := zip.NewWriter(&zb)
	for i := range 5 {
		w, _ := bigZip.Create(fmt.Sprintf("m%d.txt", i))
		w.Write(bytes.Repeat([]byte("com.example.A -> a:\n"), (MaxZipTotal/5+1)/20+1))
	}
	bigZip.Close()
	if _, err := s.Upload(ctx, p.ID, "9.9", "", "maps.zip", zb.Bytes()); err == nil || !strings.Contains(err.Error(), "unpacks") {
		t.Fatalf("oversized zip: %v", err)
	}
	if n, _ := st.SymbolFileExists(ctx, sqlc.SymbolFileExistsParams{ProjectID: p.ID, Kind: "proguard", Release: "9.9"}); n {
		t.Fatal("a refused zip must store nothing")
	}
	if n, _ := st.CountJobs(ctx); n != 1 {
		t.Fatalf("resymbolicate job expected, jobs = %d", n)
	}

	// A zip stores each entry with per-file kind detection; junk is skipped.
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	w, _ := zw.Create("proguard/0f1e2d3c-4b5a-6978-8a9b-0c1d2e3f4a5b.txt")
	w.Write([]byte("com.example.Foo -> a.b:\n    void bar() -> c\n"))
	w, _ = zw.Create("__MACOSX/._junk")
	w.Write([]byte("x"))
	w, _ = zw.Create("bundle.js.map")
	w.Write([]byte(`{"version":3,"sources":["a.js"],"names":[],"mappings":"AAAA"}`))
	zw.Close()
	rows, err = s.Upload(ctx, p.ID, "1.0", "", "symbols.zip", buf.Bytes())
	if err != nil || len(rows) != 2 {
		t.Fatalf("zip upload: %d rows %v", len(rows), err)
	}
	kinds := map[string]string{}
	for _, r := range rows {
		kinds[r.Filename] = string(r.Kind)
		if r.Kind == KindProGuard && (r.DebugID == nil || *r.DebugID != "0f1e2d3c-4b5a-6978-8a9b-0c1d2e3f4a5b") {
			t.Fatalf("proguard debug id: %+v", r)
		}
	}
	if kinds["proguard/0f1e2d3c-4b5a-6978-8a9b-0c1d2e3f4a5b.txt"] != KindProGuard || kinds["bundle.js.map"] != KindSourceMap {
		t.Fatalf("kinds = %v", kinds)
	}

	// Caller mistakes are UploadError.
	if _, err := s.Upload(ctx, p.ID, "1.0", "bogus", "x", []byte("x")); err == nil {
		t.Fatal("bad kind accepted")
	} else if _, ok := err.(UploadError); !ok {
		t.Fatalf("want UploadError, got %T", err)
	}
	if _, err := s.Upload(ctx, p.ID, "1.0", "", "x", nil); err == nil {
		t.Fatal("empty file accepted")
	}
}
