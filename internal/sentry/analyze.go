package sentry

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

// Fingerprint groups events into issues from a set of frames (symbolicated
// when available). An SDK-provided fingerprint wins; otherwise the last five
// frames (closest to the crash point, line numbers stripped) plus platform
// and error type form the signature. The result is a stable 32-hex-char
// digest, so it is safe as a primary key and URL segment. "" means the
// event has nothing to group by (no exception, no SDK fingerprint).
func Fingerprint(e *Event, frames []Frame) string {
	if e.ErrorType == "" && len(e.SDKFingerprint) == 0 && !groupableMessage(e) {
		return ""
	}
	var sig string
	if len(e.SDKFingerprint) > 0 {
		sig = "sdk:" + strings.Join(e.SDKFingerprint, "|")
	} else if e.ErrorType == "" {
		// Message events at error/fatal (Go panics, captureMessage("...",
		// "error")) group by their normalized text plus the stack, like
		// Sentry's message grouping.
		sig = "msg:" + e.Platform + ":" + normalizeMessage(e.Message) + ":" + strings.Join(frameSignature(e, frames), "|")
	} else {
		sig = e.Platform + ":" + e.ErrorType + ":" + strings.Join(frameSignature(e, frames), "|")
	}
	sum := sha256.Sum256([]byte(sig))
	return hex.EncodeToString(sum[:16])
}

