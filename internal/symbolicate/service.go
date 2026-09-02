package symbolicate

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"golang.org/x/sync/singleflight"

	"github.com/jackc/pgx/v5"

	"github.com/at-least/crashcart/internal/blob"
	"github.com/at-least/crashcart/internal/db/sqlc"
	"github.com/at-least/crashcart/internal/sentry"
	"github.com/at-least/crashcart/internal/store"
)

// Symbol-file kinds (symbol_files.kind).
const (
	KindProGuard  = "proguard"
	KindSourceMap = "sourcemap"
	KindDSYM      = "dsym"
)

const (
	// missTTL is how long a negative lookup (no mapping uploaded) is
	// remembered before the database is asked again; revalidateTTL how
	// long a parsed mapping is served before a cheap version check (row
	// count and latest upload time) confirms it is still current — an
	// upload through another replica must not leave a stale mapping here.
	missTTL       = 60 * time.Second
	revalidateTTL = 60 * time.Second
	// fetchTimeout bounds one load of a mapping from the database (shared
	// by every caller waiting for it, so no single caller's budget cuts it short).
	fetchTimeout = 2 * time.Minute
	// cacheMax bounds the parsed mappings kept in memory.
	cacheMax = 64
	// ReleaseMax bounds the events one Release() call queues (newest
	// first; the whole retention window is eligible).
	ReleaseMax = 2000
)

// Service resolves frames for a project: in-process ProGuard / source-map
// mappings (cached per project+release) and dSYM through the sidecar.
// It implements ingest.Symbolicator (Resolve) and the job handlers.
type Service struct {
	Store *store.Store
	DSYM  *DSYMClient // Enabled() false when no sidecar

	mu     sync.Mutex
	cache  map[cacheKey]*cacheEntry
	flight singleflight.Group // one load per key at a time, however many events wait for it
}

// cacheKey identifies one mapping: release-scoped ("1.2.3") or
// debug-id-scoped ("debug:<uuid>") per project and kind.
type cacheKey struct {
	projectID int64
	kind      string
	key       string
}

type cacheEntry struct {
	mapping   any // *ProGuardMapping | *SourceMapSet | nil (negative)
	loadedAt  time.Time
	checkedAt time.Time // last time version was confirmed against the database
	version   string    // symbol_files rows behind this entry: "<count>/<latest upload µs>"
}

func (k cacheKey) String() string { return fmt.Sprintf("%d/%s/%s", k.projectID, k.kind, k.key) }

const debugPrefix = "debug:"

// Resolve symbolicates ev at ingest: in-process mappings first, then —
// when a sidecar is configured and the event carries native addresses —
// the sidecar, bounded by ctx (ingest shares one budget across an
// envelope). ok=false when nothing resolved; retry=true when the
// sidecar failed or ran out of time, so a "symbolicate" job should finish
// the work later.
func (s *Service) Resolve(ctx context.Context, projectID int64, ev *sentry.Event) (frames []sentry.Frame, ok, retry bool) {
	frames, ok, err := s.resolve(ctx, projectID, ev, false)
	return frames, ok, err != nil
}

// resolve is the shared symbolication path of Resolve and Event: inline
// mappings, else the sidecar. err is only a sidecar / database failure.
// upload allows sending a symbol file the sidecar does not have yet
// (reading its bytes from the database): the job worker does, ingest
// does not — there the miss is an error, so the event goes to a job.
func (s *Service) resolve(ctx context.Context, projectID int64, ev *sentry.Event, upload bool) ([]sentry.Frame, bool, error) {
	frames, ok, err := s.Inline(ctx, projectID, ev)
	if err != nil || ok {
		return frames, ok, err
	}
	if s.DSYM.Enabled() && isNative(ev) {
		return s.dsym(ctx, projectID, ev, upload)
	}
	return nil, false, nil
}

