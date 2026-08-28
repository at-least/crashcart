// Package sentry parses the Sentry envelope protocol into the subset of
// event data CrashCart stores. Unknown fields are preserved verbatim in
// Event.Raw (the payload column).
package sentry

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"math"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// Breadcrumb is a normalized Sentry breadcrumb (last 20 are kept).
type Breadcrumb struct {
	Timestamp string `json:"timestamp"` // RFC3339 UTC or ""
	Category  string `json:"category"`
	Message   string `json:"message"`
	Level     string `json:"level"`
}

// Event is one parsed "event" envelope item.
type Event struct {
	EventID     string
	Timestamp   time.Time
	Level       string
	Message     string
	Platform    string
	OSVersion   string
	DeviceModel string
	Release     string
	Screen      string
	ErrorType   string
	Handled     *bool // false = crash, true = caught, nil = no exception
	SDKName     string
	UserID      string
	Environment string
	Tags        map[string]string
	Breadcrumbs []Breadcrumb
	// Raw is the untouched item body.
	Raw []byte

	exception *rawException
	crumbs    []rawBreadcrumb
	sdkFP     []string
}

// DeviceID is the `device_id` tag, if any.
func (e *Event) DeviceID() string { return e.Tags["device_id"] }

// IsCrash reports whether the event counts as a crash (fatal or unhandled).
func (e *Event) IsCrash() bool {
	return e.Level == "fatal" || (e.Handled != nil && !*e.Handled)
}

// Session is one Sentry session (health monitoring) item.
type Session struct {
	Release   string
	Status    string // started | exited | crashed | errored | abnormal
	StartedAt time.Time
}

// Envelope is the parsed result of one POST body.
type Envelope struct {
	Events   []*Event
	Sessions []Session
}

type itemHeader struct {
	Type   string `json:"type"`
	Length *int   `json:"length"`
}

type envelopeHeader struct {
	EventID string          `json:"event_id"`
	SentAt  json.RawMessage `json:"sent_at"`
}

// Parse splits an envelope body into events and sessions. Malformed items
// are skipped; nothing here returns an error because a partially valid
// envelope should still deliver its good items.
func Parse(body []byte, now time.Time) Envelope {
	var env Envelope
	nl := bytes.IndexByte(body, '\n')
	if nl < 0 {
		return env
	}
	var hdr envelopeHeader
	if json.Unmarshal(body[:nl], &hdr) != nil {
		return env
	}
	fallbackTS := parseTimestamp(hdr.SentAt)
	if fallbackTS.IsZero() {
		fallbackTS = now
	}

	rest := body[nl+1:]
	for len(rest) > 0 {
		line, next := cutLine(rest)
		rest = next
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		var ih itemHeader
		if json.Unmarshal(line, &ih) != nil {
			continue // resync one line at a time
		}
		// Item body: explicit length wins (bodies may contain newlines);
		// otherwise it is the next line.
		var itemBody []byte
		if ih.Length != nil && *ih.Length >= 0 && *ih.Length <= len(rest) {
			itemBody = rest[:*ih.Length]
			rest = rest[*ih.Length:]
			if len(rest) > 0 && rest[0] == '\n' {
				rest = rest[1:]
			}
		} else {
			itemBody, rest = cutLine(rest)
		}
		switch ih.Type {
		case "event":
			if ev := parseEvent(hdr.EventID, fallbackTS, itemBody, now); ev != nil {
				env.Events = append(env.Events, ev)
			}
		case "session", "sessions":
			env.Sessions = append(env.Sessions, parseSessions(itemBody, now)...)
		}
	}
	return env
}

func cutLine(b []byte) (line, rest []byte) {
	if i := bytes.IndexByte(b, '\n'); i >= 0 {
		return b[:i], b[i+1:]
	}
	return b, nil
}

// ── event ───────────────────────────────────────────────────

type rawFrame struct {
	Filename string `json:"filename"`
	AbsPath  string `json:"abs_path"`
	Function string `json:"function"`
	Module   string `json:"module"`
	Sym      string `json:"sym"`
	Lineno   int    `json:"lineno"`
	InApp    *bool  `json:"in_app"`
}

type rawException struct {
	Type       string `json:"type"`
	Value      string `json:"value"`
	Stacktrace *struct {
		Frames []rawFrame `json:"frames"`
	} `json:"stacktrace"`
	Mechanism *struct {
		Handled *bool `json:"handled"`
	} `json:"mechanism"`
}

type rawBreadcrumb struct {
	Timestamp json.RawMessage `json:"timestamp"`
	Type      string          `json:"type"`
	Category  string          `json:"category"`
	Message   string          `json:"message"`
	Level     string          `json:"level"`
	Data      map[string]any  `json:"data"`
}

