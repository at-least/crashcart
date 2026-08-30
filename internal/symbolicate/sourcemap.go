package symbolicate

import (
	"encoding/json"
	"errors"
	"sort"
	"strings"
)

// SourceMap is a parsed Source Map v3 (mappings decoded).
type SourceMap struct {
	Sources  []string
	Names    []string
	mappings []mapping // sorted by (generatedLine, generatedColumn)
}

type mapping struct {
	genLine, genCol int
	source          string
	origLine        int // 1-based, 0 = none
	origCol         int
	name            string
	hasOrig         bool
}

// ParseSourceMap decodes a source map JSON document — a plain map, or an
// indexed one (`sections`, what Metro / Hermes emit for react-native),
// whose sub-maps are folded in at their offsets.
func ParseSourceMap(content []byte) (*SourceMap, error) {
	var raw struct {
		Version    int      `json:"version"`
		Sources    []string `json:"sources"`
		SourceRoot string   `json:"sourceRoot"`
		Names      []string `json:"names"`
		Mappings   string   `json:"mappings"`
		Sections   []struct {
			Offset struct {
				Line   int `json:"line"`
				Column int `json:"column"`
			} `json:"offset"`
			Map json.RawMessage `json:"map"`
		} `json:"sections"`
	}
	if err := json.Unmarshal(content, &raw); err != nil {
		return nil, err
	}
	if len(raw.Sections) > 0 {
		sm := &SourceMap{}
		for _, sec := range raw.Sections {
			if len(sec.Map) == 0 {
				continue
			}
			sub, err := ParseSourceMap(sec.Map)
			if err != nil {
				continue // one bad section does not void the others
			}
			for _, m := range sub.mappings {
				if m.genLine == 1 { // the column offset applies to the section's first line only
					m.genCol += sec.Offset.Column
				}
				m.genLine += sec.Offset.Line
				sm.mappings = append(sm.mappings, m)
			}
			sm.Sources = append(sm.Sources, sub.Sources...)
			sm.Names = append(sm.Names, sub.Names...)
		}
		sortMappings(sm.mappings)
		return sm, nil
	}
	if raw.SourceRoot != "" {
		root := strings.TrimSuffix(raw.SourceRoot, "/") + "/"
		for i, src := range raw.Sources {
			if src != "" && !strings.HasPrefix(src, root) && !strings.Contains(src, "://") {
				raw.Sources[i] = root + src
			}
		}
	}
	sm := &SourceMap{Sources: raw.Sources, Names: raw.Names}
	var srcIdx, origLine, origCol, nameIdx int
	for lineIdx, segs := range strings.Split(raw.Mappings, ";") {
		genCol := 0
		for _, seg := range strings.Split(segs, ",") {
			if seg == "" {
				continue
			}
			vals, err := decodeVLQs(seg)
			if err != nil || len(vals) == 0 {
				continue
			}
			genCol += vals[0]
			m := mapping{genLine: lineIdx + 1, genCol: genCol}
			if len(vals) >= 4 {
				srcIdx += vals[1]
				origLine += vals[2]
				origCol += vals[3]
				if srcIdx >= 0 && srcIdx < len(sm.Sources) {
					m.source = sm.Sources[srcIdx]
				}
				m.origLine = origLine + 1
				m.origCol = origCol
				m.hasOrig = true
				if len(vals) >= 5 {
					nameIdx += vals[4]
					if nameIdx >= 0 && nameIdx < len(sm.Names) {
						m.name = sm.Names[nameIdx]
					}
				}
			}
			sm.mappings = append(sm.mappings, m)
		}
	}
	sortMappings(sm.mappings)
	return sm, nil
}

func sortMappings(ms []mapping) {
	sort.SliceStable(ms, func(i, j int) bool {
		a, b := ms[i], ms[j]
		if a.genLine != b.genLine {
			return a.genLine < b.genLine
		}
		return a.genCol < b.genCol
	})
}

const b64 = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789+/"

