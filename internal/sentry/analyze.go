package sentry

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

// Fingerprint groups events into issues from a set of frames (symbolicated
// when available). An SDK-provided fingerprint wins; then the error type
// plus the last five code frames (line numbers stripped); message-only
// events at error/fatal (Go panics, captureMessage) group by normalized
// text plus stack, like Sentry's message grouping. The result is a stable
// 32-hex-char digest, safe as a primary key and URL segment. "" means the
// event has nothing to group by.
func Fingerprint(e *Event, frames []Frame) ID {
	sig := defaultSignature(e, frames)
	if len(e.SDKFingerprint) > 0 {
		// The SDK's own fingerprint; "{{ default }}" in it stands for the
		// default grouping (Sentry's convention: ["{{ default }}", key]
		// means "the usual issue, split by key").
		parts := make([]string, len(e.SDKFingerprint))
		hasDefault := false
		for i, p := range e.SDKFingerprint {
			if isDefaultToken(p) {
				parts[i] = "{{default}}=" + sig
				hasDefault = true
			} else {
				parts[i] = p
			}
		}
		if !(hasDefault && len(parts) == 1) { // ["{{ default }}"] alone is the default grouping itself
			sig = "sdk:" + strings.Join(parts, "|")
		}
	}
	if sig == "" {
		return ""
	}
	return DerivedID([]byte(sig))
}

// defaultSignature is the grouping signature without an SDK fingerprint:
// exception type + stack, or (error-level) message + stack. "" when there
// is nothing to group by.
func defaultSignature(e *Event, frames []Frame) string {
	switch {
	case e.ErrorType != "":
		return e.Platform + ":" + e.ErrorType + ":" + strings.Join(frameSignature(e, frames), "|")
	case groupableMessage(e):
		return "msg:" + e.Platform + ":" + normalizeMessage(e.Message) + ":" + strings.Join(frameSignature(e, frames), "|")
	}
	return ""
}

// isDefaultToken: "{{ default }}" in any spacing / case.
func isDefaultToken(s string) bool {
	s = strings.TrimSpace(s)
	if !strings.HasPrefix(s, "{{") || !strings.HasSuffix(s, "}}") {
		return false
	}
	return strings.EqualFold(strings.TrimSpace(s[2:len(s)-2]), "default")
}

// frameSignature renders the last five code frames (in-app when the SDK
// marks any; SDK, runtime and placeholder frames dropped) as
// file:function. Unsymbolicated native frames use debug_id+offset, which
// is stable across runs (ASLR moves the raw address every time).
func frameSignature(e *Event, frames []Frame) []string {
	var sel, all []Frame
	for _, f := range frames {
		if FrameKind(f) != KindCode {
			continue
		}
		all = append(all, f)
		if f.IsInApp() {
			sel = append(sel, f)
		}
	}
	if len(sel) == 0 {
		sel = all
	}
	if len(sel) > 5 {
		sel = sel[len(sel)-5:]
	}
	// Line numbers are normally left out so edits do not split issues; they
	// are kept when the functions cannot tell throw sites apart — a stack
	// that collapsed to one frame, or all-anonymous frames (Dart's
	// main.<fn>, a script body).
	keepLine := len(all) <= 1 || allAnonymous(sel)
	var images imageTable
	parts := make([]string, 0, len(sel))
	for _, f := range sel {
		file, _, _ := strings.Cut(f.Filename, "?")
		if file == "" {
			file = f.Module
		}
		fn := f.Function
		if fn == "" && f.InstrAddr != "" {
			if images == nil {
				images = e.imageTable()
			}
			// An address no debug image covers is left out of the
			// signature: it is the raw, ASLR-randomized value, different
			// on every run, and would make every crash its own issue.
			// The image's name (when the frame carries one) stands in.
			if fn = images.relative(f.InstrAddr); fn == "" && f.Package != "" {
				fn = baseName(f.Package) + "+?"
			}
		}
		if fn == "" {
			fn = "?"
		}
		if keepLine && f.Lineno > 0 {
			fn += ":" + strconv.Itoa(f.Lineno)
		}
		parts = append(parts, baseName(file)+":"+fn)
	}
	return parts
}

// allAnonymous: every function is a closure/anonymous placeholder.
func allAnonymous(frames []Frame) bool {
	for _, f := range frames {
		fn := f.Function
		if fn != "" && !strings.HasSuffix(fn, "<fn>") && !strings.HasSuffix(fn, "<anonymous>") && !strings.HasPrefix(fn, "<") && !strings.Contains(fn, "{closure") {
			return false
		}
	}
	return len(frames) > 0
}

