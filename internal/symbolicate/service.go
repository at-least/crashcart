package symbolicate

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/newlix/crashcart/internal/db/sqlc"
	"github.com/newlix/crashcart/internal/pk"
	"github.com/newlix/crashcart/internal/sentry"
	"github.com/newlix/crashcart/internal/store"
)

// Symbol-file kinds (symbol_files.kind).
const (
	KindProGuard  = "proguard"
	KindSourceMap = "sourcemap"
	KindDSYM      = "dsym"
)

const (
	// missTTL is how long a negative lookup (no mapping uploaded) is
	// remembered before the database is asked again.
	missTTL = 60 * time.Second
	// cacheMax bounds the parsed mappings kept in memory.
	cacheMax = 64
	// ReleaseWindow is how far back Release() looks for unsymbolicated
	// events (updates must land before the chunk is compressed).
	ReleaseWindow = 48 * time.Hour
	// ReleaseMax bounds the events one Release() call touches.
	ReleaseMax = 2000
)

// Service resolves frames for a project: in-process ProGuard / source-map
// mappings (cached per project+release) and dSYM through the sidecar.
// It implements ingest.Symbolicator (Inline) and the job handlers.
type Service struct {
	Store *store.Store
	DSYM  *DSYMClient // Enabled() false when no sidecar

	mu    sync.Mutex
	cache map[cacheKey]*cacheEntry
}

// cacheKey identifies one mapping: release-scoped ("1.2.3") or
// debug-id-scoped ("debug:<uuid>") per project and kind.
type cacheKey struct {
	projectID int64
	kind      string
	key       string
}

type cacheEntry struct {
	mapping  any // *ProGuardMapping | *SourceMapSet | nil (negative)
	loadedAt time.Time
}

const debugPrefix = "debug:"

// Inline resolves ev's frames with a proguard/sourcemap mapping. The
// mapping is looked up in the database on a cache miss (at most once per
// minute per key) and kept in memory afterwards. ok=false when no mapping
// exists or nothing applies to the event.
func (s *Service) Inline(ctx context.Context, projectID int64, ev *sentry.Event) ([]sentry.Frame, bool) {
	frames := ev.Frames()
	if len(frames) == 0 || s.Store == nil {
		return nil, false
	}
	if wantsProGuard(ev) {
		if m := s.proguardFor(ctx, projectID, ev); m != nil {
			if out, changed := m.ResolveAll(frames); changed {
				return out, true
			}
		}
	}
	if wantsSourceMap(ev) && ev.Release != "" {
		if set := s.sourceMapsFor(ctx, projectID, ev.Release); set != nil {
			if out, changed := set.ResolveAll(frames); changed {
				return out, true
			}
		}
	}
	return nil, false
}

func wantsProGuard(ev *sentry.Event) bool {
	switch ev.Platform {
	case "android", "java", "kotlin":
		return true
	}
	for _, im := range ev.DebugImages {
		if im.Type == "proguard" {
			return true
		}
	}
	return false
}

func wantsSourceMap(ev *sentry.Event) bool {
	switch ev.Platform {
	case "javascript", "node", "react-native":
		return true
	}
	for _, f := range ev.Frames() {
		if f.Colno > 0 && f.InstrAddr == "" {
			return true
		}
	}
	return false
}

// proguardFor finds the mapping by debug_id (debug_meta.images of type
// proguard) first, then by release.
func (s *Service) proguardFor(ctx context.Context, projectID int64, ev *sentry.Event) *ProGuardMapping {
	for _, im := range ev.DebugImages {
		if im.Type != "proguard" || im.DebugID == "" {
			continue
		}
		v := s.load(ctx, cacheKey{projectID, KindProGuard, debugPrefix + normalizeDebugID(im.DebugID)})
		if m, ok := v.(*ProGuardMapping); ok {
			return m
		}
	}
	if ev.Release == "" {
		return nil
	}
	m, _ := s.load(ctx, cacheKey{projectID, KindProGuard, ev.Release}).(*ProGuardMapping)
	return m
}

func (s *Service) sourceMapsFor(ctx context.Context, projectID int64, release string) *SourceMapSet {
	set, _ := s.load(ctx, cacheKey{projectID, KindSourceMap, release}).(*SourceMapSet)
	return set
}

