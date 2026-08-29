package web

import (
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/crashcartapp/crashcart/internal/pk"
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
	Before int64  // events cursor: id < Before
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
	"screen": "screen", "error_location": "location", "fingerprint": "issue", "crash": "crash",
}

// FilterColumns are the toolbar selects (key, label) in display order.
var FilterColumns = [][2]string{
	{"q", "Message"}, {"error_type", "Error"}, {"error_location", "Location"}, {"user_id", "User"},
	{"device_id", "Device"}, {"device_model", "Model"}, {"os_version", "OS"}, {"screen", "Screen"},
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
		s.Offset = n
	}
	if n, err := strconv.ParseInt(q.Get("before"), 10, 64); err == nil && n > 0 {
		s.Before = n
	}
	for k := range q {
		if reserved[k] || !filterKey(k) {
			continue
		}
		if v := q.Get(k); v != "" {
			s.Filters[k] = v
		}
	}
	// The search form submits a transient column + query pair.
	if col, qv := q.Get("search_col"), strings.TrimSpace(q.Get("search_q")); col != "" && qv != "" && filterKey(col) {
		s.Filters[col] = qv
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
	if s.Before > 0 {
		p.Set("before", strconv.FormatInt(s.Before, 10))
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
	n.Offset, n.Before = 0, 0
	return n
}

// WithWin / WithStatus / WithSort reset paging; WithOffset / WithBefore page.
func (s ViewState) WithWin(w string) ViewState {
	n := s.clone()
	if winDays[w] > 0 {
		n.Win = w
	}
	n.Offset, n.Before = 0, 0
	return n
}
func (s ViewState) WithStatus(st string) ViewState {
	n := s.clone()
	n.Status, n.Offset = st, 0
	return n
}
func (s ViewState) WithSort(so string) ViewState { n := s.clone(); n.Sort, n.Offset = so, 0; return n }
func (s ViewState) WithOffset(o int) ViewState   { n := s.clone(); n.Offset = max(o, 0); return n }
func (s ViewState) WithBefore(b int64) ViewState { n := s.clone(); n.Before = max(b, 0); return n }

// Cleared drops all filters and paging.
func (s ViewState) Cleared() ViewState {
	n := s.clone()
	n.Filters, n.Offset, n.Before = map[string]string{}, 0, 0
	return n
}

// Window is the resolved id range [From, To) plus the chart bucket width.
type Window struct {
	From, To int64
	Width    int64 // bucket width in id units
	Days     int
}

// Window resolves the win selector at now.
func (s ViewState) Window(now time.Time) Window {
	days := winDays[s.Win]
	if days == 0 {
		days = 7
	}
	w := Window{From: pk.Lower(now.Add(-time.Duration(days) * 24 * time.Hour)), To: pk.Upper(now), Days: days}
	switch {
	case days <= 1:
		w.Width = pk.Hour
	case days <= 7:
		w.Width = 6 * pk.Hour
	default:
		w.Width = pk.Day
	}
	w.From = pk.Bucket(w.From, w.Width)
	return w
}

// Buckets lists the bucket starts of the window.
func (w Window) Buckets() []int64 {
	var out []int64
	for b := w.From; b < w.To; b += w.Width {
		out = append(out, b)
	}
	return out
}

// Label formats a bucket start for tooltips.
func (w Window) Label(bucket int64) string {
	t := pk.Time(bucket)
	if w.Width < pk.Day {
		return t.Format("Jan 2 15:04")
	}
	return t.Format("Jan 2")
}

func sortedKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
