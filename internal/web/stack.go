package web

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/crashcartapp/crashcart/internal/db/sqlc"
	"github.com/crashcartapp/crashcart/internal/sentry"
)

// Stack is one exception rendered innermost-first.
type Stack struct {
	Type, Value    string
	Handled        *bool
	Mechanism      string
	Frames         []sentry.Frame // innermost first (the protocol stores innermost last)
	Unsymbolicated bool           // any frame with an address but no function
	Symbolicated   bool           // frames came from events.symbols
}

// FrameGroup is a run of frames with the same in-app-ness, so templates
// can collapse system frames under one toggle.
type FrameGroup struct {
	InApp  bool
	Frames []sentry.Frame
}

// Groups splits the frames into alternating in-app / system runs.
func (s Stack) Groups() []FrameGroup {
	var out []FrameGroup
	for _, f := range s.Frames {
		in := f.IsInApp()
		if n := len(out); n > 0 && out[n-1].InApp == in {
			out[n-1].Frames = append(out[n-1].Frames, f)
			continue
		}
		out = append(out, FrameGroup{InApp: in, Frames: []sentry.Frame{f}})
	}
	return out
}

// isUnsymbolicated flags a native frame that still carries only an address.
func isUnsymbolicated(f sentry.Frame) bool { return f.Function == "" && f.InstrAddr != "" }

// frameLocation renders "file:line" (or module / address) for a frame.
func frameLocation(f sentry.Frame) string {
	name := firstNonEmpty(f.Filename, firstNonEmpty(f.AbsPath, f.Module))
	switch {
	case name != "" && f.Lineno > 0 && f.Colno > 0:
		return fmt.Sprintf("%s:%d:%d", name, f.Lineno, f.Colno)
	case name != "" && f.Lineno > 0:
		return fmt.Sprintf("%s:%d", name, f.Lineno)
	case name != "":
		return name
	case f.Package != "":
		return f.Package
	}
	return f.InstrAddr
}

// stacksOf builds the display stacks of an event: the symbolicated frames
// (events.symbols) replace the primary exception's frames when present.
func stacksOf(e sqlc.Event) []Stack {
	ev := parsePayload(e)
	if ev == nil {
		return nil
	}
	var symbols []sentry.Frame
	if len(e.Symbols) > 0 {
		_ = json.Unmarshal(e.Symbols, &symbols)
	}
	var out []Stack
	for i, ex := range ev.Exceptions {
		frames := ex.Frames
		sym := false
		if i == 0 && len(symbols) > 0 {
			frames, sym = symbols, true
		}
		st := Stack{Type: ex.Type, Value: ex.Value, Handled: ex.Handled, Mechanism: ex.Mechanism, Symbolicated: sym}
		st.Frames = make([]sentry.Frame, 0, len(frames))
		for j := len(frames) - 1; j >= 0; j-- {
			st.Frames = append(st.Frames, frames[j])
			if isUnsymbolicated(frames[j]) {
				st.Unsymbolicated = true
			}
		}
		out = append(out, st)
	}
	return out
}

// parsePayload re-parses the stored Sentry event (never rewritten).
func parsePayload(e sqlc.Event) *sentry.Event {
	if len(e.Payload) == 0 {
		return nil
	}
	return sentry.ParseEvent(e.EventID, e.OccurredAt, e.Payload, time.Now().UTC())
}

// KV is one key/value line of a context group.
type KV struct{ K, V string }

// ContextGroup is one named context (device, os, app, …).
type ContextGroup struct {
	Name  string
	Items []KV
}

// payloadContexts extracts contexts.device / os / app (+ any other) and
// the user object from the raw payload as flat key/value lists.
func payloadContexts(raw json.RawMessage) (contexts []ContextGroup, user []KV) {
	var doc struct {
		Contexts map[string]map[string]any `json:"contexts"`
		User     map[string]any            `json:"user"`
	}
	if json.Unmarshal(raw, &doc) != nil {
		return nil, nil
	}
	names := make([]string, 0, len(doc.Contexts))
	for n := range doc.Contexts {
		names = append(names, n)
	}
	sort.SliceStable(names, func(i, j int) bool { return ctxRank(names[i]) < ctxRank(names[j]) })
	for _, n := range names {
		if items := flatten(doc.Contexts[n]); len(items) > 0 {
			contexts = append(contexts, ContextGroup{Name: n, Items: items})
		}
	}
	return contexts, flatten(doc.User)
}

func ctxRank(n string) string {
	switch n {
	case "device":
		return "0"
	case "os":
		return "1"
	case "app":
		return "2"
	}
	return "9" + n
}

// flatten renders scalar fields of m as sorted key/value pairs (nested
// objects are shown as compact JSON).
func flatten(m map[string]any) []KV {
	var out []KV
	for k, v := range m {
		var s string
		switch x := v.(type) {
		case nil:
			continue
		case string:
			s = x
		case float64:
			s = strings.TrimSuffix(fmt.Sprintf("%.3f", x), ".000")
		case bool:
			s = fmt.Sprint(x)
		default:
			b, _ := json.Marshal(x)
			s = string(b)
		}
		if s == "" {
			continue
		}
		out = append(out, KV{k, s})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].K < out[j].K })
	return out
}

// crumbsOf decodes the breadcrumbs column, newest first, capped at 30.
func crumbsOf(e sqlc.Event) []sentry.Breadcrumb {
	ev := parsePayload(e)
	if ev == nil {
		return nil
	}
	list := ev.Breadcrumbs
	// newest first
	for i, j := 0, len(list)-1; i < j; i, j = i+1, j-1 {
		list[i], list[j] = list[j], list[i]
	}
	return list
}

func prettyJSON(raw json.RawMessage) string {
	var buf strings.Builder
	var v any
	if json.Unmarshal(raw, &v) != nil {
		return string(raw)
	}
	enc := json.NewEncoder(&buf)
	enc.SetIndent("", "  ")
	enc.SetEscapeHTML(false)
	if enc.Encode(v) != nil {
		return string(raw)
	}
	return buf.String()
}

// formatClock extracts "14:03:22" from an RFC3339 string.
func formatClock(ts string) string {
	if i := strings.IndexByte(ts, 'T'); i >= 0 && len(ts) >= i+9 {
		return ts[i+1 : i+9]
	}
	return ts
}
