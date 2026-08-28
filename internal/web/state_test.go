package web

import (
	"net/url"
	"testing"
	"time"
)

func TestViewStateRoundTrip(t *testing.T) {
	q, _ := url.ParseQuery("win=24h&anchor=2026-08-20&page=2&crash=1&release=1.2&error_type=NPE&build=42&bogus=1&search_col=q&search_q=hello")
	s := ParseViewState(q, map[string]bool{"build": true}, "/ios")
	if s.Win != "24h" || s.Anchor != "2026-08-20" || s.Page != 2 || !s.Crash || s.Release != "1.2" {
		t.Errorf("state = %+v", s)
	}
	if s.Filters["error_type"] != "NPE" || s.Filters["build"] != "42" || s.Filters["q"] != "hello" {
		t.Errorf("filters = %v", s.Filters)
	}
	if _, ok := s.Filters["bogus"]; ok {
		t.Error("unknown params are not filters")
	}
	if got := s.Href(); got != "/ios/dashboard?anchor=2026-08-20&build=42&crash=1&error_type=NPE&page=2&q=hello&release=1.2&win=24h" {
		t.Errorf("href = %s", got)
	}
	// filters reset paging; withFilter("") deletes
	n := s.WithFilter("error_type", "")
	if n.Page != 0 || n.Filters["error_type"] != "" || s.Filters["error_type"] != "NPE" {
		t.Error("WithFilter must copy and reset page")
	}
	if ParseViewState(url.Values{}, nil, "").Href() != "/dashboard" {
		t.Error("default href")
	}
}

func TestWindowFor(t *testing.T) {
	now := time.Date(2026, 8, 29, 12, 0, 0, 0, time.UTC)
	s := ViewState{Win: "7d", Anchor: "2026-08-20"}
	w := s.WindowFor(8, now) // viewer at UTC+8
	if !w.Range.Anchored || w.Range.Until == nil {
		t.Fatal("anchored window expected")
	}
	// end = local 2026-08-21 00:00 (+08:00) = 2026-08-20T16:00Z
	if !w.Range.Until.Equal(time.Date(2026, 8, 20, 16, 0, 0, 0, time.UTC)) {
		t.Errorf("until = %v", w.Range.Until)
	}
	if !w.Range.Since.Equal(time.Date(2026, 8, 13, 16, 0, 0, 0, time.UTC)) {
		t.Errorf("since = %v", w.Range.Since)
	}
	if v := w.QueryValues(); v.Get("from") != "2026-08-13T16:00:00Z" || v.Get("to") != "2026-08-20T16:00:00Z" {
		t.Errorf("query = %v", v)
	}
	rel := ViewState{Win: "24h"}.WindowFor(0, now)
	if !rel.Hourly || rel.Range.Anchored || rel.QueryValues().Get("days") != "1" {
		t.Errorf("relative = %+v", rel)
	}
}

func TestDeployments(t *testing.T) {
	deps := ParseDeployments("iOS|https://ios.example.com/|k1, Android |https://android.example.com, iOS|https://ios2.example.com")
	if len(deps) != 3 || deps[0].Slug != "ios" || deps[0].URL != "https://ios.example.com" || deps[0].Key != "k1" || deps[1].Slug != "android" || deps[2].Slug != "ios-3" {
		t.Errorf("deps = %+v", deps)
	}
	if SelfIndex(deps, "https", "android.example.com") != 1 || SelfIndex(deps, "http", "nope") != -1 {
		t.Error("selfIndex")
	}
}

func TestBucketLabel(t *testing.T) {
	ts := time.Date(2026, 8, 29, 23, 0, 0, 0, time.UTC)
	if bucketLabel(ts, true, 8) != "07:00" || bucketLabel(ts, false, 8) != "08-29" || bucketLabel(ts, true, -5) != "18:00" {
		t.Errorf("labels: %s %s", bucketLabel(ts, true, 8), bucketLabel(ts, true, -5))
	}
}
