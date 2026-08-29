package web

import (
	"net/url"
	"testing"
	"time"
)

func TestViewStateRoundTrip(t *testing.T) {
	q, _ := url.ParseQuery("win=24h&status=resolved&sort=events&offset=50&before=2026-08-29T10%3A00%3A00Z_e1&level=fatal&release=1.2&tag.build=42&bogus=1&search_col=error_type&search_q=NPE&crash=1")
	s := ParseViewState("app", q)
	if s.Slug != "app" || s.Win != "24h" || s.Status != "resolved" || s.Sort != "events" || s.Offset != 50 || s.Before != "2026-08-29T10:00:00Z_e1" {
		t.Errorf("state = %+v", s)
	}
	if s.Filters["level"] != "fatal" || s.Filters["release"] != "1.2" || s.Filters["tag.build"] != "42" || s.Filters["error_type"] != "NPE" || s.Filters["crash"] != "1" {
		t.Errorf("filters = %v", s.Filters)
	}
	if _, ok := s.Filters["bogus"]; ok {
		t.Error("unknown params are not filters")
	}
	want := "/p/app/issues?before=2026-08-29T10%3A00%3A00Z_e1&crash=1&error_type=NPE&level=fatal&offset=50&release=1.2&sort=events&status=resolved&tag.build=42&win=24h"
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
}
