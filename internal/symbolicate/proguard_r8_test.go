package symbolicate

import "testing"

// A real R8 block: CartRepository was inlined into MainActivity.onCreate.
const r8Mapping = `cc.smoke.CartRepository -> R8$$REMOVED$$CLASS$$5:
# {"id":"sourceFile","fileName":"CartRepository.java"}
cc.smoke.MainActivity -> cc.smoke.MainActivity:
# {"id":"sourceFile","fileName":"MainActivity.java"}
    int $r8$clinit -> a
    0:3:void <init>():7:7 -> <init>
    0:2:void onCreate(android.os.Bundle):9:9 -> onCreate
    3:8:void onCreate(android.os.Bundle):10:10 -> onCreate
    31:50:int cc.smoke.CartRepository.computeTotal(java.lang.String,java.lang.Integer):16:16 -> onCreate
    31:50:int cc.smoke.CartRepository.loadCart(java.lang.String):11 -> onCreate
    31:50:void onCreate(android.os.Bundle):11 -> onCreate
    51:60:void onCreate(android.os.Bundle):12:21 -> onCreate
`

func TestProGuardR8Inlining(t *testing.T) {
	m := ParseProGuard(r8Mapping)
	in := true
	got := m.Expand(Frame{Module: "cc.smoke.MainActivity", Function: "onCreate", Lineno: 47, InApp: &in})
	if len(got) != 3 {
		t.Fatalf("expected 3 expanded frames, got %d: %+v", len(got), got)
	}
	want := []struct {
		module, fn, file string
		line             int
	}{
		{"cc.smoke.MainActivity", "onCreate", "MainActivity.java", 11},
		{"cc.smoke.CartRepository", "loadCart", "CartRepository.java", 11},
		{"cc.smoke.CartRepository", "computeTotal", "CartRepository.java", 16},
	}
	for i, w := range want {
		g := got[i]
		if g.Module != w.module || g.Function != w.fn || g.Lineno != w.line || g.Filename != w.file || !g.IsInApp() {
			t.Errorf("frame %d = %+v, want %+v", i, g, w)
		}
	}
	// Ranges starting at 0 are real ranges, not "unbounded".
	if f := m.Resolve(Frame{Module: "cc.smoke.MainActivity", Function: "onCreate", Lineno: 1}); f.Lineno != 9 {
		t.Errorf("line 1 → %+v, want onCreate:9", f)
	}
	// Linear range: 51:60 → 12:21.
	if f := m.Resolve(Frame{Module: "cc.smoke.MainActivity", Function: "onCreate", Lineno: 55}); f.Lineno != 16 || f.Function != "onCreate" {
		t.Errorf("line 55 → %+v, want onCreate:16", f)
	}
	// ResolveAll grows the frame list.
	out, changed := m.ResolveAll([]Frame{{Module: "android.app.Activity", Function: "performCreate", Lineno: 8980}, {Module: "cc.smoke.MainActivity", Function: "onCreate", Lineno: 47}})
	if !changed || len(out) != 4 || out[0].Function != "performCreate" || out[3].Function != "computeTotal" {
		t.Errorf("ResolveAll = %+v (changed=%v)", out, changed)
	}
}
