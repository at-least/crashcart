// Package sentry parses the Sentry envelope protocol into what CrashCart
// stores. Unknown fields are preserved verbatim in Event.Raw (the payload
// column); everything else here is a projection of that raw JSON.
package sentry

import (
	"bytes"
	"encoding/json"
	"math"
	"strings"
	"time"
)

// Breadcrumb is a normalized Sentry breadcrumb (the last 20 are kept).
type Breadcrumb struct {
	Timestamp string         `json:"timestamp"` // RFC3339 UTC or ""
	Type      string         `json:"type,omitempty"`
	Category  string         `json:"category"`
	Message   string         `json:"message"`
	Level     string         `json:"level"`
	Data      map[string]any `json:"data,omitempty"`
}

// Frame is one stack frame as the SDK sent it (innermost frame LAST, as
// in the Sentry protocol).
type Frame struct {
	Filename   string `json:"filename,omitempty"`
	AbsPath    string `json:"abs_path,omitempty"`
	Function   string `json:"function,omitempty"`
	Module     string `json:"module,omitempty"`
	Package    string `json:"package,omitempty"`
	Lineno     int    `json:"lineno,omitempty"`
	Colno      int    `json:"colno,omitempty"`
	InApp      *bool  `json:"in_app,omitempty"`
	InstrAddr  string `json:"instruction_addr,omitempty"`
	ImageAddr  string `json:"image_addr,omitempty"`
	SymbolAddr string `json:"symbol_addr,omitempty"`
}

// IsInApp reports the SDK's in_app flag (false when absent).
func (f Frame) IsInApp() bool { return f.InApp != nil && *f.InApp }

// Exception is one entry of exception.values.
type Exception struct {
	Type        string
	Value       string
	Frames      []Frame
	Handled     *bool
	Mechanism   string
	ExceptionID *int // mechanism.exception_id / parent_id link chained exceptions
	ParentID    *int
}

// DebugImage is one debug_meta.images entry (used for dSYM lookup).
type DebugImage struct {
	Type      string `json:"type"`
	DebugID   string `json:"debug_id"`
	UUID      string `json:"uuid"` // proguard images (Android SDK) carry the id here
	CodeFile  string `json:"code_file"`
	ImageAddr string `json:"image_addr"`
	ImageSize int64  `json:"image_size"`
}

// Event is one parsed "event" envelope item.
type Event struct {
	EventID        ID
	Timestamp      time.Time
	Level          string
	Message        string
	Platform       string
	Environment    string
	Release        string
	OSVersion      string
	DeviceModel    string
	Screen         string
	ErrorType      string
	Handled        *bool // false = crash, true = caught, nil = no exception
	SDKName        string
	UserID         string
	Tags           map[string]string
	Breadcrumbs    []Breadcrumb
	Exceptions     []Exception // exception.values in SDK order
	DebugImages    []DebugImage
	SDKFingerprint []string
	Primary        int     // index into Exceptions of the root cause
	ThreadFrames   []Frame // crashed/current thread stack when there is no exception
	// Raw is the untouched item body.
	Raw []byte
}

// DeviceID is the `device_id` tag, if any.
func (e *Event) DeviceID() string { return e.Tags["device_id"] }

// IsCrash reports whether the event counts as a crash (fatal or unhandled).
func (e *Event) IsCrash() bool {
	return e.Level == "fatal" || (e.Handled != nil && !*e.Handled)
}

// Frames returns the primary exception's frames (innermost last), or the
// crashed/current thread's stack for events without an exception.
func (e *Event) Frames() []Frame {
	if len(e.Exceptions) == 0 {
		return e.ThreadFrames
	}
	return e.Exceptions[e.Primary].Frames
}

// threadFrames picks the crashed thread's stack, else the current one's.
func threadFrames(threads []rawThread) []Frame {
	var current []Frame
	for _, t := range threads {
		if t.Stacktrace == nil || len(t.Stacktrace.Frames) == 0 {
			continue
		}
		if t.Crashed {
			return t.Stacktrace.Frames
		}
		if t.Current && current == nil {
			current = t.Stacktrace.Frames
		}
	}
	return current
}

