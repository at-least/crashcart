package sentry

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"
)

// Fingerprint groups events into issues. An SDK-provided fingerprint wins;
// otherwise the last five frames (closest to the crash point, line numbers
// stripped) plus platform and error type form the signature. The result is
// a stable 32-hex-char digest, so it's safe as a primary key and URL segment.
func (e *Event) Fingerprint() string {
	if e.ErrorType == "" && len(e.sdkFP) == 0 {
		return ""
	}
	var sig string
	if len(e.sdkFP) > 0 {
		sig = "sdk:" + strings.Join(e.sdkFP, "|")
	} else {
		frames := e.frames()
		if len(frames) > 5 {
			frames = frames[len(frames)-5:]
		}
		parts := make([]string, 0, len(frames))
		for _, f := range frames {
			file := f.Filename
			if i := strings.IndexByte(file, '?'); i >= 0 {
				file = file[:i]
			}
			fn := f.Function
			if fn == "" {
				fn = f.Sym
			}
			if fn == "" {
				fn = "?"
			}
			if file == "" {
				file = "?"
			}
			parts = append(parts, file+":"+fn)
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

func (e *Event) frames() []rawFrame {
	if e.exception == nil || e.exception.Stacktrace == nil {
		return nil
	}
	return e.exception.Stacktrace.Frames
}

// Frame is an analyzed stack frame.
type Frame struct {
	Filename string `json:"filename"`
	Function string `json:"function"`
	Lineno   int    `json:"lineno"`
	InApp    bool   `json:"in_app"`
}

// Analysis is the structured diagnostic extracted from a stack trace.
type Analysis struct {
	ErrorLocation string   // "CartFragment.java:142" or ""
	RootFrame     *Frame   // deepest in-app frame (or last frame overall)
	AppFrameCount int      //
	TotalFrames   int      //
	UserJourney   []string // breadcrumbs rendered as a timeline
}

// Analyze extracts the root cause (deepest in-app frame) and a user journey.
func (e *Event) Analyze() Analysis {
	var a Analysis
	frames := e.frames()
	a.TotalFrames = len(frames)
	var root *rawFrame
	for i := range frames {
		if frames[i].InApp != nil && *frames[i].InApp {
			a.AppFrameCount++
			root = &frames[i]
		}
	}
	inApp := root != nil
	if root == nil && len(frames) > 0 {
		root = &frames[len(frames)-1]
	}
	if root != nil {
		name := root.Filename
		if name == "" {
			name = root.AbsPath
		}
		name = baseName(name)
		fn := root.Function
		if fn == "" {
			fn = root.Module
		}
		if fn == "" {
			fn = "?"
		}
		a.RootFrame = &Frame{Filename: name, Function: fn, Lineno: root.Lineno, InApp: inApp}
		a.ErrorLocation = fmt.Sprintf("%s:%d", name, root.Lineno)
	}
	for _, b := range e.crumbs {
		cat := b.Category
		if cat == "" {
			cat = b.Type
		}
		if cat == "" {
			cat = "unknown"
		}
		var line string
		switch cat {
		case "navigation", "routing":
			dest := str(b.Data["to"])
			if dest == "" {
				dest = str(b.Data["from"])
			}
			if dest == "" {
				dest = b.Message
			}
			line = "→ " + dest
		case "click", "tap", "ui":
			line = "👆 " + b.Message
		case "http", "fetch":
			url := str(b.Data["url"])
			if url == "" {
				url = b.Message
			}
			line = "🌐 " + str(b.Data["method"]) + " " + url
			if sc := str(b.Data["status_code"]); sc != "" {
				line += " → " + sc
			}
		case "console", "log":
			line = "📝 " + b.Message
		default:
			line = cat + ": " + b.Message
		}
		if strings.TrimSpace(line) != "" {
			a.UserJourney = append(a.UserJourney, line)
		}
	}
	if e.ErrorType != "" {
		loc := a.ErrorLocation
		if loc == "" {
			loc = "?"
		}
		a.UserJourney = append(a.UserJourney, "💥 "+e.ErrorType+" at "+loc)
	}
	return a
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

func str(v any) string {
	switch t := v.(type) {
	case nil:
		return ""
	case string:
		return t
	case float64:
		if t == float64(int64(t)) {
			return fmt.Sprintf("%d", int64(t))
		}
		return fmt.Sprintf("%v", t)
	default:
		return fmt.Sprintf("%v", t)
	}
}
