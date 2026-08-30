// Package sentry parses the Sentry envelope protocol into what CrashCart
// stores. Unknown fields are preserved verbatim in Event.Raw (the payload
// column); everything else here is a projection of that raw JSON.
package sentry

import (
	"bytes"
	"encoding/json"
	"errors"
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
	Transaction    string
	ErrorType      string
	Handled        *bool // exception.mechanism.handled: false = unhandled (Sentry's "Unhandled"), true = handled, nil = no mechanism (neither)
	SDKName        string
	UserID         string
	Tags           map[string]string
	Breadcrumbs    []Breadcrumb
	Exceptions     []Exception // exception.values in SDK order
	DebugImages    []DebugImage
	SDKFingerprint []string
	Primary        int     // index into Exceptions of the main exception: the one thrown last (Sentry's values[-1])
	ThreadFrames   []Frame // crashed/current thread stack when there is no exception
	// Clamped: the SDK's timestamp was replaced by the server's (a clock
	// far off); a resend gets a different one, so dedupe cannot use it.
	Clamped bool
	// Raw is the untouched item body.
	Raw []byte
}

// DeviceID is the `device_id` tag, if any — a CrashCart convention (the
// SDKs do not send a device id; an app sets this tag for the device page).
func (e *Event) DeviceID() string { return e.Tags["device_id"] }

// IsUnhandled is Sentry's "Unhandled": the SDK caught the error in a
// last-resort handler (a crash, an uncaught exception, an unhandled
// rejection) — exception.mechanism.handled = false. level is severity
// and says nothing about it; without a mechanism the event is neither
// (Sentry sets no handled tag then).
func (e *Event) IsUnhandled() bool {
	return e.Handled != nil && !*e.Handled
}

// Frames returns the main exception's frames (innermost last), or the
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

// mainException picks the exception that was actually thrown — the one
// Sentry titles the issue with and takes handled from. The protocol lists
// values oldest (root cause) to newest (thrown), so it is the last one;
// SDKs that link values with mechanism.exception_id / parent_id (Java,
// .NET) may list the outer exception first, and then it is the one
// without a parent.
func mainException(xs []Exception) int {
	if len(xs) == 0 {
		return 0
	}
	for i, x := range xs {
		if x.ExceptionID != nil {
			if x.ParentID == nil {
				return i
			}
		} else if i == len(xs)-1 {
			return i
		}
	}
	return len(xs) - 1
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
			if len(env.Events) > MaxEvents {
				env.Dropped++ // over the limit already: the request is refused, do not parse the rest
				continue
			}
			if ev := ParseEvent(hdr.EventID, fallbackTS, itemBody, now); ev != nil {
				env.Events = append(env.Events, ev)
			} else {
				env.Invalid++
			}
		case "session", "sessions":
			if len(env.Sessions) >= MaxSessions {
				env.Dropped++
				continue
			}
			ss := parseSessions(itemBody, now)
			if n := MaxSessions - len(env.Sessions); len(ss) > n {
				env.Dropped += len(ss) - n
				ss = ss[:n]
			}
			env.Sessions = append(env.Sessions, ss...)
		case "transaction", "attachment", "profile", "replay_event", "replay_recording",
			"client_report", "check_in", "log", "statsd", "feedback", "user_report", "span":
			env.Dropped++
		default:
			env.Dropped++
		}
	}
	return env
}