// primaryException picks the root cause of a chain and the exception that
// was actually thrown. SDKs that link values with mechanism.exception_id /
// parent_id (Java, .NET) may list the outer exception first; without ids
// the protocol order is oldest (root cause) to newest (thrown).
func primaryException(xs []Exception) (root, top int) {
	if len(xs) == 0 {
		return 0, 0
	}
	linked := false
	for _, x := range xs {
		if x.ExceptionID != nil {
			linked = true
			break
		}
	}
	if !linked {
		return 0, len(xs) - 1
	}
	isParent := map[int]bool{}
	for _, x := range xs {
		if x.ParentID != nil {
			isParent[*x.ParentID] = true
		}
	}
	root, top = -1, -1
	for i, x := range xs {
		if x.ParentID == nil && top < 0 {
			top = i
		}
		if x.ExceptionID != nil && !isParent[*x.ExceptionID] {
			root = i // the deepest cause: nobody's parent
		}
	}
	if top < 0 {
		top = 0
	}
	if root < 0 {
		root = top
	}
	return root, top
}

// Session is one Sentry session (release health) item, or one row of a
// sessions aggregate (then Count > 1 is possible).
type Session struct {
	SID         string // session id; updates of one session share it ("" for aggregates)
	Release     string
	Environment string
	Status      string // ok | exited | crashed | errored | abnormal
	StartedAt   time.Time
	Count       int
}

// Envelope is the parsed result of one POST body.
type Envelope struct {
	Events   []*Event
	Sessions []Session
	Dropped  int // items of types CrashCart does not store
	Invalid  int // event items that did not parse (logged and reported to the SDK)
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
		nl = len(body)
	}
	var hdr envelopeHeader
	if json.Unmarshal(body[:nl], &hdr) != nil {
		return env
	}
	fallbackTS := parseTimestamp(hdr.SentAt)
	if fallbackTS.IsZero() {
		fallbackTS = now
	}
	rest := []byte{}
	if nl < len(body) {
		rest = body[nl+1:]
	}
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
			if ev := ParseEvent(hdr.EventID, fallbackTS, itemBody, now); ev != nil {
				env.Events = append(env.Events, ev)
			} else {
				env.Invalid++
			}
		case "session", "sessions":
			env.Sessions = append(env.Sessions, parseSessions(itemBody, now)...)
		case "transaction", "attachment", "profile", "replay_event", "replay_recording",
			"client_report", "check_in", "log", "statsd", "feedback", "user_report", "span":
			env.Dropped++
		default:
			env.Dropped++
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

type rawException struct {
	Type       string `json:"type"`
	Value      string `json:"value"`
	Stacktrace *struct {
		Frames []Frame `json:"frames"`
	} `json:"stacktrace"`
	Mechanism *struct {
		Type        string `json:"type"`
		Handled     *bool  `json:"handled"`
		ExceptionID *int   `json:"exception_id"`
		ParentID    *int   `json:"parent_id"`
	} `json:"mechanism"`
}

type rawThread struct {
	Crashed    bool `json:"crashed"`
	Current    bool `json:"current"`
	Stacktrace *struct {
		Frames []Frame `json:"frames"`
	} `json:"stacktrace"`
}

// valuesOf accepts the protocol's two spellings of a value list:
// {"values": [...]} and the bare-array shorthand [...] (sentry-go).
type valuesOf[T any] struct {
	Values []T
}

func (v *valuesOf[T]) UnmarshalJSON(b []byte) error {
	b = bytes.TrimSpace(b)
	if len(b) == 0 || bytes.Equal(b, []byte("null")) {
		return nil
	}
	if b[0] == '[' {
		return json.Unmarshal(b, &v.Values)
	}
	var obj struct {
		Values []T `json:"values"`
	}
	if err := json.Unmarshal(b, &obj); err != nil {
		return err
	}
	v.Values = obj.Values
	return nil
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
	Platform    string                  `json:"platform"`
	Environment string                  `json:"environment"`
	Release     string                  `json:"release"`
	Transaction string                  `json:"transaction"`
	ServerName  string                  `json:"server_name"`
	Tags        json.RawMessage         `json:"tags"`
	Breadcrumbs valuesOf[rawBreadcrumb] `json:"breadcrumbs"`
	Fingerprint []string                `json:"fingerprint"`
	Contexts    *struct {
		Device *struct {
			Model string `json:"model"`
		} `json:"device"`
		OS *struct {
			Name    string `json:"name"`
			Version string `json:"version"`
		} `json:"os"`
		App *struct {
			AppVersion string `json:"app_version"`
			AppBuild   string `json:"app_build"`
		} `json:"app"`
		Runtime *struct {
			Name    string `json:"name"`
			Version string `json:"version"`
		} `json:"runtime"`
	} `json:"contexts"`
	Exception valuesOf[rawException] `json:"exception"`
	Threads   valuesOf[rawThread]    `json:"threads"`
	DebugMeta *struct {
		Images []DebugImage `json:"images"`
	} `json:"debug_meta"`
	SDK *struct {
		Name string `json:"name"`
	} `json:"sdk"`
	User *struct {
		ID json.RawMessage `json:"id"`
	} `json:"user"`
}

