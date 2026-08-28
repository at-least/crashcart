// Package symbolicate resolves obfuscated / minified stack frames using
// ProGuard (R8) mapping files, JavaScript source maps, or a dSYM service.
package symbolicate

import (
	"bufio"
	"regexp"
	"strings"
)

// Frame is the minimal frame shape exchanged with clients.
type Frame struct {
	Filename string `json:"filename,omitempty"`
	Function string `json:"function,omitempty"`
	Lineno   int    `json:"lineno,omitempty"`
	Colno    int    `json:"colno,omitempty"`
	Module   string `json:"module,omitempty"`
}

// ProGuardMapping is a parsed mapping.txt.
type ProGuardMapping struct {
	// obfuscated class → original class
	Classes map[string]string
	// obfuscated "class.method" → original method
	Methods map[string]proguardMethod
}

type proguardMethod struct {
	ClassName  string
	MethodName string
	Args       string
}

var (
	pgClassRe  = regexp.MustCompile(`^(\S+)\s+->\s+(\S+):$`)
	pgMethodRe = regexp.MustCompile(`^\s+(?:\d+:\d+:)?(?:\S+\s+)?(\S+)\(([^)]*)\)(?::\d+(?::\d+)?)?\s+->\s+(\S+)$`)
)

// ParseProGuard reads a ProGuard/R8 mapping file.
func ParseProGuard(content string) *ProGuardMapping {
	m := &ProGuardMapping{Classes: map[string]string{}, Methods: map[string]proguardMethod{}}
	var obfClass, origClass string
	sc := bufio.NewScanner(strings.NewReader(content))
	sc.Buffer(make([]byte, 1024*1024), 16*1024*1024)
	for sc.Scan() {
		line := sc.Text()
		if strings.TrimSpace(line) == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if mm := pgClassRe.FindStringSubmatch(line); mm != nil {
			origClass, obfClass = mm[1], mm[2]
			m.Classes[obfClass] = origClass
			continue
		}
		if mm := pgMethodRe.FindStringSubmatch(line); mm != nil && obfClass != "" {
			key := obfClass + "." + mm[3]
			if _, exists := m.Methods[key]; !exists {
				m.Methods[key] = proguardMethod{ClassName: origClass, MethodName: mm[1], Args: mm[2]}
			}
		}
	}
	return m
}

// Resolve maps one frame (filename = obfuscated class, function = method).
func (m *ProGuardMapping) Resolve(f Frame) Frame {
	if f.Filename == "" {
		return f
	}
	if meth, ok := m.Methods[f.Filename+"."+f.Function]; ok {
		return Frame{Filename: meth.ClassName + "." + meth.MethodName, Function: meth.MethodName + "(" + meth.Args + ")", Lineno: f.Lineno}
	}
	if cls, ok := m.Classes[f.Filename]; ok {
		return Frame{Filename: cls + "." + f.Function, Function: f.Function, Lineno: f.Lineno}
	}
	return f
}

// ResolveAll maps every frame.
func (m *ProGuardMapping) ResolveAll(frames []Frame) []Frame {
	out := make([]Frame, len(frames))
	for i, f := range frames {
		out[i] = m.Resolve(f)
	}
	return out
}
