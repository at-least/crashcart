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
	HasRange   bool // an obfuscated line range is present (R8 ranges start at 0)
	ObfStart   int
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
				ClassName: origClass, MethodName: mm[3], Args: mm[4], HasRange: mm[1] != "",
				ObfStart: atoi(mm[1]), ObfEnd: atoi(mm[2]), OrigStart: atoi(mm[5]), OrigEnd: atoi(mm[6]),
			}
			// An inlined callee from another class is written fully
			// qualified: "int a.b.Repo.load(java.lang.String)".
			if i := strings.LastIndexByte(pm.MethodName, '.'); i > 0 {
				pm.ClassName, pm.MethodName = pm.MethodName[:i], pm.MethodName[i+1:]
			}
			// R8 repeats one obfuscated range once per inlined call level,
			// innermost callee first; Expand walks them back out.
			m.Methods[key] = append(m.Methods[key], pm)
		}
	}
	return m
}

func atoi(s string) int {
	n, _ := strconv.Atoi(s)
	return n
}

// Expand maps one frame to its original frames. Module (or Filename when
// Module is empty) is the obfuscated class, Function the obfuscated method.
// A frame R8 inlined into expands to the whole call chain, outermost
// caller first and innermost callee last (Sentry frame order). Frames the
// mapping does not know are returned unchanged.
func (m *ProGuardMapping) Expand(f Frame) []Frame {
	class := f.Module
	if class == "" {
		class = f.Filename
	}
	if class == "" {
		return []Frame{f}
	}
	cands, ok := m.Methods[class+"."+f.Function]
	if !ok || len(cands) == 0 {
		if cls, ok := m.Classes[class]; ok {
			out := f
			out.Module = cls
			out.Filename = deobfuscatedFile(f.Filename, cls)
			return []Frame{out}
		}
		return []Frame{f}
	}
	// Entries whose obfuscated range covers the line, in file order
	// (innermost first); otherwise the range-less declarations.
	var chain []proguardMethod
	for _, c := range cands {
		if c.HasRange && f.Lineno >= c.ObfStart && f.Lineno <= c.ObfEnd {
			chain = append(chain, c)
		}
	}
	if len(chain) == 0 {
		for _, c := range cands {
			if !c.HasRange {
				chain = []proguardMethod{c}
				break
			}
		}
	}
	if len(chain) == 0 {
		chain = cands[:1]
	}
	own := m.Classes[class]
	out := make([]Frame, 0, len(chain))
	for i := len(chain) - 1; i >= 0; i-- {
		c := chain[i]
		fr := f
		fr.Module = c.ClassName
		fr.Function = c.MethodName
		fr.Lineno = c.origLine(f.Lineno)
		if c.ClassName == own {
			fr.Filename = deobfuscatedFile(f.Filename, c.ClassName)
		} else {
			// Inlined from another class: the SDK's file name belongs to
			// the class the frame was found in, not to this one.
			fr.Filename = deobfuscatedFile("", c.ClassName)
		}
		out = append(out, fr)
	}
	return out
}

// origLine maps an obfuscated line through the entry's ranges.
func (c proguardMethod) origLine(line int) int {
	switch {
	case c.OrigStart > 0 && c.OrigEnd > c.OrigStart && c.HasRange && line >= c.ObfStart:
		return c.OrigStart + (line - c.ObfStart)
	case c.OrigStart > 0:
		return c.OrigStart
	}
	return line
}

// Resolve maps one frame to its innermost original frame (see Expand).
func (m *ProGuardMapping) Resolve(f Frame) Frame {
	out := m.Expand(f)
	return out[len(out)-1]
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

// ResolveAll expands every frame and reports whether anything changed.
func (m *ProGuardMapping) ResolveAll(frames []Frame) ([]Frame, bool) {
	out := make([]Frame, 0, len(frames))
	changed := false
	for _, f := range frames {
		ex := m.Expand(f)
		if len(ex) != 1 || ex[0] != f {
			changed = true
		}
		out = append(out, ex...)
	}
	return out, changed
}
