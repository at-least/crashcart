package web

import (
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/crashcartapp/crashcart/internal/sentry"
	"github.com/crashcartapp/crashcart/internal/store"
)

// ViewState is everything a page needs from the URL query. Every navigation
// is a plain link built from a modified copy (With* return copies), so
// views are shareable and back/forward work for free.
type ViewState struct {
	Slug   string // project slug ("" on the portal)
	Win    string // 24h | 7d | 30d | 90d
	Status string // issues tab: unresolved (default) | triaged | resolved | ignored | regression | all
	Sort   string // issues sort: last_seen (default) | first_seen | events
	Offset int    // issues pager
	Before string // events cursor (store.Cursor.String()): rows after it, newest first
	// Filters are the event filters (allowlisted keys plus "tag.<key>").
	Filters map[string]string
}

// Windows lists the selectable time windows in display order.
var Windows = []string{"24h", "7d", "30d", "90d"}

var winDays = map[string]int{"24h": 1, "7d": 7, "30d": 30, "90d": 90}

// IssueStatuses are the issue tabs in display order ("all" is added last).
var IssueStatuses = []string{"unresolved", "triaged", "resolved", "ignored", "regression"}

var issueSorts = map[string]bool{"last_seen": true, "first_seen": true, "events": true}

// FilterLabels maps filter keys to chip labels; the map is also the allowlist.
var FilterLabels = map[string]string{
	"q": "message", "level": "level", "release": "release", "environment": "env", "platform": "platform",
	"error_type": "error", "user_id": "user", "device_id": "device", "device_model": "model", "os_version": "os",
	"transaction": "transaction", "culprit": "culprit", "fingerprint": "issue", "handled": "handled",
}

// FilterColumns are the toolbar selects (key, label) in display order.
var FilterColumns = [][2]string{
	{"q", "Message"}, {"error_type", "Error type"}, {"culprit", "Culprit"}, {"user_id", "User"},
	{"device_id", "Device"}, {"device_model", "Model"}, {"os_version", "OS"}, {"transaction", "Transaction"},
}

var reserved = map[string]bool{"win": true, "status": true, "sort": true, "offset": true, "before": true, "search_col": true, "search_q": true}

// ParseViewState reads the state from q for project slug.
func ParseViewState(slug string, q url.Values) ViewState {
	s := ViewState{Slug: slug, Win: "7d", Status: "unresolved", Sort: "last_seen", Filters: map[string]string{}}
	if winDays[q.Get("win")] > 0 {
		s.Win = q.Get("win")
	}
	if st := q.Get("status"); st == "all" || isIssueStatus(st) {
		s.Status = st
	}
	if issueSorts[q.Get("sort")] {
		s.Sort = q.Get("sort")
	}
	if n, err := strconv.Atoi(q.Get("offset")); err == nil && n > 0 {
		s.Offset = min(n, store.MaxOffset)
	}
	if c, ok := store.ParseCursor(q.Get("before")); ok && !c.IsZero() {
		s.Before = c.String()
	}
	for k := range q {
		if reserved[k] || !filterKey(k) {
			continue
		}
		if v := q.Get(k); v != "" {
			s.Filters[k] = sentry.Truncate(v, store.MaxFilterLen)
		}
	}
	// The search form submits a transient column + query pair.
	if col, qv := q.Get("search_col"), strings.TrimSpace(q.Get("search_q")); col != "" && qv != "" && filterKey(col) {
		s.Filters[col] = sentry.Truncate(qv, store.MaxFilterLen)
	}
	return s
}

func isIssueStatus(s string) bool {
	for _, st := range IssueStatuses {
		if st == s {
			return true
		}
	}
	return false
}

// filterKey reports whether k may be used as an event filter.
func filterKey(k string) bool {
	if _, ok := FilterLabels[k]; ok {
		return true
	}
	return strings.HasPrefix(k, "tag.") && len(k) > 4 && len(k) < 64
}

