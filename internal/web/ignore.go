package web

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/crashcartapp/crashcart/internal/store"
)

// ignore is the condition an issue is ignored under (Sentry's archive
// "until …"): any that is set puts it back to unresolved when met; none
// set means ignored for good.
type ignore struct {
	Until      *time.Time
	Events     *int64 // this many further events
	Escalating bool
}

// ignoreOptions are the status select's "Ignored …" choices, value →
// label, in display order. The bare `ignored` is the viewer's default —
// until escalating, Sentry's "Archive"; `ignored:forever` is the
// unconditional one (what the API's plain {"status": "ignored"} means).
var ignoreOptions = []struct{ Value, Label string }{
	{"ignored", "Ignored until escalating"},
	{"ignored:7d", "Ignored for 7 days"},
	{"ignored:30d", "Ignored for 30 days"},
	{"ignored:100", "Ignored until 100 more events"},
	{"ignored:1000", "Ignored until 1000 more events"},
	{"ignored:forever", "Ignored"},
}

// parseStatus turns a status form value — unresolved, resolved, or one of
// ignoreOptions — into the status and its ignore condition.
func parseStatus(v string, at time.Time) (status string, ig ignore, ok bool) {
	switch v {
	case "unresolved", "resolved":
		return v, ignore{}, true
	case "ignored":
		return "ignored", ignore{Escalating: true}, true
	}
	cond, found := strings.CutPrefix(v, "ignored:")
	if !found {
		return "", ignore{}, false
	}
	switch cond {
	case "forever":
		return "ignored", ignore{}, true
	case "escalating":
		return "ignored", ignore{Escalating: true}, true
	}
	if days, ok := strings.CutSuffix(cond, "d"); ok {
		if n, err := strconv.Atoi(days); err == nil && n > 0 && n <= 3650 {
			t := at.Add(time.Duration(n) * 24 * time.Hour)
			return "ignored", ignore{Until: &t}, true
		}
		return "", ignore{}, false
	}
	if n, err := strconv.ParseInt(cond, 10, 64); err == nil && n > 0 && n <= 1_000_000_000 {
		return "ignored", ignore{Events: &n}, true
	}
	return "", ignore{}, false
}

// statusValue is the select option that stands for the issue's current
// state (the ignore condition mapped back onto ignoreOptions).
func statusValue(is store.Issue) string {
	if is.Status != "ignored" {
		return string(is.Status)
	}
	switch {
	case is.IgnoreUntilEscalating:
		return "ignored"
	case is.IgnoreUntil != nil:
		if time.Until(*is.IgnoreUntil) <= 7*24*time.Hour {
			return "ignored:7d"
		}
		return "ignored:30d"
	case is.IgnoreUntilCount != nil:
		if *is.IgnoreUntilCount-is.EventCount <= 100 {
			return "ignored:100"
		}
		return "ignored:1000"
	}
	return "ignored:forever"
}

// ignoreLabel says under what condition an ignored issue comes back;
// "" for any other status or an unconditional ignore.
func ignoreLabel(is store.Issue) string {
	if is.Status != "ignored" {
		return ""
	}
	var parts []string
	if is.IgnoreUntilEscalating {
		rate := "no events"
		if is.IgnoreBaseline != nil && *is.IgnoreBaseline > 0 {
			rate = strings.TrimSuffix(fmt.Sprintf("%.1f", float64(*is.IgnoreBaseline)/24), ".0") + "/h"
		}
		parts = append(parts, "until escalating (was "+rate+")")
	}
	if is.IgnoreUntil != nil {
		parts = append(parts, "until "+formatDateTime(*is.IgnoreUntil))
	}
	if is.IgnoreUntilCount != nil {
		parts = append(parts, fmt.Sprintf("until %d more events", max(*is.IgnoreUntilCount-is.EventCount, 0)))
	}
	return strings.Join(parts, " · ")
}