// load returns the cached mapping for k, fetching and parsing it when the
// entry is missing or a negative entry has expired. nil = no mapping.
func (s *Service) load(ctx context.Context, k cacheKey) any {
	s.mu.Lock()
	e, ok := s.cache[k]
	if ok && (e.mapping != nil || time.Since(e.loadedAt) < missTTL) {
		s.mu.Unlock()
		return e.mapping
	}
	s.mu.Unlock()

	mapping, err := s.fetch(ctx, k)
	if err != nil {
		return nil // transient (cancelled request, database hiccup): do not cache the miss
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if s.cache == nil {
		s.cache = map[cacheKey]*cacheEntry{}
	}
	if len(s.cache) >= cacheMax {
		s.evictOldestLocked()
	}
	s.cache[k] = &cacheEntry{mapping: mapping, loadedAt: time.Now()}
	return mapping
}

func (s *Service) evictOldestLocked() {
	var oldest cacheKey
	var oldestAt time.Time
	first := true
	for k, e := range s.cache {
		if first || e.loadedAt.Before(oldestAt) {
			oldest, oldestAt, first = k, e.loadedAt, false
		}
	}
	if !first {
		delete(s.cache, oldest)
	}
}

// fetch reads and parses the symbol file(s) for k. nil, nil is a definite
// miss (cached for missTTL); an error is transient and is not cached.
func (s *Service) fetch(ctx context.Context, k cacheKey) (any, error) {
	var files []sqlc.SymbolFile
	if strings.HasPrefix(k.key, debugPrefix) {
		id := strings.TrimPrefix(k.key, debugPrefix)
		f, err := s.Store.SymbolFileByDebugID(ctx, sqlc.SymbolFileByDebugIDParams{ProjectID: k.projectID, DebugID: &id})
		if errors.Is(err, pgx.ErrNoRows) || (err == nil && f.Kind != k.kind) {
			return nil, nil
		}
		if err != nil {
			return nil, err
		}
		files = []sqlc.SymbolFile{f}
	} else {
		var err error
		files, err = s.Store.SymbolFilesForRelease(ctx, sqlc.SymbolFilesForReleaseParams{ProjectID: k.projectID, Release: k.key, Kind: k.kind})
		if err != nil {
			return nil, err
		}
		if len(files) == 0 {
			return nil, nil
		}
	}
	switch k.kind {
	case KindProGuard:
		// Several mapping files for one release are concatenated: classes
		// are disjoint per module, so one merged table serves them all.
		var sb strings.Builder
		for _, f := range files {
			sb.Write(f.Data)
			sb.WriteByte('\n')
		}
		m := ParseProGuard(sb.String())
		if len(m.Classes) == 0 {
			return nil, nil
		}
		return m, nil
	case KindSourceMap:
		byName := make(map[string][]byte, len(files))
		for _, f := range files {
			byName[f.Filename] = f.Data
		}
		set := NewSourceMapSet(byName)
		if set.Len() == 0 {
			return nil, nil
		}
		return set, nil
	}
	return nil, nil
}

// Invalidate drops the cached mappings of (project, release) — and every
// debug-id-scoped entry of the project — so the next lookup reloads.
// Call after a symbol upload.
func (s *Service) Invalidate(projectID int64, release string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for k := range s.cache {
		if k.projectID == projectID && (k.key == release || strings.HasPrefix(k.key, debugPrefix)) {
			delete(s.cache, k)
		}
	}
}

// Event symbolicates one stored event (job kind "symbolicate"). It returns
// nil when nothing can be resolved yet (the event stays unsymbolicated —
// a later upload re-queues it); only sidecar / database failures are
// errors, so the job retries.
func (s *Service) Event(ctx context.Context, projectID, eventID int64) error {
	row, err := s.Store.GetEvent(ctx, sqlc.GetEventParams{ProjectID: projectID, ID: eventID})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil // dropped by retention, or never stored
	}
	if err != nil {
		return err
	}
	if row.Symbolicated {
		return nil
	}
	now := time.Now().UTC()
	ev := sentry.ParseEvent(row.EventID, pk.Time(row.ID), row.Payload, now)
	if ev == nil {
		return nil
	}
	if ev.Release == "" && row.Release != nil {
		ev.Release = *row.Release
	}
	frames, ok := s.Inline(ctx, projectID, ev)
	if !ok && s.DSYM.Enabled() && isNative(ev) {
		frames, ok, err = s.dsym(ctx, projectID, ev)
		if err != nil {
			return fmt.Errorf("dsym: %w", err)
		}
	}
	if !ok {
		return nil
	}

	newFP := sentry.Fingerprint(ev, frames)
	location := sentry.ErrorLocation(frames)
	symbols, err := json.Marshal(frames)
	if err != nil {
		return err
	}
	oldFP := ""
	if row.Fingerprint != nil {
		oldFP = *row.Fingerprint
	}
	return s.Store.Tx(ctx, func(ctx context.Context, tx pgx.Tx, q *sqlc.Queries) error {
		if err := q.SetEventSymbols(ctx, sqlc.SetEventSymbolsParams{
			ProjectID: projectID, ID: eventID, Symbols: symbols,
			Fingerprint: nilIfEmpty(newFP), ErrorLocation: nilIfEmpty(location),
		}); err != nil {
			return err
		}
		if newFP == oldFP || newFP == "" {
			return nil
		}
		if _, err := q.UpsertIssue(ctx, sqlc.UpsertIssueParams{
			ProjectID: projectID, Fingerprint: newFP, Title: ev.IssueTitle(), Level: row.Level,
			ErrorType: nilIfEmpty(ev.ErrorType), Screen: nilIfEmpty(ev.Screen), Platform: nilIfEmpty(ev.Platform),
			EventCount: 1, StoredCount: 1, FirstSeen: eventID, LastSeen: eventID, FirstRelease: row.Release,
		}); err != nil {
			return err
		}
		if oldFP == "" {
			return nil
		}
		if err := q.AdjustIssueStoredCount(ctx, sqlc.AdjustIssueStoredCountParams{ProjectID: projectID, Fingerprint: oldFP, StoredCount: -1}); err != nil {
			return err
		}
		return q.DeleteEmptyIssue(ctx, sqlc.DeleteEmptyIssueParams{ProjectID: projectID, Fingerprint: oldFP})
	})
}

