package web

import (
	"strings"
	"testing"
)

func TestSparkline(t *testing.T) {
	s := sparkline([]int64{0, 3, 1, 5})
	if !strings.HasPrefix(s, "<svg") || !strings.Contains(s, "<polyline") || !strings.Contains(s, "<title>9 events") {
		t.Errorf("sparkline = %s", s)
	}
	if e := sparkline(nil); !strings.HasPrefix(e, "<svg") || strings.Contains(e, "polyline") {
		t.Errorf("empty sparkline = %s", e)
	}
}

func TestBars(t *testing.T) {
	series := []Series{{Name: "1.0", Token: "series-1"}, {Name: "<other>", Token: "series-other"}}
	buckets := []Bucket{{Label: "Aug 1 & co", Values: []int64{2, 1}}, {Label: "Aug 2", Values: []int64{0, 0}}}
	s := bars(buckets, series)
	if strings.Count(s, "<g class=\"bar\">") != 2 {
		t.Errorf("expected 2 bar groups: %s", s)
	}
	if !strings.Contains(s, "<title>Aug 1 &amp; co — 3 total\n1.0: 2\n&lt;other&gt;: 1</title>") {
		t.Errorf("title not escaped/formatted: %s", s)
	}
	if !strings.Contains(s, `fill="var(--series-1)"`) || !strings.Contains(s, `fill="var(--series-other)"`) {
		t.Error("series colors missing")
	}
	// stacked: the second rect sits on top of the first (smaller y)
	if strings.Count(s, "<rect") != 1+2+1 { // bucket 1: hit area + 2 value rects; bucket 2: hit area only
		t.Errorf("rect count: %s", s)
	}
	if !strings.HasPrefix(bars(nil, series), "<svg") {
		t.Error("empty chart must still be an svg")
	}
}

func TestCrashFreeBars(t *testing.T) {
	s := crashFreeBars([]HealthPoint{{Label: "d1", Total: 100, Crashed: 0}, {Label: "d2", Total: 100, Crashed: 3}, {Label: "d3", Total: 100, Crashed: 10}, {Label: "d4"}})
	for _, want := range []string{`fill="var(--ok)"`, `fill="var(--level-warning)"`, `fill="var(--level-fatal)"`, "no sessions", "crash-free: 97%"} {
		if !strings.Contains(s, want) {
			t.Errorf("missing %q in %s", want, s)
		}
	}
}

func TestStackGroups(t *testing.T) {
	yes, no := true, false
	st := Stack{Frames: []sentryFrame{{Function: "a", InApp: &yes}, {Function: "b", InApp: &no}, {Function: "c", InApp: &no}, {Function: "d", InApp: &yes}}}
	g := st.Groups()
	if len(g) != 3 || !g[0].InApp || g[1].InApp || len(g[1].Frames) != 2 || !g[2].InApp {
		t.Errorf("groups = %+v", g)
	}
	if frameLocation(sentryFrame{Filename: "a.js", Lineno: 3, Colno: 4}) != "a.js:3:4" || frameLocation(sentryFrame{InstrAddr: "0x1"}) != "0x1" {
		t.Error("frameLocation")
	}
}
