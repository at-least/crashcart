package timerange

import (
	"net/url"
	"testing"
	"time"
)

var now = time.Date(2026, 8, 29, 12, 30, 0, 0, time.UTC)

func TestParseRelative(t *testing.T) {
	r := Parse(url.Values{"days": {"3"}}, 7, now)
	if r.Days != 3 || r.Until != nil || r.Anchored || !r.Since.Equal(now.Add(-72*time.Hour)) {
		t.Errorf("relative = %+v", r)
	}
	r = Parse(url.Values{"days": {"9999"}}, 7, now)
	if r.Days != 365 {
		t.Errorf("clamp = %d", r.Days)
	}
	r = Parse(url.Values{}, 7, now)
	if r.Days != 7 {
		t.Errorf("fallback = %d", r.Days)
	}
	slots := r.DaySlots(now)
	if len(slots) != 7 || slots[6].Format("2006-01-02") != "2026-08-29" || slots[0].Format("2006-01-02") != "2026-08-23" {
		t.Errorf("day slots = %v", slots)
	}
	hs := Range{Days: 1}.HourSlots(now)
	if len(hs) != 24 || !hs[23].Equal(time.Date(2026, 8, 29, 12, 0, 0, 0, time.UTC)) {
		t.Errorf("hour slots end = %v", hs[23])
	}
}

func TestParseAnchored(t *testing.T) {
	r := Parse(url.Values{"from": {"2026-08-20T00:00:00Z"}, "to": {"2026-08-27T00:00:00Z"}}, 7, now)
	if !r.Anchored || r.Until == nil || r.Days != 7 || !r.Until.Equal(time.Date(2026, 8, 27, 0, 0, 0, 0, time.UTC)) {
		t.Errorf("anchored = %+v", r)
	}
	slots := r.DaySlots(now)
	if slots[0].Format("2006-01-02") != "2026-08-20" || slots[6].Format("2006-01-02") != "2026-08-26" {
		t.Errorf("anchored slots = %v", slots)
	}
	// future `to` is clamped to now
	r = Parse(url.Values{"from": {"2026-08-28T00:00:00Z"}, "to": {"2099-01-01T00:00:00Z"}}, 7, now)
	if !r.Until.Equal(now) || r.Days != 2 {
		t.Errorf("clamped = %+v", r)
	}
	// hour slots walk forward from `from`
	hs := r.HourSlots(now)
	if !hs[0].Equal(time.Date(2026, 8, 28, 0, 0, 0, 0, time.UTC)) {
		t.Errorf("anchored hours start = %v", hs[0])
	}
}

func TestClampInt(t *testing.T) {
	if ClampInt("abc", 1, 10, 5) != 5 || ClampInt("50", 1, 10, 5) != 10 || ClampInt("-2", 0, 10, 5) != 0 {
		t.Error("clamp")
	}
}
