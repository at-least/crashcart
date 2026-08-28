package api

import (
	"net/url"
	"testing"
	"time"
)

func TestParseWindow(t *testing.T) {
	now := time.Date(2026, 8, 29, 12, 0, 0, 0, time.UTC)
	cases := []struct {
		q        string
		from, to time.Time
		wantErr  bool
	}{
		{"", now.AddDate(0, 0, -7), now, false},
		{"days=1", now.AddDate(0, 0, -1), now, false},
		{"days=90", now.AddDate(0, 0, -90), now, false},
		{"days=91", time.Time{}, time.Time{}, true},
		{"days=0", time.Time{}, time.Time{}, true},
		{"days=x", time.Time{}, time.Time{}, true},
		{"from=2026-08-01T00:00:00Z&to=2026-08-02T00:00:00Z", time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC), time.Date(2026, 8, 2, 0, 0, 0, 0, time.UTC), false},
		{"from=2026-08-01T00:00:00Z", time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC), now, false},
		{"to=2026-08-10T00:00:00Z&days=3", time.Date(2026, 8, 7, 0, 0, 0, 0, time.UTC), time.Date(2026, 8, 10, 0, 0, 0, 0, time.UTC), false},
		{"from=2026-08-02T00:00:00Z&to=2026-08-01T00:00:00Z", time.Time{}, time.Time{}, true},
		{"from=2026-01-01T00:00:00Z&to=2026-08-01T00:00:00Z", time.Time{}, time.Time{}, true},
		{"from=nope", time.Time{}, time.Time{}, true},
		{"from=2026-08-01T02:00:00%2B02:00&to=2026-08-01T12:00:00Z", time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC), time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC), false},
	}
	for _, c := range cases {
		q, _ := url.ParseQuery(c.q)
		from, to, err := parseWindowAt(q, now)
		if c.wantErr {
			if err == nil {
				t.Errorf("%q: expected error, got %v..%v", c.q, from, to)
			}
			continue
		}
		if err != nil {
			t.Errorf("%q: %v", c.q, err)
			continue
		}
		if !from.Equal(c.from) || !to.Equal(c.to) {
			t.Errorf("%q: got %v..%v want %v..%v", c.q, from, to, c.from, c.to)
		}
		if from.Location() != time.UTC || to.Location() != time.UTC {
			t.Errorf("%q: not UTC", c.q)
		}
	}
}

func TestParseWindowNow(t *testing.T) {
	from, to, err := ParseWindow(url.Values{})
	if err != nil {
		t.Fatal(err)
	}
	if d := to.Sub(from); d != 7*24*time.Hour {
		t.Errorf("default span = %v", d)
	}
	if time.Since(to) > time.Minute {
		t.Errorf("to should be now, got %v", to)
	}
}
