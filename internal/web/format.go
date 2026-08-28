package web

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/a-h/templ"
)

// timeAgo renders "3m ago" style relative times (UTC absolute beyond a day).
func timeAgo(t, now time.Time) string {
	d := now.Sub(t)
	if d < 0 {
		return formatTime(t)
	}
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds ago", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm ago", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh ago", int(d.Hours()))
	}
	return t.UTC().Format("Jan 2 15:04:05")
}

// formatTime is "Aug 17 14:03:22 UTC".
func formatTime(t time.Time) string { return t.UTC().Format("Jan 2 15:04:05") + " UTC" }

// formatDateTime is "2026-08-17 14:03:22 UTC".
func formatDateTime(t time.Time) string { return t.UTC().Format("2006-01-02 15:04:05") + " UTC" }

// formatClock extracts "14:03:22" from an RFC3339 string.
func formatClock(ts string) string {
	if i := strings.IndexByte(ts, 'T'); i >= 0 && len(ts) >= i+9 {
		return ts[i+1 : i+9]
	}
	return ts
}

// levelKey normalizes an SDK level to one of the styled severities.
func levelKey(level string) string {
	switch l := strings.ToLower(level); l {
	case "fatal", "error", "warning", "info", "debug":
		return l
	case "warn":
		return "warning"
	case "critical":
		return "fatal"
	}
	return "debug"
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

func jsonAttr(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}

// tagsMap decodes a jsonb tags column.
func tagsMap(raw json.RawMessage) map[string]string {
	m := map[string]string{}
	_ = json.Unmarshal(raw, &m)
	return m
}

// ── htmx attribute helpers ─────────────────────────────────

// hxNav swaps #shell with the given URL; HX-Push-Url from the server
// updates the address bar.
func hxNav(url string) templ.Attributes {
	return templ.Attributes{"hx-get": url, "hx-target": "#shell", "hx-select": "#shell", "hx-swap": "outerHTML"}
}

// rowTrigger fires on a focusable row unless a nested control was clicked.
const rowTrigger = "click[!event.target.closest('button,a,input,select')], keydown[key=='Enter']"

func hxRowNav(url string) templ.Attributes {
	a := hxNav(url)
	a["hx-trigger"] = rowTrigger
	a["tabindex"] = "0"
	return a
}

// now is the render clock (overridable in tests).
var now = time.Now

// sortedKeys returns m's keys in order.
func sortedKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// hxDetail loads an event's detail fragment into the sheet.
func hxDetail(base string, id int64) templ.Attributes {
	return templ.Attributes{
		"hx-get":      fmt.Sprintf("%s/events/%d/detail", base, id),
		"hx-trigger":  rowTrigger,
		"hx-target":   "#sheet-body",
		"hx-swap":     "innerHTML show:#sheet:top",
		"hx-push-url": "false",
	}
}
