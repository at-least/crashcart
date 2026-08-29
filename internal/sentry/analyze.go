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
		// Prefer in-app frames; fall back to all frames when the SDK marks
		// none. SDK-internal and pseudo frames never contribute — Dart
		// marks its own package in_app and pads async stacks with
		// "<asynchronous suspension>", which would make every error from
		// one entry point look alike.
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
	var root, last *Frame
	for i := range frames {
		if isSDKFrame(frames[i]) {
			continue
		}
		last = &frames[i]
		if frames[i].IsInApp() {
			root = &frames[i]
		}
	}
	if root == nil {
		root = last
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
	if strings.HasPrefix(f.Function, "<") && strings.HasSuffix(f.Function, ">") { // <asynchronous suspension>
		return true
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
