package sentry

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"
)

// Fingerprint groups events into issues from a set of frames (symbolicated
// when available). An SDK-provided fingerprint wins; otherwise the last five
// frames (closest to the crash point, line numbers stripped) plus platform
// and error type form the signature. The result is a stable 32-hex-char
// digest, so it is safe as a primary key and URL segment. "" means the
// event has nothing to group by (no exception, no SDK fingerprint).
func Fingerprint(e *Event, frames []Frame) string {
	if e.ErrorType == "" && len(e.SDKFingerprint) == 0 {
		return ""
	}
	var sig string
	if len(e.SDKFingerprint) > 0 {
		sig = "sdk:" + strings.Join(e.SDKFingerprint, "|")
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
				fn = f.InstrAddr // unsymbolicated native frame: address is the identity
			}
			if fn == "" {
				fn = "?"
			}
			if file == "" {
				file = "?"
			}
			parts = append(parts, baseName(file)+":"+fn)
		}
		sig = e.Platform + ":" + e.ErrorType + ":" + strings.Join(parts, "|")
	}
	sum := sha256.Sum256([]byte(sig))
	return hex.EncodeToString(sum[:16])
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
	var root *Frame
	for i := range frames {
		if frames[i].IsInApp() {
			root = &frames[i]
		}
	}
	if root == nil && len(frames) > 0 {
		root = &frames[len(frames)-1]
	}
	if root == nil {
		return ""
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
	if f.Package == "sentry" || strings.HasPrefix(f.Package, "sentry_") {
		return true
	}
	for _, p := range []string{f.Filename, f.AbsPath, f.Module} {
		if strings.HasPrefix(p, "package:sentry") || strings.Contains(p, "/sentry_sdk/") || strings.HasPrefix(p, "io.sentry.") || strings.HasPrefix(p, "Sentry.") || strings.Contains(p, "/@sentry/") {
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
