package symbolicate

import (
	"archive/zip"
	"bytes"
	"context"
	"testing"

	"github.com/crashcartapp/crashcart/internal/db/sqlc"
	"github.com/crashcartapp/crashcart/internal/testdb"
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
