package symbolicate

import (
	"encoding/json"
	"testing"

	"github.com/crashcartapp/crashcart/internal/sentry"
)

const mappingTxt = `# compiler: R8
com.example.CartFragment -> a.b.c:
    void onCreateView(android.os.Bundle) -> a
    12:15:void loadCart(java.lang.String):45:48 -> b
com.example.Api -> a.d:
    fetch() -> x
`

func TestProGuard(t *testing.T) {
	m := ParseProGuard(mappingTxt)
	if m.Classes["a.b.c"] != "com.example.CartFragment" || m.Classes["a.d"] != "com.example.Api" {
		t.Fatalf("classes = %v", m.Classes)
	}
	in := true
	f := m.Resolve(Frame{Module: "a.b.c", Function: "b", Filename: "SourceFile", Lineno: 13, InApp: &in})
	if f.Module != "com.example.CartFragment" || f.Function != "loadCart" || f.Lineno != 46 || f.Filename != "CartFragment.java" || !f.IsInApp() {
		t.Errorf("method resolve = %+v", f)
	}
	f = m.Resolve(Frame{Filename: "a.d", Function: "x"})
	if f.Module != "com.example.Api" || f.Function != "fetch" || f.Filename != "a.d" {
		t.Errorf("simple method (class in filename) = %+v", f)
	}
	f = m.Resolve(Frame{Module: "a.b.c", Function: "unknown"})
	if f.Module != "com.example.CartFragment" || f.Function != "unknown" {
		t.Errorf("class-only = %+v", f)
	}
	f = m.Resolve(Frame{Module: "zzz", Function: "q"})
	if f.Module != "zzz" {
		t.Errorf("unmapped should pass through: %+v", f)
	}
	if _, changed := m.ResolveAll([]Frame{{Module: "zzz"}}); changed {
		t.Error("nothing should have changed")
	}
}

func TestSourceMap(t *testing.T) {
	// Generated line 1: col 0 → src/a.js:1:0 (name "foo"); col 10 → src/a.js:3:4
	sm, err := ParseSourceMap([]byte(`{"version":3,"sources":["src/a.js"],"names":["foo"],"mappings":"AAAAA,UAEI"}`))
	if err != nil {
		t.Fatal(err)
	}
	f := sm.Resolve(Frame{Filename: "bundle.js", Lineno: 1, Colno: 1})
	if f.Filename != "src/a.js" || f.Lineno != 1 || f.Function != "foo" {
		t.Errorf("first mapping = %+v", f)
	}
	f = sm.Resolve(Frame{Filename: "bundle.js", Lineno: 1, Colno: 13})
	if f.Filename != "src/a.js" || f.Lineno != 3 || f.Colno != 5 {
		t.Errorf("second mapping = %+v", f)
	}
	f = sm.Resolve(Frame{Filename: "bundle.js", Lineno: 5, Colno: 0})
	if f.Filename != "bundle.js" || f.Lineno != 5 {
		t.Errorf("unmapped line should pass through: %+v", f)
	}

	set := NewSourceMapSet(map[string][]byte{"bundle.js.map": []byte(`{"version":3,"sources":["src/a.js"],"names":["foo"],"mappings":"AAAAA,UAEI"}`)})
	f = set.Resolve(Frame{Filename: "https://cdn.example.com/static/bundle.js?v=3", Lineno: 1, Colno: 1})
	if f.Filename != "src/a.js" || f.Function != "foo" {
		t.Errorf("set by name = %+v", f)
	}
	f = set.Resolve(Frame{Filename: "other.js", Lineno: 1, Colno: 1})
	if f.Filename != "other.js" {
		t.Errorf("another file must not be resolved through the only map (a vendor chunk is not the bundle) = %+v", f)
	}

	// ResolveAll reports whether anything changed: mapped frames do, unmapped ones pass through.
	frames := []Frame{{Filename: "bundle.js", Lineno: 1, Colno: 1}, {Filename: "bundle.js", Lineno: 5, Colno: 0}}
	out, changed := sm.ResolveAll(frames)
	if !changed || len(out) != 2 || out[0].Filename != "src/a.js" || out[1] != frames[1] {
		t.Errorf("ResolveAll = %+v changed=%v", out, changed)
	}
	if frames[0].Filename != "bundle.js" {
		t.Error("ResolveAll must not modify its input")
	}
	if out, changed := sm.ResolveAll([]Frame{{Filename: "bundle.js", Lineno: 5}}); changed || len(out) != 1 {
		t.Errorf("nothing mapped: %+v changed=%v", out, changed)
	}
	if out, changed := sm.ResolveAll(nil); changed || len(out) != 0 {
		t.Errorf("empty: %+v changed=%v", out, changed)
	}
}