const (
	maxMessage = 500
	noMessage  = "(no message)"
)

// ParseEvent parses a single event JSON body (envelope item or the legacy
// /store/ endpoint). Returns nil for anything that is not a JSON object.
func ParseEvent(headerEventID string, fallbackTS time.Time, body []byte, now time.Time) *Event {
	trimmed := bytes.TrimSpace(body)
	if len(trimmed) == 0 || trimmed[0] != '{' {
		return nil
	}
	var re rawEvent
	if json.Unmarshal(trimmed, &re) != nil {
		return nil
	}
	ev := &Event{
		Raw:            trimmed,
		Level:          normalizeLevel(re.Level),
		Platform:       re.Platform,
		Environment:    re.Environment,
		Screen:         re.Transaction,
		Release:        re.Release,
		Tags:           parseTags(re.Tags),
		SDKFingerprint: re.Fingerprint,
	}
	ev.Timestamp = parseTimestamp(re.Timestamp)
	if ev.Timestamp.IsZero() {
		ev.Timestamp = fallbackTS
	}
	// Clamp absurd client clocks: more than 1 h in the future becomes now.
	if ev.Timestamp.After(now.Add(time.Hour)) {
		ev.Timestamp = now
	}
	// The SDK's event_id (a 32-hex UUID per the protocol), the envelope
	// header's, or — no usable id — one derived from the body, so a resend
	// still lands on the same row.
	if id, ok := ParseID(re.EventID); ok {
		ev.EventID = id
	} else if id, ok := ParseID(headerEventID); ok {
		ev.EventID = id
	} else {
		ev.EventID = DerivedID(trimmed)
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
		if c.App != nil && c.App.AppVersion != "" && ev.Release == "" {
			ev.Release = c.App.AppVersion
		}
	}
	if ev.DeviceModel == "" {
		ev.DeviceModel = re.ServerName
	}
	for _, x := range re.Exception.Values {
		ex := Exception{Type: x.Type, Value: x.Value}
		if x.Stacktrace != nil {
			ex.Frames = x.Stacktrace.Frames
		}
		if x.Mechanism != nil {
			ex.Handled = x.Mechanism.Handled
			ex.Mechanism = x.Mechanism.Type
			ex.ExceptionID, ex.ParentID = x.Mechanism.ExceptionID, x.Mechanism.ParentID
		}
		ev.Exceptions = append(ev.Exceptions, ex)
	}
	// The root cause names the issue; the thrown (outermost) exception
	// carries the handled flag. A cause without its own stack borrows the
	// crashed thread's.
	if len(ev.Exceptions) > 0 {
		root, top := primaryException(ev.Exceptions)
		ev.Primary = root
		ev.ErrorType = ev.Exceptions[root].Type
		ev.Handled = ev.Exceptions[top].Handled
		if ev.Handled == nil {
			ev.Handled = ev.Exceptions[root].Handled
		}
		if ev.Handled == nil {
			// Every SDK marks its unhandled paths with handled:false
			// explicitly; a captureException without a mechanism is a
			// caught exception.
			handled := true
			ev.Handled = &handled
		}
		if len(ev.Exceptions[root].Frames) == 0 {
			// .NET sends a never-thrown exception without a stack and the
			// capturing thread's stack under threads; native SDKs mark the
			// crashed thread.
			ev.Exceptions[root].Frames = threadFrames(re.Threads.Values)
		}
	} else {
		ev.ThreadFrames = threadFrames(re.Threads.Values)
	}
	if re.DebugMeta != nil {
		ev.DebugImages = re.DebugMeta.Images
		for i := range ev.DebugImages {
			if ev.DebugImages[i].DebugID == "" {
				ev.DebugImages[i].DebugID = ev.DebugImages[i].UUID
			}
		}
	}
	if re.SDK != nil {
		ev.SDKName = re.SDK.Name
	}
	if re.User != nil {
		ev.UserID = scalarString(re.User.ID)
	}
	ev.Message = truncate(extractMessage(&re, ev.Primary), maxMessage)
	for _, b := range lastBreadcrumbs(re.Breadcrumbs.Values) {
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
		ev.Breadcrumbs = append(ev.Breadcrumbs, Breadcrumb{Timestamp: ts, Type: b.Type, Category: cat, Message: b.Message, Level: lvl, Data: b.Data})
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
	case "fatal", "error", "warning", "info", "debug":
		return l
	default:
		return "info"
	}
}