var errBadVLQ = errors.New("invalid base64 VLQ")

func decodeVLQs(s string) ([]int, error) {
	var out []int
	result, shift := 0, 0
	for i := 0; i < len(s); i++ {
		d := strings.IndexByte(b64, s[i])
		if d < 0 {
			return nil, errBadVLQ
		}
		result += (d & 31) << shift
		shift += 5
		if d&32 == 0 {
			neg := result&1 == 1
			result >>= 1
			if neg {
				result = -result
			}
			out = append(out, result)
			result, shift = 0, 0
		}
	}
	if shift != 0 {
		return nil, errBadVLQ
	}
	return out, nil
}

// Resolve maps a minified (line, column) back to the original source.
// Sentry columns are 1-based, source maps 0-based.
func (sm *SourceMap) Resolve(f Frame) Frame {
	if f.Lineno < 1 || len(sm.mappings) == 0 {
		return f
	}
	col := f.Colno
	if col > 0 {
		col--
	}
	// last mapping with (line, col) <= (f.Lineno, col)
	i := sort.Search(len(sm.mappings), func(i int) bool {
		m := sm.mappings[i]
		return m.genLine > f.Lineno || (m.genLine == f.Lineno && m.genCol > col)
	}) - 1
	if i < 0 {
		return f
	}
	m := sm.mappings[i]
	if !m.hasOrig || m.genLine != f.Lineno {
		return f
	}
	out := f
	out.Lineno = m.origLine
	out.Colno = m.origCol + 1
	if m.source != "" {
		out.Filename = m.source
		out.AbsPath = ""
	}
	if m.name != "" {
		out.Function = m.name
	}
	return out
}

// ResolveAll maps every frame and reports whether any changed.
func (sm *SourceMap) ResolveAll(frames []Frame) ([]Frame, bool) {
	out := make([]Frame, len(frames))
	changed := false
	for i, f := range frames {
		out[i] = sm.Resolve(f)
		if out[i] != f {
			changed = true
		}
	}
	return out, changed
}

// SourceMapSet holds every source map uploaded for one release, keyed by
// the generated file name (mapping "bundle.js.map" ↔ frame "bundle.js").
type SourceMapSet struct {
	byFile map[string]*SourceMap
	single *SourceMap // used when only one map exists and nothing matches
}

// NewSourceMapSet parses files; unparsable ones are skipped.
func NewSourceMapSet(files map[string][]byte) *SourceMapSet {
	set := &SourceMapSet{byFile: map[string]*SourceMap{}}
	for name, data := range files {
		sm, err := ParseSourceMap(data)
		if err != nil {
			continue
		}
		key := strings.TrimSuffix(baseName(name), ".map")
		set.byFile[key] = sm
		set.single = sm
	}
	if len(set.byFile) != 1 {
		set.single = nil
	}
	return set
}

// Len is the number of parsed maps.
func (s *SourceMapSet) Len() int { return len(s.byFile) }

// Resolve picks the map for the frame's file and resolves it.
func (s *SourceMapSet) Resolve(f Frame) Frame {
	name := f.Filename
	if name == "" {
		name = f.AbsPath
	}
	name = baseName(name)
	if i := strings.IndexAny(name, "?#"); i >= 0 {
		name = name[:i]
	}
	sm := s.byFile[name]
	if sm == nil && name == "" { // a frame that names no file: the release's one map is the only guess there is
		sm = s.single
	}
	if sm == nil {
		return f
	}
	return sm.Resolve(f)
}

// ResolveAll maps every frame and reports whether any changed.
func (s *SourceMapSet) ResolveAll(frames []Frame) ([]Frame, bool) {
	out := make([]Frame, len(frames))
	changed := false
	for i, f := range frames {
		out[i] = s.Resolve(f)
		if out[i] != f {
			changed = true
		}
	}
	return out, changed
}

func baseName(p string) string {
	if i := strings.LastIndexAny(p, "/\\"); i >= 0 {
		p = p[i+1:]
	}
	return p
}