// TestIndexedSourceMap: a map with `sections` (Metro / Hermes) folds its
// sub-maps in at their offsets; sourceRoot prefixes the sources.
func TestIndexedSourceMap(t *testing.T) {
	inner := `{"version":3,"sourceRoot":"src","sources":["a.js"],"names":["foo"],"mappings":"AAAAA,UAEI"}`
	sm, err := ParseSourceMap([]byte(`{"version":3,"sections":[{"offset":{"line":0,"column":0},"map":` + inner + `},{"offset":{"line":10,"column":5},"map":` + inner + `}]}`))
	if err != nil {
		t.Fatal(err)
	}
	if f := sm.Resolve(Frame{Filename: "bundle.js", Lineno: 1, Colno: 1}); f.Filename != "src/a.js" || f.Lineno != 1 || f.Function != "foo" {
		t.Errorf("first section = %+v", f)
	}
	// Second section: line 11, and its first line's columns shifted by 5.
	if f := sm.Resolve(Frame{Filename: "bundle.js", Lineno: 11, Colno: 6}); f.Filename != "src/a.js" || f.Lineno != 1 || f.Function != "foo" {
		t.Errorf("second section start = %+v", f)
	}
	if f := sm.Resolve(Frame{Filename: "bundle.js", Lineno: 11, Colno: 18}); f.Filename != "src/a.js" || f.Lineno != 3 {
		t.Errorf("second section, second mapping = %+v", f)
	}
	if f := sm.Resolve(Frame{Filename: "bundle.js", Lineno: 11, Colno: 3}); f.Filename != "bundle.js" {
		t.Errorf("before the section's column offset = %+v", f)
	}
	// A set with one map applies it to frames of that file, or of no file
	// at all — not to a vendor chunk it does not belong to.
	set := NewSourceMapSet(map[string][]byte{"bundle.js.map": []byte(inner)})
	if f := set.Resolve(Frame{Filename: "https://x/vendor.js", Lineno: 1, Colno: 1}); f.Filename != "https://x/vendor.js" {
		t.Errorf("another file must not be resolved through the wrong map: %+v", f)
	}
	if f := set.Resolve(Frame{Lineno: 1, Colno: 1}); f.Filename != "src/a.js" {
		t.Errorf("a nameless frame takes the only map: %+v", f)
	}
	if f := set.Resolve(Frame{Filename: "https://x/bundle.js?v=2", Lineno: 1, Colno: 1}); f.Filename != "src/a.js" {
		t.Errorf("the named file: %+v", f)
	}
}

func TestFrameJSONOmitsZero(t *testing.T) {
	b, _ := json.Marshal(Frame{Filename: "a"})
	if string(b) != `{"filename":"a"}` {
		t.Errorf("got %s", b)
	}
}

func TestDetectKind(t *testing.T) {
	cases := []struct {
		name string
		head string
		want string
	}{
		{"mapping.txt", "", KindProGuard},
		{"app-release-mapping.txt", "", KindProGuard},
		{"bundle.js.map", "", KindSourceMap},
		{"App.dSYM", "", KindDSYM},
		{"dSYMs.zip", "", KindDSYM},
		{"blob", "\xcf\xfa\xed\xfe\x0c\x00\x00\x01", KindDSYM},
		{"blob", `{"version":3,"mappings":"AAAA"}`, KindSourceMap},
		{"blob", "# compiler: R8\ncom.a -> b:\n", KindProGuard},
		{"blob", "hello", ""},
	}
	for _, c := range cases {
		if got := DetectKind(c.name, []byte(c.head)); got != c.want {
			t.Errorf("DetectKind(%q, %q) = %q, want %q", c.name, c.head, got, c.want)
		}
	}
}

func TestParseHexAndDebugID(t *testing.T) {
	if v, ok := sentry.ParseHex("0x1049e2b50"); !ok || v != 0x1049e2b50 {
		t.Errorf("parseHex = %x %v", v, ok)
	}
	if _, ok := sentry.ParseHex(""); ok {
		t.Error("empty should fail")
	}
	if got := normalizeDebugID("4A3B4C5D-1234-5678-9ABC-DEF012345678-1"); got != "4a3b4c5d-1234-5678-9abc-def012345678" {
		t.Errorf("normalizeDebugID = %q", got)
	}
}
