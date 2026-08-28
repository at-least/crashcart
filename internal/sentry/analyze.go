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
		// Prefer in-app frames; fall back to all frames when the SDK marks none.
		var sel []Frame
		for _, f := range frames {
			if f.IsInApp() {
				sel = append(sel, f)
			}
		}
		if len(sel) == 0 {
			sel = frames
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

// IssueTitle is the human title for the event's issue.
func (e *Event) IssueTitle() string {
	t := e.ErrorType
	if t == "" {
		t = "Unknown"
	}
	if e.Screen != "" {
		return t + " in " + e.Screen
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
