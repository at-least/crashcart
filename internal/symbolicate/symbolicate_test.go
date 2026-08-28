package symbolicate

import (
	"encoding/json"
	"testing"
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
	f := m.Resolve(Frame{Filename: "a.b.c", Function: "b", Lineno: 13})
	if f.Filename != "com.example.CartFragment.loadCart" || f.Function != "loadCart(java.lang.String)" || f.Lineno != 13 {
		t.Errorf("method resolve = %+v", f)
	}
	f = m.Resolve(Frame{Filename: "a.d", Function: "x"})
	if f.Filename != "com.example.Api.fetch" || f.Function != "fetch()" {
		t.Errorf("simple method = %+v", f)
	}
	f = m.Resolve(Frame{Filename: "a.b.c", Function: "unknown"})
	if f.Filename != "com.example.CartFragment.unknown" {
		t.Errorf("class-only = %+v", f)
	}
	f = m.Resolve(Frame{Filename: "zzz", Function: "q"})
	if f.Filename != "zzz" {
		t.Errorf("unmapped should pass through: %+v", f)
	}
}

func TestSourceMap(t *testing.T) {
	// Generated line 1: col 0 → src/a.js:1:0 (name "foo"); col 10 → src/a.js:3:4
	sm, err := ParseSourceMap([]byte(`{"version":3,"sources":["src/a.js"],"names":["foo"],"mappings":"AAAAA,UAEI"}`))
	if err != nil {
		t.Fatal(err)
	}
	f := sm.Resolve(Frame{Filename: "bundle.js", Lineno: 1, Colno: 0})
	if f.Filename != "src/a.js" || f.Lineno != 1 || f.Function != "foo" {
		t.Errorf("first mapping = %+v", f)
	}
	f = sm.Resolve(Frame{Filename: "bundle.js", Lineno: 1, Colno: 12})
	if f.Filename != "src/a.js" || f.Lineno != 3 || f.Colno != 4 {
		t.Errorf("second mapping = %+v", f)
	}
	f = sm.Resolve(Frame{Filename: "bundle.js", Lineno: 5, Colno: 0})
	if f.Filename != "bundle.js" || f.Lineno != 5 {
		t.Errorf("unmapped line should pass through: %+v", f)
	}
}

func TestFrameJSONOmitsZero(t *testing.T) {
	b, _ := json.Marshal(Frame{Filename: "a"})
	if string(b) != `{"filename":"a"}` {
		t.Errorf("got %s", b)
	}
}