// frameSignature renders the last five code frames (in-app when the SDK
// marks any, SDK-internal and pseudo frames dropped) as file:function.
// Unsymbolicated native frames use debug_id+offset, which is stable across
// runs (ASLR moves the raw address every time).
func frameSignature(e *Event, frames []Frame) []string {
	var sel, all []Frame
	for _, f := range frames {
		if isSDKFrame(f) {
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
	// Line numbers are normally left out so edits do not split issues; when
	// the whole stack collapses to one code frame (Dart's async entry
	// point, a script body) the line is all that tells throw sites apart.
	keepLine := len(all) <= 1
	parts := make([]string, 0, len(sel))
	for _, f := range sel {
		file := f.Filename
		if file == "" {
			file = f.Module
		}
		if i := strings.IndexByte(file, '?'); i >= 0 {
			file = file[:i]
		}
		fn := f.Function
		if fn == "" && f.InstrAddr != "" {
			fn = e.relativeAddr(f.InstrAddr)
		}
		if fn == "" {
			fn = "?"
		}
		if file == "" {
			file = "?"
		}
		if keepLine && f.Lineno > 0 {
			fn += ":" + strconv.Itoa(f.Lineno)
		}
		parts = append(parts, baseName(file)+":"+fn)
	}
	return parts
}

// groupableMessage: message-only events worth an issue.
func groupableMessage(e *Event) bool {
	return (e.Level == "fatal" || e.Level == "error") && e.Message != "" && e.Message != "(no message)"
}

var messageNoise = regexp.MustCompile(`0x[0-9a-fA-F]+|[0-9a-fA-F]{8,}|\d+`)

// normalizeMessage strips the parts of a message that vary per occurrence.
func normalizeMessage(m string) string {
	return messageNoise.ReplaceAllString(m, "#")
}

// relativeAddr maps an instruction address to "<debug_id>+<offset>" using
// debug_meta.images; the raw address is returned when no image contains it.
func (e *Event) relativeAddr(addr string) string {
	a, ok := parseHex(addr)
	if !ok {
		return addr
	}
	for _, im := range e.DebugImages {
		base, ok := parseHex(im.ImageAddr)
		if !ok || a < base || (im.ImageSize > 0 && a >= base+uint64(im.ImageSize)) {
			continue
		}
		id := im.DebugID
		if id == "" {
			id = baseName(im.CodeFile)
		}
		return id + "+0x" + strconv.FormatUint(a-base, 16)
	}
	return addr
}

func parseHex(s string) (uint64, bool) {
	s = strings.TrimPrefix(strings.TrimPrefix(s, "0x"), "0X")
	if s == "" {
		return 0, false
	}
	v, err := strconv.ParseUint(s, 16, 64)
	return v, err == nil
}

// IssueTitle is the human title for the event's issue: the exception type,
// the screen it happened in when known, and a short form of the message so
// two "Error" issues can be told apart in a list.
func (e *Event) IssueTitle() string {
	t := e.ErrorType
	if t == "" {
		// A message event (Go panic, captureMessage at error level).
		if m := strings.TrimSpace(e.Message); m != "" && m != "(no message)" {
			return truncate(strings.SplitN(m, "\n", 2)[0], 100)
		}
		t = "Unknown"
	}
	if e.Screen != "" {
		t += " in " + e.Screen
	}
	if len(e.Exceptions) > e.Primary {
		if v := strings.TrimSpace(e.Exceptions[e.Primary].Value); v != "" {
			if i := strings.IndexByte(v, '\n'); i >= 0 {
				v = v[:i]
			}
			t += ": " + truncate(v, 80)
		}
	}
	return t
}

// ErrorLocation is "File.ext:line" of the deepest in-app frame (or the
// innermost frame overall when nothing is marked in-app); "" without frames.
func ErrorLocation(frames []Frame) string {
	// Innermost in-app frame with a file and line wins; then any in-app
	// frame; then the innermost frame with a file and line; then whatever
	// is innermost. Runtime frames (__rustc, std) rarely carry a file.
	var located, inApp, anyLocated, last *Frame
	for i := range frames {
		f := &frames[i]
		if isSDKFrame(*f) || isRuntimeFrame(*f) {
			continue
		}
		last = f
		hasFile := (f.Filename != "" || f.AbsPath != "") && f.Lineno > 0
		if hasFile {
			anyLocated = f
		}
		if f.IsInApp() {
			inApp = f
			if hasFile {
				located = f
			}
		}
	}
	root := located
	for _, c := range []*Frame{inApp, anyLocated, last} {
		if root == nil {
			root = c
		}
	}
	if root == nil {
		return ""
	}
	if root.Function == "" && root.Filename == "" && root.AbsPath == "" && root.Module == "" {
		return "" // address-only native frame: unknown until symbolicated
	}
	name := root.Filename
	if name == "" {
		name = root.AbsPath
	}
	if name == "" {
		name = root.Module
	}
	if root.Lineno == 0 && root.Function != "" {
		return baseName(name) + ":" + root.Function
	}
	return fmt.Sprintf("%s:%d", baseName(name), root.Lineno)
}

// NeedsSymbolication reports whether frames carry raw addresses or look
// obfuscated enough that a symbol file would improve them.
func (e *Event) NeedsSymbolication() bool {
	for _, f := range e.Frames() {
		if f.InstrAddr != "" && f.Function == "" {
			return true
		}
	}
	switch e.Platform {
	case "android", "java":
		return len(e.Frames()) > 0 && len(e.DebugImages) > 0 // proguard uuid in debug_meta
	case "javascript", "node":
		for _, f := range e.Frames() {
			if f.Colno > 0 && strings.Contains(f.Filename, ".min.") {
				return true
			}
		}
	}
	return false
}

// isSDKFrame reports frames that belong to a Sentry SDK or are stack
// placeholders rather than code.
func isSDKFrame(f Frame) bool {
	// Placeholders such as "<asynchronous suspension>" (Dart puts it in the
	// file name with no function, some SDKs in the function with no file);
	// "<anonymous>" with a file:line is real code.
	pseudo := func(v string) bool { return strings.HasPrefix(v, "<") && strings.HasSuffix(v, ">") }
	if pseudo(f.Function) && f.Filename == "" && f.AbsPath == "" && f.Lineno == 0 {
		return true
	}
	if f.Function == "" && f.Lineno == 0 && (pseudo(f.Filename) || pseudo(f.AbsPath)) {
		return true
	}
	if f.Function == "" && f.Filename == "" && f.AbsPath == "" && f.Module == "" && f.InstrAddr == "" {
		return true // an empty frame carries no identity
	}
	if f.Module == "java.lang.Thread" && f.Function == "getStackTrace" {
		return true // the stack-capture call itself (Java message events)
	}
	if f.Package == "sentry" || strings.HasPrefix(f.Package, "sentry_") || strings.HasPrefix(f.Package, "sentry-") {
		return true
	}
	if strings.HasPrefix(f.Function, "sentry_") || strings.HasPrefix(f.Function, "sentry::") || strings.HasPrefix(f.Function, "sentry.") || strings.HasPrefix(f.Function, "io.sentry.") {
		return true // sentry_panic::…, sentry_core::…, sentry.CurrentHub…, io.sentry.Sentry.captureException
	}
	for _, p := range []string{f.Filename, f.AbsPath, f.Module} {
		if strings.HasPrefix(p, "package:sentry") || strings.Contains(p, "/sentry_sdk/") || strings.HasPrefix(p, "io.sentry.") || strings.HasPrefix(p, "Sentry.") ||
			strings.Contains(p, "/@sentry/") || strings.Contains(p, "/sentry-") || strings.Contains(p, "getsentry/sentry-go") || strings.HasPrefix(p, "sentry_") {
			return true
		}
	}
	return false
}

// isRuntimeFrame reports language-runtime frames that sit between the
// crash and user code (Rust's unwinder, Go's panic machinery).
func isRuntimeFrame(f Frame) bool {
	switch f.Package {
	case "__rustc", "std", "core", "alloc":
		return true
	}
	return strings.HasPrefix(f.Function, "__rustc::") || strings.HasPrefix(f.Function, "std::panicking::") ||
		strings.HasPrefix(f.Function, "core::panicking::") || f.Function == "gopanic" || strings.HasPrefix(f.Function, "runtime.")
}

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
