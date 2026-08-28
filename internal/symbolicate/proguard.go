// Package symbolicate resolves obfuscated / minified stack frames using
// ProGuard (R8) mapping files, JavaScript source maps, or the dSYM sidecar.
package symbolicate

import (
	"bufio"
	"regexp"
	"strconv"
	"strings"

	"github.com/newlix/crashcart/internal/sentry"
)

// Frame is the frame shape all resolvers work on: the Sentry frame itself,
// so resolved frames keep in_app, addresses and everything else the SDK sent.
type Frame = sentry.Frame

// ProGuardMapping is a parsed mapping.txt.
type ProGuardMapping struct {
	// obfuscated class → original class
	Classes map[string]string
	// obfuscated "class.method" → candidate original methods (R8 emits one
	// entry per inlined line range; the first one is the plain declaration).
	Methods map[string][]proguardMethod
}

type proguardMethod struct {
	ClassName  string
	MethodName string
	Args       string
	ObfStart   int // obfuscated line range, 0 = unbounded
	ObfEnd     int
	OrigStart  int // original line range, 0 = none
	OrigEnd    int
}

var (
	pgClassRe = regexp.MustCompile(`^(\S+)\s+->\s+(\S+):$`)
	// [obfStart:obfEnd:] [returnType ]name(args)[:origStart[:origEnd]] -> obfName
	pgMethodRe = regexp.MustCompile(`^\s+(?:(\d+):(\d+):)?(?:\S+\s+)?([^\s(]+)\(([^)]*)\)(?::(\d+)(?::(\d+))?)?\s+->\s+(\S+)$`)
)

// ParseProGuard reads a ProGuard/R8 mapping file.
func ParseProGuard(content string) *ProGuardMapping {
	m := &ProGuardMapping{Classes: map[string]string{}, Methods: map[string][]proguardMethod{}}
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
			key := obfClass + "." + mm[7]
			pm := proguardMethod{
				ClassName: origClass, MethodName: mm[3], Args: mm[4],
				ObfStart: atoi(mm[1]), ObfEnd: atoi(mm[2]), OrigStart: atoi(mm[5]), OrigEnd: atoi(mm[6]),
			}
			// Inlined callees repeat the obfuscated range with a different
			// original method; keep the outermost (first) per range.
			m.Methods[key] = append(m.Methods[key], pm)
		}
	}
	return m
}

func atoi(s string) int {
	n, _ := strconv.Atoi(s)
	return n
}

// Resolve maps one frame: module (or filename when module is empty) is the
// obfuscated class, function the obfuscated method. Resolved frames get the
// original class in Module, the original method in Function and the
// remapped line; everything else is kept.
func (m *ProGuardMapping) Resolve(f Frame) Frame {
	class := f.Module
	if class == "" {
		class = f.Filename
	}
	if class == "" {
		return f
	}
	if cands, ok := m.Methods[class+"."+f.Function]; ok && len(cands) > 0 {
		meth := cands[0]
		line := f.Lineno
		for _, c := range cands {
			if c.ObfStart == 0 || (f.Lineno >= c.ObfStart && f.Lineno <= c.ObfEnd) {
				meth = c
				break
			}
		}
		if meth.OrigStart > 0 {
			line = meth.OrigStart
			if meth.ObfStart > 0 && meth.OrigEnd > meth.OrigStart && f.Lineno >= meth.ObfStart {
				line = meth.OrigStart + (f.Lineno - meth.ObfStart)
			}
		}
		out := f
		out.Module = meth.ClassName
		out.Function = meth.MethodName
		out.Lineno = line
		out.Filename = deobfuscatedFile(f.Filename, meth.ClassName)
		return out
	}
	if cls, ok := m.Classes[class]; ok {
		out := f
		out.Module = cls
		out.Filename = deobfuscatedFile(f.Filename, cls)
		return out
	}
	return f
}

// deobfuscatedFile keeps a real source file name and replaces the R8
// placeholders ("SourceFile", "Unknown Source", "") with Outer.java.
func deobfuscatedFile(filename, class string) string {
	switch filename {
	case "", "SourceFile", "Unknown Source", "Unknown", "Native Method":
	default:
		return filename
	}
	name := class
	if i := strings.LastIndexByte(name, '.'); i >= 0 {
		name = name[i+1:]
	}
	if i := strings.IndexByte(name, '$'); i > 0 {
		name = name[:i]
	}
	if name == "" {
		return filename
	}
	return name + ".java"
}

// ResolveAll maps every frame and reports whether any changed.
func (m *ProGuardMapping) ResolveAll(frames []Frame) ([]Frame, bool) {
	out := make([]Frame, len(frames))
	changed := false
	for i, f := range frames {
		out[i] = m.Resolve(f)
		if out[i] != f {
			changed = true
		}
	}
	return out, changed
}
