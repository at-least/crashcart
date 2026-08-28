// Package timerange resolves a query window from request parameters.
//
// Explicit `from` / `to` (RFC3339) take precedence; otherwise `days`
// (relative to now). `to` is exclusive and never in the future.
package timerange

import (
	"net/url"
	"strconv"
	"time"
)

// Range is a half-open window [Since, Until). Until == nil means "open
// ended" (everything up to now), which lets aggregate queries skip the upper
// bound entirely.
type Range struct {
	Since time.Time
	Until *time.Time
	Days  int
	// Anchored is true when the caller passed explicit bounds.
	Anchored bool
}

// End is the effective upper bound (Until, or now).
func (r Range) End(now time.Time) time.Time {
	if r.Until != nil {
		return *r.Until
	}
	return now
}

// Parse reads from/to/days. days is clamped to [1, 365].
func Parse(q url.Values, fallbackDays int, now time.Time) Range {
	now = now.UTC()
	from, okFrom := parseTime(q.Get("from"))
	to, okTo := parseTime(q.Get("to"))
	if okFrom {
		if from.After(now) {
			from = now
		}
		until := now
		if okTo && to.Before(now) {
			until = to
		}
		if until.Before(from) {
			until = from
		}
		days := int((until.Sub(from) + 24*time.Hour - time.Nanosecond) / (24 * time.Hour))
		if days < 1 {
			days = 1
		}
		u := until
		return Range{Since: from, Until: &u, Days: days, Anchored: true}
	}
	days := ClampInt(q.Get("days"), 1, 365, fallbackDays)
	return Range{Since: now.Add(-time.Duration(days) * 24 * time.Hour), Days: days}
}

func parseTime(s string) (time.Time, bool) {
	if s == "" {
		return time.Time{}, false
	}
	for _, layout := range []string{time.RFC3339Nano, time.RFC3339, "2006-01-02T15:04:05", "2006-01-02"} {
		if t, err := time.Parse(layout, s); err == nil {
			return t.UTC(), true
		}
	}
	return time.Time{}, false
}

// ClampInt parses an integer query param into [lo, hi]; missing/invalid → def.
func ClampInt(s string, lo, hi, def int) int {
	n, err := strconv.Atoi(s)
	if err != nil {
		return def
	}
	return min(max(n, lo), hi)
}

// DaySlots lists the UTC days covered by the range, oldest first. Anchored
// ranges walk forward from the start day; relative ranges end today.
func (r Range) DaySlots(now time.Time) []time.Time {
	var start time.Time
	if r.Anchored {
		start = startOfDay(r.Since)
	} else {
		start = startOfDay(now.UTC()).AddDate(0, 0, -(r.Days - 1))
	}
	out := make([]time.Time, r.Days)
	for i := range out {
		out[i] = start.AddDate(0, 0, i)
	}
	return out
}

// HourSlots lists the 24 hour buckets of a one-day range, oldest first:
// anchored → the 24 hours from Since; relative → the 24 hours ending now.
func (r Range) HourSlots(now time.Time) []time.Time {
	var start time.Time
	if r.Anchored {
		start = r.Since.UTC().Truncate(time.Hour)
	} else {
		start = now.UTC().Truncate(time.Hour).Add(-23 * time.Hour)
	}
	out := make([]time.Time, 24)
	for i := range out {
		out[i] = start.Add(time.Duration(i) * time.Hour)
	}
	return out
}

func startOfDay(t time.Time) time.Time {
	t = t.UTC()
	return time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, time.UTC)
}