// Inline resolves ev's frames with a proguard/sourcemap mapping. The
// mapping is looked up in the database on a cache miss (at most once per
// minute per key) and kept in memory afterwards. ok=false when no mapping
// exists or nothing applies to the event; err is a database failure
// (the caller retries — "no mapping" and "could not load the mapping"
// must not look the same, or the event is never revisited).
func (s *Service) Inline(ctx context.Context, projectID int64, ev *sentry.Event) ([]sentry.Frame, bool, error) {
	frames := ev.Frames()
	if len(frames) == 0 || s.Store == nil {
		return nil, false, nil
	}
	if wantsProGuard(ev) {
		m, err := s.proguardFor(ctx, projectID, ev)
		if err != nil {
			return nil, false, err
		}
		if m != nil {
			if out, changed := m.ResolveAll(frames); changed {
				return out, true, nil
			}
		}
	}
	if wantsSourceMap(ev) && ev.Release != "" {
		set, err := s.sourceMapsFor(ctx, projectID, ev.Release)
		if err != nil {
			return nil, false, err
		}
		if set != nil {
			if out, changed := set.ResolveAll(frames); changed {
				return out, true, nil
			}
		}
	}
	return nil, false, nil
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
func (s *Service) proguardFor(ctx context.Context, projectID int64, ev *sentry.Event) (*ProGuardMapping, error) {
	for _, im := range ev.DebugImages {
		if im.Type != "proguard" || im.DebugID == "" {
			continue
		}
		v, err := s.load(ctx, cacheKey{projectID, KindProGuard, debugPrefix + normalizeDebugID(im.DebugID)})
		if err != nil {
			return nil, err
		}
		if m, ok := v.(*ProGuardMapping); ok {
			return m, nil
		}
	}
	if ev.Release == "" {
		return nil, nil
	}
	v, err := s.load(ctx, cacheKey{projectID, KindProGuard, ev.Release})
	m, _ := v.(*ProGuardMapping)
	return m, err
}

func (s *Service) sourceMapsFor(ctx context.Context, projectID int64, release string) (*SourceMapSet, error) {
	v, err := s.load(ctx, cacheKey{projectID, KindSourceMap, release})
	set, _ := v.(*SourceMapSet)
	return set, err
}

// load returns the cached mapping for k, fetching and parsing it when the
// entry is missing, a negative entry has expired, or the rows behind a
// positive one have changed (checked at most every revalidateTTL). nil,
// nil = no mapping; an error is a database failure, never cached.
func (s *Service) load(ctx context.Context, k cacheKey) (any, error) {
	s.mu.Lock()
	e, ok := s.cache[k]
	if ok && e.mapping == nil && time.Since(e.loadedAt) >= missTTL {
		ok = false
	}
	if ok && e.mapping != nil && time.Since(e.checkedAt) >= revalidateTTL {
		s.mu.Unlock()
		v, err := s.version(ctx, k)
		s.mu.Lock()
		if err == nil && v == e.version {
			e.checkedAt = time.Now()
		} else if err == nil {
			ok = false // re-uploaded (possibly through another replica): reload
		} // on an error the cached mapping is served: it was right a minute ago
	}
	if ok {
		s.mu.Unlock()
		return e.mapping, nil
	}
	s.mu.Unlock()

	// One fetch per key however many events wait for it (a crash burst
	// right after a release would otherwise parse the mapping N times),
	// on its own deadline: the first caller's budget must not cut short
	// a load everyone else is waiting for.
	ch := s.flight.DoChan(k.String(), func() (any, error) {
		fctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), fetchTimeout)
		defer cancel()
		mapping, version, err := s.fetch(fctx, k)
		if err != nil {
			return nil, err
		}
		s.mu.Lock()
		defer s.mu.Unlock()
		if s.cache == nil {
			s.cache = map[cacheKey]*cacheEntry{}
		}
		if len(s.cache) >= cacheMax {
			s.evictOldestLocked()
		}
		now := time.Now()
		s.cache[k] = &cacheEntry{mapping: mapping, loadedAt: now, checkedAt: now, version: version}
		return mapping, nil
	})
	select {
	case r := <-ch:
		return r.Val, r.Err
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// version is the cheap fingerprint of the symbol_files rows behind k —
// the same one fetch derives from the rows it read.
func (s *Service) version(ctx context.Context, k cacheKey) (string, error) {
	debugID, release := "", k.key
	if strings.HasPrefix(k.key, debugPrefix) {
		debugID, release = strings.TrimPrefix(k.key, debugPrefix), ""
	}
	row, err := s.Store.SymbolFilesVersion(ctx, sqlc.SymbolFilesVersionParams{ProjectID: k.projectID, Kind: sqlc.SymbolKind(k.kind), Release: release, DebugID: debugID})
	if err != nil {
		return "", err
	}
	return versionOf(row.N, row.Latest), nil
}

func versionOf(n int64, latest time.Time) string {
	if n == 0 {
		return "0/0"
	}
	return fmt.Sprintf("%d/%d", n, latest.UnixMicro())
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

// fetch reads and parses the symbol file(s) for k and returns the version
// of the rows it read. A nil mapping is a definite miss (cached for
// missTTL); an error is transient and is not cached.
func (s *Service) fetch(ctx context.Context, k cacheKey) (mapping any, version string, err error) {
	mapping, version, err = s.fetchOnce(ctx, k)
	if errors.Is(err, blob.ErrNotFound) {
		// A re-upload replaced an object between the row read and the
		// Get: the rows now point at the new keys. Once more, then it is
		// a transient error (never a cached miss).
		mapping, version, err = s.fetchOnce(ctx, k)
	}
	return mapping, version, err
}

func (s *Service) fetchOnce(ctx context.Context, k cacheKey) (mapping any, version string, err error) {
	var files []sqlc.SymbolFile
	if strings.HasPrefix(k.key, debugPrefix) {
		id := strings.TrimPrefix(k.key, debugPrefix)
		f, err := s.Store.SymbolFileByDebugID(ctx, sqlc.SymbolFileByDebugIDParams{ProjectID: k.projectID, DebugID: &id})
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, versionOf(0, time.Time{}), nil
		}
		if err != nil {
			return nil, "", err
		}
		if string(f.Kind) != k.kind {
			return nil, versionOf(0, time.Time{}), nil // SymbolFilesVersion filters by kind too
		}
		files = []sqlc.SymbolFile{f}
	} else {
		files, err = s.Store.SymbolFilesForRelease(ctx, sqlc.SymbolFilesForReleaseParams{ProjectID: k.projectID, Release: k.key, Kind: sqlc.SymbolKind(k.kind)})
		if err != nil {
			return nil, "", err
		}
	}
	var latest time.Time
	for _, f := range files {
		if f.UploadedAt.After(latest) {
			latest = f.UploadedAt
		}
	}
	version = versionOf(int64(len(files)), latest)
	if len(files) == 0 {
		return nil, version, nil
	}
	for i := range files {
		if files[i].Data, err = s.symbolBytes(ctx, files[i].Data, files[i].BlobKey); err != nil {
			return nil, "", err
		}
	}
	m, err := parseFiles(k.kind, files)
	return m, version, err
}

// symbolFileBytes loads one row's bytes by id for the sidecar, re-reading
// the row once when its object was replaced under us (fetch's rule).
func (s *Service) symbolFileBytes(ctx context.Context, id int64) ([]byte, error) {
	for attempt := 0; ; attempt++ {
		row, err := s.Store.SymbolFileData(ctx, id)
		if err != nil {
			return nil, err
		}
		b, err := s.symbolBytes(ctx, row.Data, row.BlobKey)
		if errors.Is(err, blob.ErrNotFound) && attempt == 0 {
			continue
		}
		return b, err
	}
}

// parseFiles builds the in-memory mapping of one cache key from its rows.
func parseFiles(kind string, files []sqlc.SymbolFile) (any, error) {
	switch kind {
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

// Event symbolicates one stored event (job kind "symbolicate"; the args
// carry the event's time, so only its partition is read). It returns nil when
// nothing can be resolved yet (the event stays unsymbolicated — a later
// upload re-queues it); only sidecar / database failures are errors, so
// the job retries.
func (s *Service) Event(ctx context.Context, projectID int64, eventID sentry.ID, at time.Time) error {
	row, err := s.Store.GetEventAt(ctx, sqlc.GetEventAtParams{ProjectID: projectID, EventID: eventID, OccurredAt: at})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil // dropped by retention, or never stored
	}
	if err != nil {
		return err
	}
	if row.Symbolicated {
		return nil
	}
	payload, err := s.Store.Payload(ctx, nil, row)
	if err != nil {
		return err
	}
	if payload == nil {
		return nil // imported without a payload: nothing to resolve
	}
	now := time.Now().UTC()
	ev := sentry.ParseEvent(string(row.EventID), row.OccurredAt, payload, now)
	if ev == nil {
		return nil
	}
	if ev.Release == "" && row.Release != nil {
		ev.Release = *row.Release
	}
	frames, ok, err := s.resolve(ctx, projectID, ev, true)
	if err != nil {
		return fmt.Errorf("dsym: %w", err)
	}
	if !ok {
		return nil
	}

	newFP := sentry.Fingerprint(ev, frames)
	location := sentry.Culprit(frames)
	symbols, err := json.Marshal(frames)
	if err != nil {
		return err
	}
	var oldFP sentry.ID
	if row.Fingerprint != nil {
		oldFP = *row.Fingerprint
	}
	return s.Store.Tx(ctx, func(ctx context.Context, tx pgx.Tx, q *sqlc.Queries) error {
		if err := q.SetEventSymbols(ctx, sqlc.SetEventSymbolsParams{
			ProjectID: projectID, EventID: eventID, OccurredAt: row.OccurredAt, Symbols: symbols,
			Fingerprint: newFP.Ptr(), Culprit: nilIfEmpty(location),
		}); err != nil {
			return err
		}
		if newFP == oldFP || newFP == "" {
			return nil
		}
		// The event moved between issues: its hour's per-issue counts
		// are recomputed.
		if err := q.MarkEventStatsDirty(ctx, sqlc.MarkEventStatsDirtyParams{ProjectID: projectID, Buckets: []time.Time{row.OccurredAt.UTC().Truncate(time.Hour)}}); err != nil {
			return err
		}
		if _, err := q.UpsertIssue(ctx, sqlc.UpsertIssueParams{
			ProjectID: projectID, Fingerprint: newFP, Title: ev.IssueTitle(), Level: row.Level,
			ErrorType: nilIfEmpty(ev.ErrorType), Transaction: nilIfEmpty(ev.Transaction), Platform: nilIfEmpty(ev.Platform),
			EventCount: 1, StoredCount: 1, FirstSeen: row.OccurredAt, LastSeen: row.OccurredAt, FirstRelease: row.Release,
			Releases: []string{ev.Release},
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
// image. The sidecar caches symbol files by SymbolKey; the bytes are read
// from the database only when it reports a miss and upload is set.
func (s *Service) dsym(ctx context.Context, projectID int64, ev *sentry.Event, upload bool) ([]sentry.Frame, bool, error) {
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

	var releaseFiles []symbolFileMeta
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
		var load func(context.Context) ([]byte, error)
		if upload {
			id := file.ID
			load = func(ctx context.Context) ([]byte, error) { return s.symbolFileBytes(ctx, id) }
		}
		results, err := s.DSYM.Resolve(ctx, SymbolKey(file.ID, file.UploadedAt), load, addrs)
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

// SymbolKey names one uploaded symbol file to the sidecar: the row's id
// and upload time, so a re-upload under the same name is a new key and a
// stale cached copy is never used.
func SymbolKey(id int64, uploadedAt time.Time) string {
	return fmt.Sprintf("%d-%d", id, uploadedAt.Unix())
}

// symbolFileMeta is a symbol_files row without its data.
type symbolFileMeta = sqlc.SymbolFileMetasForReleaseRow

// dsymFile finds the symbol file for image: by debug_id, else among the
// release's dSYMs by the image's file name (the only one, if just one).
func (s *Service) dsymFile(ctx context.Context, projectID int64, release string, image sentry.DebugImage, releaseFiles *[]symbolFileMeta, loaded *bool) (*symbolFileMeta, error) {
	if image.DebugID != "" {
		id := normalizeDebugID(image.DebugID)
		f, err := s.Store.SymbolFileMetaByDebugID(ctx, sqlc.SymbolFileMetaByDebugIDParams{ProjectID: projectID, DebugID: &id})
		if err == nil && f.Kind == KindDSYM {
			return &symbolFileMeta{ID: f.ID, ProjectID: f.ProjectID, Kind: f.Kind, Release: f.Release, DebugID: f.DebugID, Filename: f.Filename, Size: f.Size, UploadedAt: f.UploadedAt}, nil
		}
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return nil, err
		}
	}
	if release == "" {
		return nil, nil
	}
	if !*loaded {
		files, err := s.Store.SymbolFileMetasForRelease(ctx, sqlc.SymbolFileMetasForReleaseParams{ProjectID: projectID, Release: release, Kind: KindDSYM})
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
	if want == "" && len(*releaseFiles) == 1 { // an image with no name: the release's one dSYM is the only guess there is
		return &(*releaseFiles)[0], nil
	}
	return nil, nil
}

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

// Release re-symbolicates the unsymbolicated events of a release from the
// last ReleaseWindow (job kind "resymbolicate"), called after a symbol
// upload: the cache for the release is dropped and one "symbolicate" job
// per event (at most ReleaseMax) is queued in a single statement, so the
// work is spread over the workers and each event retries on its own.
func (s *Service) Release(ctx context.Context, projectID int64, release string) error {
	s.Invalidate(projectID, release)
	n, err := s.Store.EnqueueSymbolicateRelease(ctx, sqlc.EnqueueSymbolicateReleaseParams{
		ProjectID: projectID, Release: &release, Limit: ReleaseMax,
	})
	if err != nil {
		return err
	}
	if n > 0 {
		slog.Info("symbolicate: release queued", "project", projectID, "release", release, "events", n)
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