func extractMessage(re *rawEvent, primary int) string {
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
	if len(re.Exception.Values) > primary {
		v := re.Exception.Values[primary]
		t := v.Type
		if t == "" {
			t = "Error"
		}
		if v.Value == "" {
			return t
		}
		return t + ": " + v.Value
	}
	return noMessage
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

// lastBreadcrumbs keeps the newest 20.
func lastBreadcrumbs(list []rawBreadcrumb) []rawBreadcrumb {
	if len(list) > 20 {
		return list[len(list)-20:]
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
	SID     string          `json:"sid"`
	Status  string          `json:"status"`
	Release string          `json:"release"`
	Started json.RawMessage `json:"started"`
	Attrs   *struct {
		Release     string `json:"release"`
		Environment string `json:"environment"`
	} `json:"attrs"`
	Environment string `json:"environment"`
}

type rawSessionAggregate struct {
	Started  json.RawMessage `json:"started"`
	Exited   int             `json:"exited"`
	Errored  int             `json:"errored"`
	Crashed  int             `json:"crashed"`
	Abnormal int             `json:"abnormal"`
}

func parseSessions(body []byte, now time.Time) []Session {
	var out []Session
	// Single session item.
	var single rawSession
	if json.Unmarshal(body, &single) == nil && single.Status != "" {
		if s, ok := sessionFrom(single, now); ok {
			out = append(out, s)
		}
		return out
	}
	// Aggregate: {"aggregates":[{started,exited,crashed,errored,abnormal}], "attrs":{release,environment}}
	var agg struct {
		Aggregates []rawSessionAggregate `json:"aggregates"`
		Attrs      *struct {
			Release     string `json:"release"`
			Environment string `json:"environment"`
		} `json:"attrs"`
		Items []rawSession `json:"items"`
	}
	if json.Unmarshal(body, &agg) != nil {
		return nil
	}
	for _, it := range agg.Items {
		if s, ok := sessionFrom(it, now); ok {
			out = append(out, s)
		}
	}
	if agg.Attrs == nil || agg.Attrs.Release == "" {
		return out
	}
	for _, a := range agg.Aggregates {
		ts := parseTimestamp(a.Started)
		if ts.IsZero() {
			ts = now
		}
		for status, n := range map[string]int{"exited": a.Exited, "errored": a.Errored, "crashed": a.Crashed, "abnormal": a.Abnormal} {
			if n > 0 {
				out = append(out, Session{Release: agg.Attrs.Release, Environment: agg.Attrs.Environment, Status: status, StartedAt: ts, Count: n})
			}
		}
	}
	return out
}

func sessionFrom(s rawSession, now time.Time) (Session, bool) {
	rel, env := s.Release, s.Environment
	if s.Attrs != nil {
		if rel == "" {
			rel = s.Attrs.Release
		}
		if env == "" {
			env = s.Attrs.Environment
		}
	}
	if rel == "" || s.Status == "" {
		return Session{}, false
	}
	ts := parseTimestamp(s.Started)
	if ts.IsZero() {
		ts = now
	}
	return Session{SID: s.SID, Release: rel, Environment: env, Status: s.Status, StartedAt: ts, Count: 1}, true
}