type rawEvent struct {
	EventID   string          `json:"event_id"`
	Timestamp json.RawMessage `json:"timestamp"`
	Level     string          `json:"level"`
	Message   json.RawMessage `json:"message"`
	Logentry  *struct {
		Message   string `json:"message"`
		Formatted string `json:"formatted"`
	} `json:"logentry"`
	Platform    string          `json:"platform"`
	Environment string          `json:"environment"`
	Release     string          `json:"release"`
	Transaction string          `json:"transaction"`
	ServerName  string          `json:"server_name"`
	Tags        json.RawMessage `json:"tags"`
	Breadcrumbs json.RawMessage `json:"breadcrumbs"`
	Fingerprint []string        `json:"fingerprint"`
	Contexts    *struct {
		Device *struct {
			Model string `json:"model"`
		} `json:"device"`
		OS *struct {
			Version string `json:"version"`
		} `json:"os"`
		App *struct {
			AppVersion string `json:"app_version"`
		} `json:"app"`
		Runtime *struct {
			Name    string `json:"name"`
			Version string `json:"version"`
		} `json:"runtime"`
	} `json:"contexts"`
	Exception *struct {
		Values []rawException `json:"values"`
	} `json:"exception"`
	SDK *struct {
		Name string `json:"name"`
	} `json:"sdk"`
	User *struct {
		ID json.RawMessage `json:"id"`
	} `json:"user"`
}

var eventIDRe = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)

const maxMessage = 500

func parseEvent(headerEventID string, fallbackTS time.Time, body []byte, now time.Time) *Event {
	var re rawEvent
	if json.Unmarshal(body, &re) != nil || len(bytes.TrimSpace(body)) == 0 || bytes.TrimSpace(body)[0] != '{' {
		return nil
	}
	ev := &Event{
		Raw:         body,
		Level:       normalizeLevel(re.Level),
		Platform:    re.Platform,
		Environment: re.Environment,
		Screen:      re.Transaction,
		Release:     re.Release,
		Tags:        parseTags(re.Tags),
		sdkFP:       re.Fingerprint,
	}
	ev.Timestamp = parseTimestamp(re.Timestamp)
	if ev.Timestamp.IsZero() {
		ev.Timestamp = fallbackTS
	}
	switch {
	case eventIDRe.MatchString(re.EventID):
		ev.EventID = re.EventID
	case eventIDRe.MatchString(headerEventID):
		ev.EventID = headerEventID
	default:
		sum := sha256.Sum256(body)
		ev.EventID = "ts-" + strconv.FormatInt(ev.Timestamp.UnixMilli(), 10) + "-" + hex.EncodeToString(sum[:6])
	}
	if c := re.Contexts; c != nil {
		if c.OS != nil {
			ev.OSVersion = c.OS.Version
		}
		if ev.OSVersion == "" && c.Runtime != nil {
			name := c.Runtime.Name
			if name == "" {
				name = "?"
			}
			ev.OSVersion = name + "/" + c.Runtime.Version
		}
		if c.Device != nil {
			ev.DeviceModel = c.Device.Model
		}
		if c.App != nil && c.App.AppVersion != "" {
			ev.Release = c.App.AppVersion
		}
	}
	if ev.DeviceModel == "" {
		ev.DeviceModel = re.ServerName
	}
	if re.Exception != nil && len(re.Exception.Values) > 0 {
		ex := re.Exception.Values[0]
		ev.exception = &ex
		ev.ErrorType = ex.Type
		if ex.Mechanism != nil {
			ev.Handled = ex.Mechanism.Handled
		}
	}
	if re.SDK != nil {
		ev.SDKName = re.SDK.Name
	}
	if re.User != nil {
		ev.UserID = scalarString(re.User.ID)
	}
	ev.Message = truncate(extractMessage(&re), maxMessage)
	ev.crumbs = parseRawBreadcrumbs(re.Breadcrumbs)
	ev.Breadcrumbs = make([]Breadcrumb, 0, len(ev.crumbs))
	for _, b := range ev.crumbs {
		cat := b.Category
		if cat == "" {
			cat = "default"
		}
		lvl := b.Level
		if lvl == "" {
			lvl = "info"
		}
		ts := ""
		if t := parseTimestamp(b.Timestamp); !t.IsZero() {
			ts = t.UTC().Format(time.RFC3339)
		}
		ev.Breadcrumbs = append(ev.Breadcrumbs, Breadcrumb{Timestamp: ts, Category: cat, Message: b.Message, Level: lvl})
	}
	return ev
}

func normalizeLevel(l string) string {
	switch l = strings.ToLower(strings.TrimSpace(l)); l {
	case "":
		return "error"
	case "warn":
		return "warning"
	case "critical":
		return "fatal"
	default:
		return l
	}
}