// groupableMessage: message-only events worth an issue.
func groupableMessage(e *Event) bool {
	return (e.Level == "fatal" || e.Level == "error") && e.hasMessage()
}

func (e *Event) hasMessage() bool { return e.Message != "" && e.Message != noMessage }

var messageNoise = regexp.MustCompile(`0x[0-9a-fA-F]+|[0-9a-fA-F]{8,}|\d+`)

// normalizeMessage strips the parts of a message that vary per occurrence.
func normalizeMessage(m string) string {
	return messageNoise.ReplaceAllString(m, "#")
}

// imageTable is debug_meta.images with the bases parsed once per event.
type imageTable []struct {
	base, end uint64
	id        string
}

func (e *Event) imageTable() imageTable {
	t := make(imageTable, 0, len(e.DebugImages))
	for _, im := range e.DebugImages {
		base, ok := ParseHex(im.ImageAddr)
		if !ok {
			continue
		}
		id := im.DebugID
		if id == "" {
			id = baseName(im.CodeFile)
		}
		end := ^uint64(0)
		if im.ImageSize > 0 {
			end = base + uint64(im.ImageSize)
		}
		t = append(t, struct {
			base, end uint64
			id        string
		}{base, end, id})
	}
	return t
}

// relative maps an instruction address to "<debug_id>+<offset>"; "" when
// no image contains it (or it does not parse).
func (t imageTable) relative(addr string) string {
	a, ok := ParseHex(addr)
	if !ok {
		return ""
	}
	for _, im := range t {
		if a >= im.base && a < im.end {
			return im.id + "+0x" + strconv.FormatUint(a-im.base, 16)
		}
	}
	return ""
}

// ImageFor returns the index of the debug image containing addr, or -1.
func (e *Event) ImageFor(addr uint64) int {
	for i, im := range e.DebugImages {
		base, ok := ParseHex(im.ImageAddr)
		if ok && addr >= base && (im.ImageSize <= 0 || addr < base+uint64(im.ImageSize)) {
			return i
		}
	}
	return -1
}

// ParseHex parses an "0x…" (or bare) hexadecimal address.
func ParseHex(s string) (uint64, bool) {
	s = strings.TrimPrefix(strings.TrimPrefix(strings.TrimSpace(s), "0x"), "0X")
	if s == "" {
		return 0, false
	}
	v, err := strconv.ParseUint(s, 16, 64)
	return v, err == nil
}

// IssueTitle is the human title for the event's issue: the exception type,
// the transaction it happened in when known, and a short form of the message so
// two "Error" issues can be told apart in a list.
func (e *Event) IssueTitle() string {
	t := e.ErrorType
	if t == "" {
		if e.hasMessage() { // a message event (Go panic, captureMessage at error level)
			first, _, _ := strings.Cut(strings.TrimSpace(e.Message), "\n")
			return Truncate(first, 100)
		}
		t = "Unknown"
	}
	if len(e.Exceptions) > e.Primary {
		if v, _, _ := strings.Cut(strings.TrimSpace(e.Exceptions[e.Primary].Value), "\n"); v != "" {
			t += ": " + Truncate(v, 80)
		}
	}
	return t
}

// Culprit is "File.ext:line" of the innermost code frame, ranked:
// in-app with a file and line, then in-app, then any frame with a file and
// line, then whatever is innermost. "" without usable frames.
func Culprit(frames []Frame) string {
	var best *Frame
	bestRank := -1
	for i := range frames {
		f := &frames[i]
		if FrameKind(*f) != KindCode {
			continue
		}
		rank := 0
		if (f.Filename != "" || f.AbsPath != "") && f.Lineno > 0 {
			rank++
		}
		if f.IsInApp() {
			rank += 2
		}
		if rank >= bestRank {
			best, bestRank = f, rank
		}
	}
	if best == nil || (best.Function == "" && best.Filename == "" && best.AbsPath == "" && best.Module == "") {
		return "" // nothing, or an address-only native frame: unknown until symbolicated
	}
	name := best.Filename
	if name == "" {
		name = best.AbsPath
	}
	if name == "" {
		name = best.Module
	}
	if best.Lineno == 0 && best.Function != "" {
		return baseName(name) + ":" + best.Function
	}
	return fmt.Sprintf("%s:%d", baseName(name), best.Lineno)
}

