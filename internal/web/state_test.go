package web

import (
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/crashcartapp/crashcart/internal/store"
)

func TestViewStateRoundTrip(t *testing.T) {
	q, _ := url.ParseQuery("win=24h&status=resolved&sort=events&offset=50&before=2026-08-29T10%3A00%3A00Z_e1e1e1e1e1e1e1e1e1e1e1e1e1e1e1e1&level=fatal&release=1.2&tag.build=42&bogus=1&search_col=error_type&search_q=NPE&handled=false")
	s := ParseViewState("app", q)
	if s.Slug != "app" || s.Win != "24h" || s.Status != "resolved" || s.Sort != "events" || s.Offset != 50 || s.Before != "2026-08-29T10:00:00Z_e1e1e1e1e1e1e1e1e1e1e1e1e1e1e1e1" {
		t.Errorf("state = %+v", s)
	}
	if s.Filters["level"] != "fatal" || s.Filters["release"] != "1.2" || s.Filters["tag.build"] != "42" || s.Filters["error_type"] != "NPE" || s.Filters["handled"] != "false" {
		t.Errorf("filters = %v", s.Filters)
	}
	if _, ok := s.Filters["bogus"]; ok {
		t.Error("unknown params are not filters")
	}
	want := "/p/app/issues?before=2026-08-29T10%3A00%3A00Z_e1e1e1e1e1e1e1e1e1e1e1e1e1e1e1e1&error_type=NPE&handled=false&level=fatal&offset=50&release=1.2&sort=events&status=resolved&tag.build=42&win=24h"
	if got := s.Href("/issues"); got != want {
		t.Errorf("href = %s\nwant %s", got, want)
	}
	// Round trip: parse(Query()) == state
	q2, _ := url.ParseQuery(s.Query()[1:])
	s2 := ParseViewState("app", q2)
	if s2.Href("/issues") != want {
		t.Errorf("round trip = %s", s2.Href("/issues"))
	}
	// Defaults are omitted.
	if ParseViewState("x", url.Values{}).Href("/events") != "/p/x/events" {
		t.Error("default href")
	}
	// With* return copies and reset paging.
	n := s.WithFilter("level", "")
	if n.Offset != 0 || n.Before != "" || n.Filters["level"] != "" || s.Filters["level"] != "fatal" {
		t.Error("WithFilter must copy and reset paging")
	}
	if s.WithWin("bogus").Win != "24h" || s.WithWin("90d").Win != "90d" {
		t.Error("WithWin validates")
	}
	if p := s.Persist(); len(p.Filters) != 0 || p.Win != "24h" || p.Status != "unresolved" || p.Href("") != "/p/app?win=24h" {
		t.Errorf("Persist = %+v", p)
	}
	if s.WithFilter("bogus", "x").Filters["bogus"] != "" {
		t.Error("WithFilter must reject unknown keys")
	}
}

func TestWindow(t *testing.T) {
	now := time.Date(2026, 8, 29, 12, 30, 0, 0, time.UTC)
	w := ViewState{Win: "24h"}.Window(now)
	if w.Width != time.Hour || !w.To.Equal(now) || !w.From.Equal(now.Add(-24*time.Hour).Truncate(time.Hour)) || w.Days != 1 {
		t.Errorf("24h = %+v", w)
	}
	if n := len(w.Buckets()); n != 25 {
		t.Errorf("24h buckets = %d", n)
	}
	w = ViewState{Win: "30d"}.Window(now)
	if w.Width != day || w.Days != 30 || len(w.Buckets()) != 31 || w.From.Hour() != 0 {
		t.Errorf("30d = %+v (%d buckets)", w, len(w.Buckets()))
	}
	if w.Label(w.From) != w.From.Format("Jan 2") {
		t.Errorf("label = %s", w.Label(w.From))
	}
	if d := (ViewState{}).Window(now); d.Days != 7 || d.Width != 6*time.Hour || d.From.Hour()%6 != 0 {
		t.Errorf("default = %+v", d)
	}
}