func extractMessage(re *rawEvent) string {
	if re.Logentry != nil {
		if re.Logentry.Formatted != "" {
			return re.Logentry.Formatted
		}
		if re.Logentry.Message != "" {
			return re.Logentry.Message
		}
	}
	if len(re.Message) > 0 {
		var s string
		if json.Unmarshal(re.Message, &s) == nil && s != "" {
			return s
		}
		var obj struct {
			Formatted string `json:"formatted"`
			Message   string `json:"message"`
		}
		if json.Unmarshal(re.Message, &obj) == nil {
			if obj.Formatted != "" {
				return obj.Formatted
			}
			if obj.Message != "" {
				return obj.Message
			}
		}
	}
	if re.Exception != nil && len(re.Exception.Values) > 0 {
		v := re.Exception.Values[0]
		t := v.Type
		if t == "" {
			t = "Error"
		}
		return t + ": " + v.Value
	}
	return "(no message)"
}

// parseTags accepts {"k": v} or [["k", v], …]. Values are stringified.
func parseTags(raw json.RawMessage) map[string]string {
	out := map[string]string{}
	if len(raw) == 0 {
		return out
	}
	var obj map[string]json.RawMessage
	if json.Unmarshal(raw, &obj) == nil {
		for k, v := range obj {
			out[k] = scalarString(v)
		}
		return out
	}
	var pairs [][]json.RawMessage
	if json.Unmarshal(raw, &pairs) == nil {
		for _, p := range pairs {
			if len(p) >= 2 {
				out[scalarString(p[0])] = scalarString(p[1])
			}
		}
	}
	return out
}

// parseRawBreadcrumbs accepts {"values": [...]} or a bare array; keeps the last 20.
func parseRawBreadcrumbs(raw json.RawMessage) []rawBreadcrumb {
	if len(raw) == 0 {
		return nil
	}
	var list []rawBreadcrumb
	if json.Unmarshal(raw, &list) != nil {
		var wrapped struct {
			Values []rawBreadcrumb `json:"values"`
		}
		if json.Unmarshal(raw, &wrapped) != nil {
			return nil
		}
		list = wrapped.Values
	}
	if len(list) > 20 {
		list = list[len(list)-20:]
	}
	return list
}

// scalarString renders a JSON scalar as text ("" for null/missing;
// objects/arrays stay as compact JSON).
func scalarString(raw json.RawMessage) string {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 || bytes.Equal(raw, []byte("null")) {
		return ""
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	return string(raw)
}

// parseTimestamp accepts RFC3339 (any offset), unix seconds or unix ms.
func parseTimestamp(raw json.RawMessage) time.Time {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 {
		return time.Time{}
	}
	var f float64
	if json.Unmarshal(raw, &f) == nil {
		if math.IsNaN(f) || math.IsInf(f, 0) || f <= 0 {
			return time.Time{}
		}
		if f < 1e12 { // seconds
			sec, frac := math.Modf(f)
			return time.Unix(int64(sec), int64(frac*1e9)).UTC()
		}
		return time.UnixMilli(int64(f)).UTC()
	}
	var s string
	if json.Unmarshal(raw, &s) != nil || strings.TrimSpace(s) == "" {
		return time.Time{}
	}
	for _, layout := range []string{time.RFC3339Nano, time.RFC3339, "2006-01-02T15:04:05.999999999", "2006-01-02T15:04:05", "2006-01-02 15:04:05.999999999Z07:00", "2006-01-02 15:04:05"} {
		if t, err := time.Parse(layout, s); err == nil {
			return t.UTC()
		}
	}
	return time.Time{}
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n])
}

// ── sessions ────────────────────────────────────────────────

type rawSession struct {
	Status  string          `json:"status"`
	Release string          `json:"release"`
	Started json.RawMessage `json:"started"`
	Attrs   *struct {
		Release string `json:"release"`
	} `json:"attrs"`
}

func parseSessions(body []byte, now time.Time) []Session {
	var items []rawSession
	var single rawSession
	if json.Unmarshal(body, &single) == nil && single.Status != "" {
		items = []rawSession{single}
	} else {
		var agg struct {
			Items []rawSession `json:"items"`
		}
		if json.Unmarshal(body, &agg) != nil {
			return nil
		}
		items = agg.Items
	}
	var out []Session
	for _, s := range items {
		rel := s.Release
		if rel == "" && s.Attrs != nil {
			rel = s.Attrs.Release
		}
		if rel == "" || s.Status == "" {
			continue
		}
		ts := parseTimestamp(s.Started)
		if ts.IsZero() {
			ts = now
		}
		out = append(out, Session{Release: rel, Status: s.Status, StartedAt: ts})
	}
	return out
}