// Limits on one envelope. Parse stops parsing event items once it holds
// MaxEvents+1 (enough for ingest to refuse the request) and session rows
// at MaxSessions (the rest are dropped): a 20 MB body must not become
// hundreds of thousands of parsed events before it is rejected.
const (
	MaxEvents   = 500
	MaxSessions = 5000
)

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
		return typeErrorsDropped(json.Unmarshal(b, &v.Values))
	}
	var obj struct {
		Values []T `json:"values"`
	}
	if err := typeErrorsDropped(json.Unmarshal(b, &obj)); err != nil {
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
	ServerName  string                  `json:"server_name"` // becomes the server_name tag, as in Sentry
	Tags        json.RawMessage         `json:"tags"`
	Breadcrumbs valuesOf[rawBreadcrumb] `json:"breadcrumbs"`
	Fingerprint []string                `json:"fingerprint"`
	Contexts    *struct {
		Device *struct {
			Model string `json:"model"`
		} `json:"device"`
		OS *struct {
			Version string `json:"version"`
		} `json:"os"`
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
	if err := json.Unmarshal(trimmed, &re); err != nil {
		// A field of the wrong type (a numeric release, "lineno":"142")
		// is dropped by the decoder, which still fills the rest: keep the
		// event, as Relay would. Only unparseable JSON is refused.
		var te *json.UnmarshalTypeError
		if !errors.As(err, &te) {
			return nil
		}
	}
	ev := &Event{
		Raw:            trimmed,
		Level:          normalizeLevel(re.Level),
		Platform:       re.Platform,
		Environment:    re.Environment,
		Transaction:    re.Transaction,
		Release:        re.Release,
		Tags:           parseTags(re.Tags),
		SDKFingerprint: re.Fingerprint,
	}
	ev.Timestamp = parseTimestamp(re.Timestamp)
	if ev.Timestamp.IsZero() {
		ev.Timestamp = fallbackTS
	}
	ev.Timestamp, ev.Clamped = clampFuture(ev.Timestamp, now)
	// Microseconds, like the TIMESTAMPTZ it is stored as, so the stored
	// occurred_at equals this value exactly.
	ev.Timestamp = ev.Timestamp.UTC().Truncate(time.Microsecond)
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
	// The columns are the Sentry fields of the same name, nothing else:
	// no release from contexts.app, no OS from contexts.runtime, no device
	// from server_name. server_name becomes a tag, as Sentry makes it.
	if c := re.Contexts; c != nil {
		if c.OS != nil {
			ev.OSVersion = c.OS.Version
		}
		if c.Device != nil {
			ev.DeviceModel = c.Device.Model
		}
	}
	if re.ServerName != "" {
		if _, ok := ev.Tags["server_name"]; !ok {
			ev.Tags["server_name"] = re.ServerName
		}
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
	// The exception thrown last (Sentry's main exception) names the issue
	// and carries the handled flag; its causes are shown under it. One
	// without its own stack borrows the crashed thread's.
	if len(ev.Exceptions) > 0 {
		main := mainException(ev.Exceptions)
		ev.Primary = main
		ev.ErrorType = ev.Exceptions[main].Type
		ev.Handled = ev.Exceptions[main].Handled
		if len(ev.Exceptions[main].Frames) == 0 {
			// .NET sends a never-thrown exception without a stack and the
			// capturing thread's stack under threads; native SDKs mark the
			// crashed thread.
			ev.Exceptions[main].Frames = threadFrames(re.Threads.Values)
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
	ev.Message = Truncate(extractMessage(&re, ev.Primary), maxMessage)
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
	ev.sanitize()
	return ev
}

// typeErrorsDropped keeps the decoder's best effort: a field of the wrong
// JSON type is left zero, the rest of the document is used.
func typeErrorsDropped(err error) error {
	var te *json.UnmarshalTypeError
	if errors.As(err, &te) {
		return nil
	}
	return err
}

// Field bounds (runes), Relay's limits where it has them: a TEXT column
// that is indexed refuses values past ~2.7 KB, and one such string would
// fail the whole envelope — which the SDK would then resend forever.
const (
	maxRelease     = 200
	maxEnvironment = 64
	maxScreen      = 200 // event.transaction
	maxUserID      = 128
	maxErrorType   = 200
	maxDevice      = 200 // device model, OS version
	maxSDKName     = 100
	maxPlatform    = 64
	maxTagKey      = 32
	maxTagValue    = 200
	maxFrameField  = 1024
	maxSessionID   = 128
)

// clean drops NUL — Postgres TEXT and JSONB refuse it, and "\u0000" is
// valid JSON — and bounds the length.
func clean(s string, max int) string {
	if strings.IndexByte(s, 0) >= 0 {
		s = strings.ReplaceAll(s, "\x00", "")
	}
	return Truncate(s, max)
}

// sanitize applies clean to every string that reaches a column or a JSONB
// value (tags, symbols). Raw is untouched: it is stored as bytes.
func (e *Event) sanitize() {
	e.Platform = clean(e.Platform, maxPlatform)
	e.Environment = clean(e.Environment, maxEnvironment)
	e.Transaction = clean(e.Transaction, maxScreen)
	e.Release = clean(e.Release, maxRelease)
	e.Message = clean(e.Message, maxMessage)
	e.ErrorType = clean(e.ErrorType, maxErrorType)
	e.DeviceModel = clean(e.DeviceModel, maxDevice)
	e.OSVersion = clean(e.OSVersion, maxDevice)
	e.SDKName = clean(e.SDKName, maxSDKName)
	e.UserID = clean(e.UserID, maxUserID)
	for k, v := range e.Tags {
		ck := clean(k, maxTagKey+1)
		if ck == "" || len([]rune(ck)) > maxTagKey { // an over-long key is dropped (Relay does the same), not merged with another
			delete(e.Tags, k)
			continue
		}
		if ck != k {
			delete(e.Tags, k)
		}
		e.Tags[ck] = clean(v, maxTagValue)
	}
	for i := range e.Exceptions {
		x := &e.Exceptions[i]
		x.Type = clean(x.Type, maxErrorType)
		x.Value = clean(x.Value, maxMessage)
		x.Mechanism = clean(x.Mechanism, maxErrorType)
		for j := range x.Frames {
			f := &x.Frames[j]
			f.Filename = clean(f.Filename, maxFrameField)
			f.AbsPath = clean(f.AbsPath, maxFrameField)
			f.Function = clean(f.Function, maxFrameField)
			f.Module = clean(f.Module, maxFrameField)
			f.Package = clean(f.Package, maxFrameField)
			f.InstrAddr = clean(f.InstrAddr, maxFrameField)
			f.ImageAddr = clean(f.ImageAddr, maxFrameField)
			f.SymbolAddr = clean(f.SymbolAddr, maxFrameField)
		}
	}
}

// clampFuture: a timestamp more than a minute ahead of the server
// (Relay's max_secs_in_future) is a wrong clock; the event is taken as
// happening now. (The mirror rule for the past lives in ingest, which
// knows the retention window.)
func clampFuture(t, now time.Time) (time.Time, bool) {
	if t.After(now.Add(time.Minute)) {
		return now, true
	}
	return t, false
}

// normalizeLevel accepts the spellings Relay does (warn, critical, log)
// and defaults to error, the protocol's default level.
func normalizeLevel(l string) string {
	switch l = strings.ToLower(strings.TrimSpace(l)); l {
	case "warn":
		return "warning"
	case "critical":
		return "fatal"
	case "log":
		return "info"
	case "fatal", "error", "warning", "info", "debug":
		return l
	default:
		return "error"
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
		// Out of any plausible range (1e15 ms is the year 33658): a value
		// the float→int64 conversion could not represent, or garbage.
		// Zero means "no timestamp", and the fallback applies.
		if math.IsNaN(f) || math.IsInf(f, 0) || f <= 0 || f >= 1e15 {
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

func Truncate(s string, n int) string {
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
	Errors  int             `json:"errors"` // errors seen in the session: > 0 makes an ok/exited session "errored"
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
	rel, env := clean(agg.Attrs.Release, maxRelease), clean(agg.Attrs.Environment, maxEnvironment)
	for _, a := range agg.Aggregates {
		ts := parseTimestamp(a.Started)
		if ts.IsZero() {
			ts = now
		}
		ts, _ = clampFuture(ts, now)
		// A fixed order: the rows are upserted in this order, and the
		// hour rows they lock must be taken in one order by every writer.
		for _, sc := range [...]struct {
			status string
			n      int
		}{{"exited", a.Exited}, {"errored", a.Errored}, {"crashed", a.Crashed}, {"abnormal", a.Abnormal}} {
			if sc.n > 0 {
				out = append(out, Session{Release: rel, Environment: env, Status: sc.status, StartedAt: ts, Count: min(sc.n, math.MaxInt32)}) // the column is INTEGER
			}
		}
	}
	return out
}

// validSessionStatus mirrors the session_status enum. The wire statuses
// are ok, exited, crashed, abnormal; errored is derived (errors > 0) and
// accepted as well.
var validSessionStatus = map[string]bool{"ok": true, "exited": true, "crashed": true, "errored": true, "abnormal": true}

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
	rel, env = clean(rel, maxRelease), clean(env, maxEnvironment)
	if rel == "" || !validSessionStatus[s.Status] {
		return Session{}, false // the status is a Postgres enum: an unknown value would fail the whole envelope
	}
	ts := parseTimestamp(s.Started)
	if ts.IsZero() {
		ts = now
	}
	ts, _ = clampFuture(ts, now)
	status := s.Status
	if s.Errors > 0 && (status == "ok" || status == "exited") {
		status = "errored" // Sentry's errored session: it did not crash, but saw errors
	}
	return Session{SID: clean(s.SID, maxSessionID), Release: rel, Environment: env, Status: status, StartedAt: ts, Count: 1}, true
}