// Query renders the state as a query string (stable key order, defaults omitted).
func (s ViewState) Query() string {
	p := url.Values{}
	if s.Win != "7d" && s.Win != "" {
		p.Set("win", s.Win)
	}
	if s.Status != "unresolved" && s.Status != "" {
		p.Set("status", s.Status)
	}
	if s.Sort != "last_seen" && s.Sort != "" {
		p.Set("sort", s.Sort)
	}
	if s.Offset > 0 {
		p.Set("offset", strconv.Itoa(s.Offset))
	}
	if s.Before != "" {
		p.Set("before", s.Before)
	}
	for _, k := range sortedKeys(s.Filters) {
		p.Set(k, s.Filters[k])
	}
	if enc := p.Encode(); enc != "" {
		return "?" + enc
	}
	return ""
}

// Base is the project's URL prefix ("/p/<slug>").
func (s ViewState) Base() string { return "/p/" + s.Slug }

// Href is the URL of page (e.g. "/issues") for this project with the state.
func (s ViewState) Href(page string) string { return s.Base() + page + s.Query() }

// Persist keeps only the cross-page state (window) — used by nav links so
// the events filters don't leak into the issues page and vice versa.
func (s ViewState) Persist() ViewState {
	return ViewState{Slug: s.Slug, Win: s.Win, Status: "unresolved", Sort: "last_seen", Filters: map[string]string{}}
}

func (s ViewState) clone() ViewState {
	f := make(map[string]string, len(s.Filters))
	for k, v := range s.Filters {
		f[k] = v
	}
	s.Filters = f
	return s
}

// WithFilter sets (or removes, when value == "") one filter and resets paging.
func (s ViewState) WithFilter(key, value string) ViewState {
	n := s.clone()
	if value == "" || !filterKey(key) {
		delete(n.Filters, key)
	} else {
		n.Filters[key] = value
	}
	n.Offset, n.Before = 0, ""
	return n
}

// WithWin / WithStatus / WithSort reset paging; WithOffset / WithBefore page.
func (s ViewState) WithWin(w string) ViewState {
	n := s.clone()
	if winDays[w] > 0 {
		n.Win = w
	}
	n.Offset, n.Before = 0, ""
	return n
}
func (s ViewState) WithStatus(st string) ViewState {
	n := s.clone()
	n.Status, n.Offset = st, 0
	return n
}
func (s ViewState) WithSort(so string) ViewState { n := s.clone(); n.Sort, n.Offset = so, 0; return n }
func (s ViewState) WithOffset(o int) ViewState   { n := s.clone(); n.Offset = max(o, 0); return n }
func (s ViewState) WithBefore(c store.Cursor) ViewState {
	n := s.clone()
	n.Before = c.String()
	return n
}

// Cursor is the parsed Before.
func (s ViewState) Cursor() store.Cursor { c, _ := store.ParseCursor(s.Before); return c }

// Cleared drops all filters and paging.
func (s ViewState) Cleared() ViewState {
	n := s.clone()
	n.Filters, n.Offset, n.Before = map[string]string{}, 0, ""
	return n
}

// Window is the resolved time range [From, To) plus the chart bucket
// width. From is aligned to a bucket start (UTC).
type Window struct {
	From, To time.Time
	Width    time.Duration
	Days     int
}

const day = 24 * time.Hour

// Window resolves the win selector at now.
func (s ViewState) Window(now time.Time) Window {
	days := winDays[s.Win]
	if days == 0 {
		days = 7
	}
	now = now.UTC()
	w := Window{From: now.Add(-time.Duration(days) * day), To: now, Days: days}
	switch {
	case days <= 1:
		w.Width = time.Hour
	case days <= 7:
		w.Width = 6 * time.Hour
	default:
		w.Width = day
	}
	w.From = w.Bucket(w.From)
	return w
}

// Bucket truncates t to the start of its Width-sized bucket (UTC-aligned;
// Go's Truncate counts from the zero time, which is midnight UTC).
func (w Window) Bucket(t time.Time) time.Time { return t.UTC().Truncate(w.Width) }

// Seconds is the bucket width for the chart queries.
func (w Window) Seconds() int64 { return int64(w.Width / time.Second) }

// Buckets lists the bucket starts of the window.
func (w Window) Buckets() []time.Time {
	var out []time.Time
	for b := w.From; b.Before(w.To); b = b.Add(w.Width) {
		out = append(out, b)
	}
	return out
}

// Label formats a bucket start for tooltips.
func (w Window) Label(bucket time.Time) string {
	if w.Width < day {
		return bucket.UTC().Format("Jan 2 15:04")
	}
	return bucket.UTC().Format("Jan 2")
}

func sortedKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