func TestFormat(t *testing.T) {
	for n, want := range map[int64]string{0: "0", 999: "999", 1200: "1.2k", 10000: "10k", 999999: "999k", 1500000: "1.5M", 12000000: "12M"} {
		if got := compact(n); got != want {
			t.Errorf("compact(%d) = %s, want %s", n, got, want)
		}
	}
	if percent(0, 0) != "n/a" || percent(1, 4) != "25%" || percent(999, 1000) != "99.9%" || crashFree(1000, 1) != "99.9%" || crashFree(3, 0) != "100%" {
		t.Errorf("percent: %s %s %s %s", percent(1, 4), percent(999, 1000), crashFree(1000, 1), crashFree(3, 0))
	}
	n := time.Date(2026, 8, 29, 12, 0, 0, 0, time.UTC)
	if timeAgo(n.Add(-3*time.Minute), n) != "3m ago" || timeAgo(n.Add(-2*24*time.Hour), n) != "2d ago" || timeAgo(n.Add(-30*24*time.Hour), n) != "Jul 30" {
		t.Errorf("timeAgo: %s %s", timeAgo(n.Add(-3*time.Minute), n), timeAgo(n.Add(-30*24*time.Hour), n))
	}
	// A device clock ahead of ours is shown absolute rather than as a negative age.
	if got := timeAgo(n.Add(time.Minute), n); got != "Aug 29 12:01:00 UTC" {
		t.Errorf("future timeAgo = %s", got)
	}
	local := n.In(time.FixedZone("X", 8*3600))
	if formatTime(local) != "Aug 29 12:00:00 UTC" || formatDateTime(local) != "2026-08-29 12:00:00 UTC" {
		t.Errorf("format: %s / %s (must render in UTC)", formatTime(local), formatDateTime(local))
	}
	for in, want := range map[string]string{"FATAL": "fatal", "warn": "warning", "Warning": "warning", "critical": "fatal", "info": "info", "": "debug", "verbose": "debug"} {
		if got := levelKey(in); got != want {
			t.Errorf("levelKey(%q) = %s, want %s", in, got, want)
		}
	}
	if plural(1, "event") != "1 event" || plural(0, "event") != "0 events" || plural(12, "user") != "12 users" {
		t.Errorf("plural: %s %s", plural(1, "event"), plural(0, "event"))
	}
	if orDash("") != "—" || orDash("x") != "x" || firstNonEmpty("", "b") != "b" || firstNonEmpty("a", "b") != "a" || boolAttr(true) != "true" || boolAttr(false) != "false" {
		t.Error("orDash / firstNonEmpty / boolAttr")
	}
	if m := tagsMap([]byte(`{"build":"42"}`)); m["build"] != "42" {
		t.Errorf("tagsMap = %v", m)
	}
	if m := tagsMap([]byte(`not json`)); m == nil || len(m) != 0 {
		t.Errorf("tagsMap of garbage must be an empty map, got %v", m)
	}
	var nilStr *string
	x := "x"
	if deref(nilStr) != "" || deref(&x) != "x" || i64(7) != "7" || itoa(-3) != "-3" {
		t.Error("deref / i64 / itoa")
	}
}

func TestViewStatePaging(t *testing.T) {
	q, _ := url.ParseQuery("win=24h&level=fatal&offset=50")
	s := ParseViewState("app", q)
	n := s.WithOffset(100)
	if n.Offset != 100 || s.Offset != 50 || n.Filters["level"] != "fatal" || n.Win != "24h" {
		t.Errorf("WithOffset = %+v (from %+v)", n, s)
	}
	if n := s.WithOffset(-5); n.Offset != 0 {
		t.Errorf("negative offset = %d", n.Offset)
	}
	c := store.Cursor{At: time.Date(2026, 8, 29, 10, 0, 0, 0, time.UTC), EventID: "e1e1e1e1e1e1e1e1e1e1e1e1e1e1e1e1"}
	b := s.WithBefore(c)
	if b.Before != c.String() || s.Before != "" || b.Cursor() != c {
		t.Errorf("WithBefore = %+v", b)
	}
	if got := b.Href("/events"); !strings.Contains(got, "before=2026-08-29T10%3A00%3A00Z_e1e1e1e1e1e1e1e1e1e1e1e1e1e1e1e1") || !strings.Contains(got, "level=fatal") {
		t.Errorf("href = %s", got)
	}
	if z := s.WithBefore(store.Cursor{}); z.Before != "" {
		t.Errorf("zero cursor = %q", z.Before)
	}
	// Filters are copied, not shared.
	n.Filters["level"] = "info"
	if s.Filters["level"] != "fatal" {
		t.Error("WithOffset must copy the filters")
	}
}

func TestViewStateBounds(t *testing.T) {
	q := url.Values{"offset": {"900000000"}, "q": {strings.Repeat("x", 5000)}, "release": {"1.0"}}
	s := ParseViewState("p", q)
	if s.Offset != store.MaxOffset {
		t.Errorf("offset = %d, want clamped to %d", s.Offset, store.MaxOffset)
	}
	if len(s.Filters["q"]) != store.MaxFilterLen || s.Filters["release"] != "1.0" {
		t.Errorf("filters = %v", s.Filters)
	}
}

// TestSafeNext: the post-login target stays on this site — a path only;
// scheme-relative and backslash forms (a slash to browsers) are refused.
func TestSafeNext(t *testing.T) {
	for in, want := range map[string]string{
		"":                     "/",
		"/p/shop?win=7d":       "/p/shop?win=7d",
		"https://evil.example": "/",
		"//evil.example/x":     "/",
		`/\evil.example`:       "/",
		"/%5Cevil.example":     "/%5Cevil.example", // a literal path segment, not a host
		"javascript:alert(1)":  "/",
		"/p/shop#frag":         "/p/shop#frag",
	} {
		if got := safeNext(in); got != want {
			t.Errorf("safeNext(%q) = %q, want %q", in, got, want)
		}
	}
}
