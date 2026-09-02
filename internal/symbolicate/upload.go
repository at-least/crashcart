package symbolicate

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"path"
	"regexp"
	"strings"
	"time"

	"github.com/at-least/crashcart/internal/db/sqlc"
)

// MaxUpload caps one symbol upload (and one zip entry); a zip may unpack
// to MaxZipTotal over at most MaxZipEntries files (a 50 MB deflate of
// thousands of 50 MB entries is not a symbol upload).
const (
	MaxUpload     = 50 << 20
	MaxZipTotal   = 4 * MaxUpload
	MaxZipEntries = 256
)

// UploadError is a caller mistake (bad kind, empty or malformed file);
// HTTP handlers map it to 400.
type UploadError string

func (e UploadError) Error() string { return string(e) }

var kinds = map[string]bool{KindProGuard: true, KindSourceMap: true, KindDSYM: true}

// uuidRe matches the `<uuid>.txt` names sentry-cli gives ProGuard mappings.
var uuidRe = regexp.MustCompile(`(?i)^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

// Upload stores one symbol file — or every entry of a zip — for a release,
// detecting the kind per file when kind is "", deriving debug ids (Mach-O
// LC_UUID for dSYMs, `<uuid>.txt` for ProGuard), then re-queues the
// release's unsymbolicated events and drops cached mappings. This is the
// single write path used by the JSON API, the sentry-cli endpoint and the
// viewer.
func (s *Service) Upload(ctx context.Context, projectID int64, release, kind, filename string, data []byte) ([]sqlc.UpsertSymbolFileRow, error) {
	return s.upload(ctx, projectID, release, kind, filename, data, nil)
}

// UploadWithDebugID is Upload with a caller-supplied debug id (sentry-cli
// computes one for ProGuard mappings and sends it in the assemble request;
// the Android SDK reports the same uuid in debug_meta). Zips fall back to
// per-file detection.
func (s *Service) UploadWithDebugID(ctx context.Context, projectID int64, release, kind, filename string, data []byte, debugID string) ([]sqlc.UpsertSymbolFileRow, error) {
	var id *string
	if debugID = strings.ToLower(strings.TrimSpace(debugID)); debugID != "" {
		id = &debugID
	}
	return s.upload(ctx, projectID, release, kind, filename, data, id)
}

func (s *Service) upload(ctx context.Context, projectID int64, release, kind, filename string, data []byte, debugID *string) ([]sqlc.UpsertSymbolFileRow, error) {
	if len(data) == 0 {
		return nil, UploadError("empty file")
	}
	if len(data) > MaxUpload {
		return nil, UploadError("upload exceeds 50 MB")
	}
	if kind != "" && !kinds[kind] {
		return nil, UploadError("kind must be proguard, sourcemap or dsym")
	}
	var rows []sqlc.UpsertSymbolFileRow
	if isZip(filename, data) {
		zr, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
		if err != nil {
			return nil, UploadError("invalid zip: " + err.Error())
		}
		if len(zr.File) > MaxZipEntries {
			return nil, UploadError(fmt.Sprintf("zip has more than %d entries", MaxZipEntries))
		}
		// Bound the whole archive before storing anything (the directory's
		// sizes first; the bytes actually read are counted again below, as
		// a header can lie).
		var declared uint64
		for _, f := range zr.File {
			declared += f.UncompressedSize64
		}
		if declared > MaxZipTotal {
			return nil, UploadError("zip unpacks to more than 200 MB")
		}
		total := 0
		for _, f := range zr.File {
			name := path.Clean("/" + strings.ReplaceAll(f.Name, "\\", "/"))[1:]
			if f.FileInfo().IsDir() || name == "" || strings.HasPrefix(name, "__MACOSX/") || strings.HasPrefix(path.Base(name), ".") {
				continue
			}
			if f.UncompressedSize64 > MaxUpload {
				return nil, UploadError("zip entry too large: " + name)
			}
			rc, err := f.Open()
			if err != nil {
				return nil, UploadError("invalid zip entry: " + name)
			}
			content, err := io.ReadAll(io.LimitReader(rc, MaxUpload+1))
			rc.Close()
			if err != nil {
				return nil, err
			}
			if len(content) > MaxUpload {
				return nil, UploadError("zip entry too large: " + name)
			}
			if total += len(content); total > MaxZipTotal {
				return nil, UploadError("zip unpacks to more than 200 MB")
			}
			if len(content) == 0 {
				continue
			}
			k := kind
			if k == "" {
				if k = classify(name, content); k == "" {
					continue // Info.plist, Relocations/*.yml, … — not a symbol file
				}
			}
			row, err := s.upsert(ctx, projectID, release, k, name, content, nil)
			if err != nil {
				return nil, err
			}
			rows = append(rows, row)
		}
		if len(rows) == 0 {
			return nil, UploadError("zip contains no symbol files")
		}
	} else {
		if kind == "" {
			if kind = classify(filename, data); kind == "" {
				return nil, UploadError("unrecognized symbol file (expected a ProGuard mapping, a source map or a Mach-O dSYM)")
			}
		}
		row, err := s.upsert(ctx, projectID, release, kind, baseName(filename), data, debugID)
		if err != nil {
			return nil, err
		}
		rows = append(rows, row)
	}
	if release != "" {
		args, _ := json.Marshal(map[string]string{"release": release})
		if err := s.Store.EnqueueJob(ctx, sqlc.EnqueueJobParams{Kind: "resymbolicate", ProjectID: projectID, Args: args, RunAfter: time.Now()}); err != nil {
			return nil, err
		}
	}
	s.Invalidate(projectID, release)
	return rows, nil
}

// classify is DetectKind plus a content check for binaries: an opaque file
// only counts as a dSYM when it actually parses as Mach-O (Info.plist,
// Relocations/*.yml and nested archives inside a dSYM zip are skipped).
func classify(name string, data []byte) string {
	k := DetectKind(name, data[:min(len(data), 4096)])
	if k == KindDSYM || k == "" {
		if IsMachO(data) {
			return KindDSYM
		}
		return ""
	}
	return k
}

func (s *Service) upsert(ctx context.Context, projectID int64, release, kind, name string, data []byte, debugID *string) (sqlc.UpsertSymbolFileRow, error) {
	if debugID == nil {
		debugID = DebugIDFor(kind, name, data)
	}
	// A dSYM uploaded without a release (sentry-cli debug-files upload)
	// is named after the binary — "App" for every build — and the row key
	// is (project, kind, release, filename): build 2.1 would replace 2.0's
	// dSYM while 2.0 is still crashing in the field. The debug id makes
	// the name unique per build (and keeps the "App." prefix the release
	// lookup matches on, should the row be tagged with a release later).
	if release == "" && kind == KindDSYM && debugID != nil && !strings.Contains(name, *debugID) {
		name = name + "." + *debugID
	}
	return s.Store.UpsertSymbolFile(ctx, sqlc.UpsertSymbolFileParams{
		ProjectID: projectID, Kind: sqlc.SymbolKind(kind), Release: nilIfEmpty(release), DebugID: debugID,
		Filename: name, Size: int64(len(data)), Data: data,
	})
}

// DebugIDFor derives the debug id of a symbol file: the Mach-O LC_UUID for
// dSYM binaries, the `<uuid>` stem sentry-cli gives ProGuard mappings.
func DebugIDFor(kind, filename string, data []byte) *string {
	base := baseName(filename)
	stem := strings.TrimSuffix(base, path.Ext(base))
	switch kind {
	case KindProGuard:
		if uuidRe.MatchString(stem) {
			s := strings.ToLower(stem)
			return &s
		}
	case KindDSYM:
		if id, ok := MachOUUID(data); ok {
			return &id
		}
	}
	return nil
}

func isZip(filename string, data []byte) bool {
	return bytes.HasPrefix(data, []byte("PK\x03\x04")) || strings.HasSuffix(strings.ToLower(filename), ".zip")
}

// Associate tags a release onto mappings that were uploaded without one
// (sentry-cli's follow-up after `upload-proguard`). debugID targets one
// mapping; "" tags the project's ProGuard uploads of the last hour. Cached
// release lookups are dropped so the newly tagged mapping is found.
func (s *Service) Associate(ctx context.Context, projectID int64, release, debugID string) error {
	release = strings.TrimSpace(release)
	if release == "" {
		return nil
	}
	var id *string
	if debugID = strings.ToLower(strings.TrimSpace(debugID)); debugID != "" {
		id = &debugID
	}
	if _, err := s.Store.SetSymbolFileRelease(ctx, sqlc.SetSymbolFileReleaseParams{
		ProjectID: projectID, Release: &release, DebugID: id, Since: time.Now().Add(-time.Hour),
	}); err != nil {
		return err
	}
	s.Invalidate(projectID, release)
	return nil
}

// ReadMultipartUpload reads the `file` part of a multipart request, bounded
// by MaxUpload, for the HTTP handlers that accept symbol uploads. Caller
// mistakes are UploadError.
func ReadMultipartUpload(w http.ResponseWriter, r *http.Request) (name string, data []byte, err error) {
	r.Body = http.MaxBytesReader(w, r.Body, MaxUpload+1<<20)
	if err := r.ParseMultipartForm(8 << 20); err != nil {
		var mbe *http.MaxBytesError
		if errors.As(err, &mbe) {
			return "", nil, UploadError("upload exceeds 50 MB")
		}
		return "", nil, UploadError("multipart form expected: " + err.Error())
	}
	f, fh, err := r.FormFile("file")
	if err != nil {
		return "", nil, UploadError(`multipart field "file" is required`)
	}
	defer f.Close()
	data, err = io.ReadAll(io.LimitReader(f, MaxUpload+1))
	if err != nil {
		return "", nil, err
	}
	if len(data) > MaxUpload {
		return "", nil, UploadError("upload exceeds 50 MB")
	}
	return fh.Filename, data, nil
}