// isNative reports whether the event carries raw addresses the sidecar
// could resolve.
func isNative(ev *sentry.Event) bool {
	if len(ev.DebugImages) == 0 {
		return false
	}
	for _, f := range ev.Frames() {
		if f.InstrAddr != "" {
			return true
		}
	}
	return false
}

// dsym resolves native frames through the sidecar, one request per debug
// image that has a dSYM (by debug_id, else by release). ok=false when no
// frame resolved; err only for sidecar / database failures.
func (s *Service) dsym(ctx context.Context, projectID int64, ev *sentry.Event) ([]sentry.Frame, bool, error) {
	frames := ev.Frames()
	out := make([]sentry.Frame, len(frames))
	copy(out, frames)

	// Frame index → image, by address range (or the frame's image_addr).
	type lookup struct {
		idx  int
		addr uint64
	}
	byImage := map[int][]lookup{}
	for i, f := range frames {
		if f.InstrAddr == "" || f.Function != "" {
			continue
		}
		addr, ok := sentry.ParseHex(f.InstrAddr)
		if !ok {
			continue
		}
		if im := findImage(ev, f, addr); im >= 0 {
			byImage[im] = append(byImage[im], lookup{i, addr})
		}
	}
	if len(byImage) == 0 {
		return nil, false, nil
	}

	var releaseFiles []sqlc.SymbolFile
	releaseLoaded := false
	resolved := false
	for im, lks := range byImage {
		image := ev.DebugImages[im]
		file, err := s.dsymFile(ctx, projectID, ev.Release, image, &releaseFiles, &releaseLoaded)
		if err != nil {
			return nil, false, err
		}
		if file == nil {
			continue
		}
		base, _ := sentry.ParseHex(image.ImageAddr)
		module := baseName(image.CodeFile)
		if len(lks) > MaxDSYMFrames {
			lks = lks[len(lks)-MaxDSYMFrames:] // innermost frames matter most
		}
		addrs := make([]DSYMAddr, len(lks))
		for i, lk := range lks {
			addrs[i] = DSYMAddr{Address: lk.addr - base, Module: module}
		}
		results, err := s.DSYM.Resolve(ctx, file.Data, addrs)
		if err != nil {
			return nil, false, err
		}
		for i, r := range results {
			if i >= len(lks) || !r.Resolved() {
				continue
			}
			f := &out[lks[i].idx]
			f.Function = r.Function
			if r.Filename != "" && r.Filename != "??" {
				f.Filename = baseName(r.Filename)
				f.AbsPath = r.Filename
			}
			f.Lineno = r.Lineno
			if f.Package == "" {
				f.Package = module
			}
			resolved = true
		}
	}
	return out, resolved, nil
}