// NeedsSymbolication reports whether frames carry raw addresses or look
// obfuscated enough that a symbol file would improve them.
func (e *Event) NeedsSymbolication() bool {
	for _, f := range e.Frames() {
		if f.InstrAddr != "" && f.Function == "" {
			return true
		}
	}
	// The same predicates the resolver applies (a cached lookup when the
	// mapping is not there): a ProGuard mapping can be matched by release
	// as well as by the debug_meta uuid, and a bundle is minified whether
	// or not its name says ".min." (webpack's "main.3f2a1c.js" does not).
	switch e.Platform {
	case "android", "java", "kotlin":
		return len(e.Frames()) > 0
	case "javascript", "node", "react-native":
		for _, f := range e.Frames() {
			if f.Colno > 0 {
				return true
			}
		}
	}
	return false
}

// Kind classifies a frame for grouping and location purposes.
type Kind int

const (
	KindCode        Kind = iota // application or library code
	KindPlaceholder             // "<asynchronous suspension>", empty frames
	KindSDK                     // a Sentry SDK's own frames
	KindRuntime                 // language runtime between the crash and user code
)

// Noise patterns, matched against the frame fields named in each row.
// Adding an SDK or runtime means adding a row here and nowhere else.
var frameNoise = []struct {
	kind   Kind
	field  func(Frame) string
	prefix bool // prefix match; else substring
	pat    string
}{
	{KindSDK, pkg, true, "sentry"}, // sentry, sentry_core, sentry-panic
	{KindSDK, fn, true, "sentry_"},
	{KindSDK, fn, true, "sentry::"},
	{KindSDK, fn, true, "sentry."}, // sentry.CurrentHub… (Go)
	{KindSDK, fn, true, "io.sentry."},
	{KindSDK, path, true, "package:sentry"},
	{KindSDK, path, true, "io.sentry."},
	{KindSDK, path, true, "Sentry."},
	{KindSDK, path, true, "sentry_"},
	{KindSDK, path, false, "/sentry_sdk/"},
	{KindSDK, path, false, "/@sentry/"},
	{KindSDK, path, false, "/sentry-"},
	{KindSDK, path, false, "getsentry/sentry-go"},
	{KindRuntime, pkg, true, "__rustc"},
	{KindRuntime, fn, true, "__rustc::"},
	{KindRuntime, fn, true, "std::panicking::"},
	{KindRuntime, fn, true, "core::panicking::"},
	{KindRuntime, fn, true, "runtime."}, // Go
}

func pkg(f Frame) string { return f.Package }
func fn(f Frame) string  { return f.Function }
func path(f Frame) string {
	return f.Filename + "\x00" + f.AbsPath + "\x00" + f.Module
}

// FrameKind classifies one frame. Placeholders are structural (no list):
// "<…>" names with no file/line, or a frame with no identity at all;
// "<anonymous>" with a file:line is real code.
func FrameKind(f Frame) Kind {
	hasFile := f.Filename != "" || f.AbsPath != ""
	switch {
	case isPlaceholder(f.Function) && !hasFile && f.Lineno == 0,
		f.Function == "" && f.Lineno == 0 && (isPlaceholder(f.Filename) || isPlaceholder(f.AbsPath)),
		f.Function == "" && !hasFile && f.Module == "" && f.InstrAddr == "":
		return KindPlaceholder
	case f.Module == "java.lang.Thread" && f.Function == "getStackTrace", // the stack-capture call itself
		f.Function == "gopanic",
		f.Package == "std" || f.Package == "core" || f.Package == "alloc":
		return KindRuntime
	}
	for _, n := range frameNoise {
		v := n.field(f)
		if n.prefix {
			// path joins three fields; a prefix must match at a field start.
			for _, part := range strings.Split(v, "\x00") {
				if strings.HasPrefix(part, n.pat) {
					return n.kind
				}
			}
		} else if strings.Contains(v, n.pat) {
			return n.kind
		}
	}
	return KindCode
}

func isPlaceholder(v string) bool { return strings.HasPrefix(v, "<") && strings.HasSuffix(v, ">") }

func baseName(p string) string {
	if p == "" {
		return "?"
	}
	if i := strings.LastIndexAny(p, "/\\"); i >= 0 {
		p = p[i+1:]
	}
	if p == "" {
		return "?"
	}
	return p
}
