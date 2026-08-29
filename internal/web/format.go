package web

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/at-least/crashcart/internal/pk"
)

// now is the render clock (overridable in tests).
var now = time.Now

// timeAgo renders "3m ago" style relative times; absolute beyond a week.
func timeAgo(t, now time.Time) string {
	d := now.Sub(t)
	switch {
	case d < 0:
		return formatTime(t)
	case d < time.Minute:
		return fmt.Sprintf("%ds ago", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm ago", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh ago", int(d.Hours()))
	case d < 7*24*time.Hour:
		return fmt.Sprintf("%dd ago", int(d.Hours()/24))
	}
	return t.UTC().Format("Jan 2")
}

// idAgo is timeAgo for an event/issue id.
func idAgo(id int64) string { return timeAgo(pk.Time(id), now()) }

// formatTime is "Aug 17 14:03:22 UTC".
func formatTime(t time.Time) string { return t.UTC().Format("Jan 2 15:04:05") + " UTC" }

// formatDateTime is "2026-08-17 14:03:22 UTC".
func formatDateTime(t time.Time) string { return t.UTC().Format("2006-01-02 15:04:05") + " UTC" }

// idTime is formatDateTime for an id.
func idTime(id int64) string { return formatDateTime(pk.Time(id)) }

// compact renders 1234 as "1.2k", 1234567 as "1.2M".
func compact(n int64) string {
	switch {
	case n < 0:
		return "-" + compact(-n)
	case n < 1000:
		return fmt.Sprintf("%d", n)
	case n < 10_000:
		return strings.TrimSuffix(fmt.Sprintf("%.1f", float64(n)/1000), ".0") + "k"
	case n < 1_000_000:
		return fmt.Sprintf("%dk", n/1000)
	case n < 10_000_000:
		return strings.TrimSuffix(fmt.Sprintf("%.1f", float64(n)/1_000_000), ".0") + "M"
	}
	return fmt.Sprintf("%dM", n/1_000_000)
}

// percent renders a ratio as "99.5%"; "n/a" when the denominator is zero.
func percent(num, den int64) string {
	if den <= 0 {
		return "n/a"
	}
	p := float64(num) / float64(den) * 100
	if p >= 99.95 && num != den {
		return "99.9%"
	}
	return strings.TrimSuffix(fmt.Sprintf("%.1f", p), ".0") + "%"
}

// crashFree is the crash-free session rate as text.
func crashFree(total, crashed int64) string { return percent(total-crashed, total) }

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

func orDash(s string) string {
	if s == "" {
		return "—"
	}
	return s
}

func firstNonEmpty(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

func boolAttr(b bool) string {
	if b {
		return "true"
	}
	return "false"
}

func plural(n int64, word string) string {
	if n == 1 {
		return fmt.Sprintf("%d %s", n, word)
	}
	return fmt.Sprintf("%d %ss", n, word)
}

// tagsMap decodes a jsonb tags column.
func tagsMap(raw json.RawMessage) map[string]string {
	m := map[string]string{}
	_ = json.Unmarshal(raw, &m)
	return m
}

func i64(n int64) string { return fmt.Sprintf("%d", n) }
func itoa(n int) string  { return fmt.Sprintf("%d", n) }
