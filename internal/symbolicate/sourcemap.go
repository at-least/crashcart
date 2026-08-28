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

// ParseSourceMap decodes a source map JSON document.
func ParseSourceMap(content []byte) (*SourceMap, error) {
	var raw struct {
		Version  int      `json:"version"`
		Sources  []string `json:"sources"`
		Names    []string `json:"names"`
		Mappings string   `json:"mappings"`
	}
	if err := json.Unmarshal(content, &raw); err != nil {
		return nil, err
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
	sort.SliceStable(sm.mappings, func(i, j int) bool {
		a, b := sm.mappings[i], sm.mappings[j]
		if a.genLine != b.genLine {
			return a.genLine < b.genLine
		}
		return a.genCol < b.genCol
	})
	return sm, nil
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
func (sm *SourceMap) Resolve(f Frame) Frame {
	if f.Lineno < 1 || len(sm.mappings) == 0 {
		return f
	}
	// last mapping with (line, col) <= (f.Lineno, f.Colno)
	i := sort.Search(len(sm.mappings), func(i int) bool {
		m := sm.mappings[i]
		return m.genLine > f.Lineno || (m.genLine == f.Lineno && m.genCol > f.Colno)
	}) - 1
	if i < 0 {
		return f
	}
	m := sm.mappings[i]
	if !m.hasOrig || m.genLine != f.Lineno {
		return f
	}
	out := Frame{Filename: f.Filename, Function: f.Function, Lineno: m.origLine, Colno: m.origCol}
	if m.source != "" {
		out.Filename = m.source
	}
	if m.name != "" {
		out.Function = m.name
	}
	return out
}

// ResolveAll maps every frame.
func (sm *SourceMap) ResolveAll(frames []Frame) []Frame {
	out := make([]Frame, len(frames))
	for i, f := range frames {
		out[i] = sm.Resolve(f)
	}
	return out
}