// dsymFile finds the symbol file for image: by debug_id, else among the
// release's dSYMs by code_file basename (or the only one).
func (s *Service) dsymFile(ctx context.Context, projectID int64, release string, image sentry.DebugImage, releaseFiles *[]sqlc.SymbolFile, loaded *bool) (*sqlc.SymbolFile, error) {
	if image.DebugID != "" {
		id := normalizeDebugID(image.DebugID)
		f, err := s.Store.SymbolFileByDebugID(ctx, sqlc.SymbolFileByDebugIDParams{ProjectID: projectID, DebugID: &id})
		if err == nil && f.Kind == KindDSYM {
			return &f, nil
		}
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return nil, err
		}
	}
	if release == "" {
		return nil, nil
	}
	if !*loaded {
		files, err := s.Store.SymbolFilesForRelease(ctx, sqlc.SymbolFilesForReleaseParams{ProjectID: projectID, Release: release, Kind: KindDSYM})
		if err != nil {
			return nil, err
		}
		*releaseFiles, *loaded = files, true
	}
	want := strings.ToLower(baseName(image.CodeFile))
	for i := range *releaseFiles {
		name := strings.ToLower(strings.TrimSuffix(baseName((*releaseFiles)[i].Filename), ".dsym"))
		if want != "" && (name == want || strings.HasPrefix(name, want+".")) {
			return &(*releaseFiles)[i], nil
		}
	}
	if len(*releaseFiles) == 1 {
		return &(*releaseFiles)[0], nil
	}
	return nil, nil
}

// findImage picks the debug image containing addr: by the frame's
// image_addr first, then by [image_addr, image_addr+image_size).
func findImage(ev *sentry.Event, f sentry.Frame, addr uint64) int {
	if f.ImageAddr != "" {
		for i, im := range ev.DebugImages {
			if im.ImageAddr == f.ImageAddr {
				return i
			}
		}
	}
	return ev.ImageFor(addr)
}

// normalizeDebugID lower-cases and strips the "-<age>" suffix some SDKs add.
func normalizeDebugID(id string) string {
	id = strings.ToLower(strings.TrimSpace(id))
	if len(id) > 36 && id[36] == '-' {
		id = id[:36]
	}
	return id
}

// Release re-symbolicates every unsymbolicated event of a release from the
// last ReleaseWindow (job kind "resymbolicate"), called after a symbol
// upload. The cache for the release is dropped first.
func (s *Service) Release(ctx context.Context, projectID int64, release string) error {
	s.Invalidate(projectID, release)
	ids, err := s.Store.UnsymbolicatedEvents(ctx, sqlc.UnsymbolicatedEventsParams{
		ProjectID: projectID, Release: &release, ID: pk.Lower(time.Now().Add(-ReleaseWindow)), Limit: ReleaseMax,
	})
	if err != nil {
		return err
	}
	var failed int
	var last error
	for _, id := range ids {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := s.Event(ctx, projectID, id); err != nil {
			failed++
			last = err
		}
	}
	if failed > 0 {
		return fmt.Errorf("resymbolicate %q: %d of %d events failed: %w", release, failed, len(ids), last)
	}
	return nil
}

// DetectKind guesses the symbol-file kind from the upload's name and its
// first bytes: "proguard" (mapping.txt), "sourcemap" (.map) or "dsym"
// (Mach-O / zipped dSYM). "" when unrecognizable.
func DetectKind(filename string, head []byte) string {
	name := strings.ToLower(baseName(filename))
	trimmed := bytes.TrimSpace(head)
	switch {
	case strings.HasSuffix(name, ".map") || strings.HasSuffix(name, ".js.map"):
		return KindSourceMap
	case name == "mapping.txt" || strings.HasSuffix(name, "mapping.txt") || strings.HasSuffix(name, ".txt"):
		return KindProGuard
	case strings.HasSuffix(name, ".dsym") || strings.HasSuffix(name, ".zip") || strings.Contains(name, ".dsym"):
		return KindDSYM
	}
	if len(trimmed) >= 4 {
		switch string(trimmed[:4]) {
		case "\xcf\xfa\xed\xfe", "\xce\xfa\xed\xfe", "\xfe\xed\xfa\xcf", "\xfe\xed\xfa\xce", // Mach-O
			"\xca\xfe\xba\xbe", // fat binary
			"PK\x03\x04":       // zip
			return KindDSYM
		}
	}
	if len(trimmed) > 0 && trimmed[0] == '{' && bytes.Contains(head, []byte(`"mappings"`)) {
		return KindSourceMap
	}
	if bytes.Contains(head, []byte(" -> ")) || bytes.HasPrefix(trimmed, []byte("# compiler")) {
		return KindProGuard
	}
	return ""
}

func nilIfEmpty(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}
