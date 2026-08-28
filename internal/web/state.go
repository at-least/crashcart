package web

import (
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"time"

	"github.com/newlix/crashcart/internal/timerange"
)

// ViewState is the dashboard state, encoded entirely in the URL query so
// every view is shareable and back/forward work for free.
type ViewState struct {
	// Base is the URL prefix of this deployment's pages ("" or "/ios").
	Base string
	Win  string // "24h" | "7d" | "30d"
	// Anchor is "" (follow now) or a YYYY-MM-DD local calendar date whose
	// end (exclusive) is the window's upper bound.
	Anchor  string
	Page    int
	Crash   bool   // Crashes card toggle → crashes only
	Err     bool   // Errors card toggle → level=fatal,error
	Release string // release picker (no chip)
	// Filters: q, level, error_type, user_id, device_id, device_model,
	// os_version, error_location + custom tag keys.
	Filters map[string]string
}

// FilterLabels maps filter keys to chip labels.
var FilterLabels = map[string]string{
	"q":              "message",
	"device_model":   "model",
	"os_version":     "os",
	"error_type":     "error",
	"user_id":        "user",
	"device_id":      "device",
	"level":          "level",
	"error_location": "location",
	"fingerprint":    "issue",
}

// SearchColumns are the search-bar targets (before custom tags).
var SearchColumns = [][2]string{
	{"q", "Message"}, {"error_type", "Error"}, {"user_id", "User"}, {"device_model", "Model"}, {"os_version", "OS"},
}

var (
	winValues = map[string]bool{"24h": true, "7d": true, "30d": true}
	dateRe    = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}$`)
	reserved  = map[string]bool{"win": true, "anchor": true, "page": true, "crash": true, "err": true, "release": true, "search_col": true, "search_q": true}
)

// ParseViewState reads the state from q.
func ParseViewState(q url.Values, customTagKeys map[string]bool, base string) ViewState {
	s := ViewState{Base: base, Win: "7d", Filters: map[string]string{}}
	if winValues[q.Get("win")] {
		s.Win = q.Get("win")
	}
	if dateRe.MatchString(q.Get("anchor")) {
		s.Anchor = q.Get("anchor")
	}
	if n, err := strconv.Atoi(q.Get("page")); err == nil && n > 0 {
		s.Page = n
	}
	s.Crash = q.Get("crash") == "1"
	s.Err = q.Get("err") == "1"
	s.Release = q.Get("release")
	for k := range q {
		if reserved[k] {
			continue
		}
		if _, ok := FilterLabels[k]; !ok && !customTagKeys[k] {
			continue
		}
		if v := q.Get(k); v != "" {
			s.Filters[k] = v
		}
	}
	// The search bar submits transient search_col / search_q.
	if col, qv := q.Get("search_col"), q.Get("search_q"); col != "" && qv != "" {
		if _, ok := FilterLabels[col]; ok || customTagKeys[col] {
			s.Filters[col] = qv
		}
	}
	return s
}

// Query renders the state as a query string (stable key order).
func (s ViewState) Query() string {
	p := url.Values{}
	if s.Win != "7d" {
		p.Set("win", s.Win)
	}
	if s.Anchor != "" {
		p.Set("anchor", s.Anchor)
	}
	keys := make([]string, 0, len(s.Filters))
	for k := range s.Filters {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		p.Set(k, s.Filters[k])
	}
	if s.Release != "" {
		p.Set("release", s.Release)
	}
	if s.Crash {
		p.Set("crash", "1")
	}
	if s.Err {
		p.Set("err", "1")
	}
	if s.Page > 0 {
		p.Set("page", strconv.Itoa(s.Page))
	}
	if enc := p.Encode(); enc != "" {
		return "?" + enc
	}
	return ""
}

// Href is the dashboard URL for the state.
func (s ViewState) Href() string { return s.Base + "/dashboard" + s.Query() }

// SettingsHref is the settings URL for this deployment.
func (s ViewState) SettingsHref() string { return s.Base + "/settings" }

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
	if value == "" {
		delete(n.Filters, key)
	} else {
		n.Filters[key] = value
	}
	n.Page = 0
	return n
}

// WithWin / WithAnchor / WithRelease / WithCrash / WithErr / WithPage return
// modified copies; all but WithPage reset paging.
func (s ViewState) WithWin(w string) ViewState    { n := s.clone(); n.Win, n.Page = w, 0; return n }
func (s ViewState) WithAnchor(a string) ViewState { n := s.clone(); n.Anchor, n.Page = a, 0; return n }
func (s ViewState) WithRelease(r string) ViewState {
	n := s.clone()
	n.Release, n.Page = r, 0
	return n
}
func (s ViewState) WithCrash(c bool) ViewState { n := s.clone(); n.Crash, n.Page = c, 0; return n }
func (s ViewState) WithErr(e bool) ViewState   { n := s.clone(); n.Err, n.Page = e, 0; return n }
func (s ViewState) WithPage(p int) ViewState   { n := s.clone(); n.Page = max(p, 0); return n }
func (s ViewState) Cleared() ViewState {
	n := s.clone()
	n.Filters, n.Crash, n.Err, n.Page = map[string]string{}, false, false, 0
	return n
}

// Window is the resolved query window plus chart granularity.
type Window struct {
	Range  timerange.Range
	Hourly bool
	TZ     int // viewer offset in hours (from the cc-tz cookie)
}

// WindowFor resolves win + anchor into a Range. The anchor is a LOCAL
// calendar date of the viewer; tzHours shifts it to UTC bounds.
func (s ViewState) WindowFor(tzHours int, now time.Time) Window {
	days := map[string]int{"24h": 1, "7d": 7, "30d": 30}[s.Win]
	if days == 0 {
		days = 7
	}
	w := Window{Hourly: s.Win == "24h", TZ: tzHours}
	if s.Anchor == "" {
		w.Range = timerange.Range{Since: now.UTC().Add(-time.Duration(days) * 24 * time.Hour), Days: days}
		return w
	}
	d, err := time.Parse("2006-01-02", s.Anchor)
	if err != nil {
		w.Range = timerange.Range{Since: now.UTC().Add(-time.Duration(days) * 24 * time.Hour), Days: days}
		return w
	}
	end := d.AddDate(0, 0, 1).Add(-time.Duration(tzHours) * time.Hour)
	if end.After(now) {
		end = now.UTC()
	}
	start := end.Add(-time.Duration(days) * 24 * time.Hour)
	w.Range = timerange.Range{Since: start, Until: &end, Days: days, Anchored: true}
	return w
}

// QueryValues renders the window as API query params (from/to or days).
func (w Window) QueryValues() url.Values {
	p := url.Values{}
	if w.Range.Anchored && w.Range.Until != nil {
		p.Set("from", w.Range.Since.UTC().Format(time.RFC3339))
		p.Set("to", w.Range.Until.UTC().Format(time.RFC3339))
	} else {
		p.Set("days", strconv.Itoa(w.Range.Days))
	}
	return p
}
