package symbolicate

import (
	"bytes"
	"context"
	"debug/macho"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/at-least/crashcart/internal/sentry"
)

// Sidecar is the dSYM symbolication service (`crashcart symbolicate`): an
// HTTP server around llvm-symbolizer with a disk cache of symbol files.
// It runs in its own container, the only one that needs LLVM installed;
// the main process talks to it through DSYMClient.
//
// Protocol (all JSON):
//
//	POST /symbolicate  {"symbol": key, "frames": [{"address": "0x…", "module": "App"}]}
//	    200 {"frames": [{"function", "filename", "lineno"}]}  index-aligned with the request
//	    404 {"error": "unknown symbol"}                        the file is not cached: PUT it, retry
//	PUT  /symbols/{key}   body = the symbol file          204
//	GET  /health                                            200
//
// The key names one uploaded file (DSYMClient derives it from the row's id
// and upload time, so a re-upload is a new key); the cache is least
// recently used, bounded by MaxBytes.
type Sidecar struct {
	Dir        string        // cache directory
	MaxBytes   int64         // cache bound (0 = SidecarDefaultMaxBytes)
	Symbolizer string        // llvm-symbolizer binary (default: "llvm-symbolizer" on PATH)
	Timeout    time.Duration // per llvm-symbolizer run (default 30 s)
	Log        *slog.Logger

	resolve sync.Once
	bin     string
	binErr  error

	semOnce sync.Once
	sem     chan struct{} // bounds concurrent llvm-symbolizer processes (each can take a GB on a big dSYM)
	bases   sync.Map      // file path → loadAddress result, computed once per cached file
}

// loadInfo is what a symbol file's Mach-O header says about its address
// space: the __TEXT segment's vmaddr (the address the image is linked
// at) and, for a fat file, the slice to symbolize.
type loadInfo struct {
	base uint64
	arch string
}

// loadAddress reads file's load address. A request carries offsets from
// the image's load address (instruction_addr - image_addr); DWARF holds
// linked addresses, and an iOS / macOS image is linked at 0x100000000
// (its __TEXT vmaddr), not 0 — llvm-symbolizer resolves nothing for a
// bare offset. The base is added to every address before it is asked.
// A file that is not Mach-O (ELF: linked at 0) has base 0.
func loadAddress(file string) loadInfo {
	if fat, err := macho.OpenFat(file); err == nil {
		defer fat.Close()
		var pick *macho.FatArch
		for i := range fat.Arches {
			a := &fat.Arches[i]
			if pick == nil || a.Cpu == macho.CpuArm64 {
				pick = a
			}
		}
		if pick == nil {
			return loadInfo{}
		}
		return loadInfo{base: textBase(pick.File), arch: archName(pick.Cpu)}
	}
	f, err := macho.Open(file)
	if err != nil {
		return loadInfo{}
	}
	defer f.Close()
	return loadInfo{base: textBase(f)}
}

func textBase(f *macho.File) uint64 {
	if seg := f.Segment("__TEXT"); seg != nil {
		return seg.Addr
	}
	return 0
}

func archName(cpu macho.Cpu) string {
	switch cpu {
	case macho.CpuArm64:
		return "arm64"
	case macho.CpuAmd64:
		return "x86_64"
	case macho.CpuArm:
		return "arm"
	case macho.Cpu386:
		return "i386"
	}
	return ""
}

func (s *Sidecar) loadInfo(file string) loadInfo {
	if v, ok := s.bases.Load(file); ok {
		return v.(loadInfo)
	}
	li := loadAddress(file)
	s.bases.Store(file, li)
	return li
}

// SidecarDefaultMaxBytes is the cache bound when MaxBytes is 0: room for
// a few dozen large app dSYMs.
const SidecarDefaultMaxBytes = 4 << 30

// symbolKey: no leading dot — dotfiles are the PUT temp files, which the
// eviction skips, so a key like ".x" would escape the cache bound.
var symbolKey = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`)

// Handler returns the HTTP handler.
func (s *Sidecar) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /symbolicate", s.serveSymbolicate)
	mux.HandleFunc("PUT /symbols/{key}", s.servePut)
	mux.HandleFunc("GET /health", func(w http.ResponseWriter, r *http.Request) {
		if _, err := s.symbolizerPath(); err != nil {
			writeJSON(w, http.StatusServiceUnavailable, map[string]string{"status": "llvm-symbolizer not found"})
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	})
	return mux
}

// symbolizerPath resolves the llvm-symbolizer binary once (PATH does not
// change while the sidecar runs; /health is polled every few seconds).
func (s *Sidecar) symbolizerPath() (string, error) {
	s.resolve.Do(func() {
		bin := s.Symbolizer
		if bin == "" {
			bin = "llvm-symbolizer"
		}
		s.bin, s.binErr = exec.LookPath(bin)
	})
	return s.bin, s.binErr
}

func (s *Sidecar) log() *slog.Logger {
	if s.Log != nil {
		return s.Log
	}
	return slog.Default()
}

func (s *Sidecar) path(key string) string { return filepath.Join(s.Dir, key) }

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

// symbolicateRequest is the body of POST /symbolicate.
type symbolicateRequest struct {
	Symbol string `json:"symbol"`
	Frames []struct {
		Address string `json:"address"`
		Module  string `json:"module"`
	} `json:"frames"`
}

func (s *Sidecar) serveSymbolicate(w http.ResponseWriter, r *http.Request) {
	var req symbolicateRequest
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "bad request: " + err.Error()})
		return
	}
	if !symbolKey.MatchString(req.Symbol) || len(req.Frames) == 0 {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "missing symbol or frames"})
		return
	}
	if len(req.Frames) > MaxDSYMFrames {
		req.Frames = req.Frames[:MaxDSYMFrames]
	}
	file := s.path(req.Symbol)
	if _, err := os.Stat(file); err != nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "unknown symbol"})
		return
	}
	now := time.Now()
	os.Chtimes(file, now, now) // LRU: the newest mtime is the most recently used
	addrs := make([]string, len(req.Frames))
	for i, f := range req.Frames {
		addrs[i] = f.Address
	}
	timeout := s.Timeout
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	ctx, cancel := context.WithTimeout(r.Context(), timeout)
	defer cancel()
	results, err := s.run(ctx, file, addrs)
	if err != nil {
		status := http.StatusBadGateway
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			status = http.StatusGatewayTimeout
		}
		s.log().Warn("symbolicate: llvm-symbolizer", "symbol", req.Symbol, "err", err)
		writeJSON(w, status, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"frames": results})
}

// run symbolizes addrs against file in one llvm-symbolizer process
// (--output-style=JSON: one object per address, inlined frames innermost
// first). An address it cannot resolve comes back with an empty Function.
func (s *Sidecar) run(ctx context.Context, file string, addrs []string) ([]DSYMResult, error) {
	bin, err := s.symbolizerPath()
	if err != nil {
		return nil, err
	}
	li := s.loadInfo(file)
	args := []string{"--output-style=JSON", "--obj=" + file}
	if li.arch != "" {
		args = append(args, "--default-arch="+li.arch)
	}
	for _, a := range addrs {
		if off, ok := sentry.ParseHex(a); ok && li.base != 0 {
			a = fmt.Sprintf("0x%x", off+li.base)
		}
		args = append(args, a)
	}
	// One process per request, at most NumCPU at a time: an idle wait
	// beats NumCPU × 1 GB of llvm-symbolizer on one crash burst.
	s.semOnce.Do(func() { s.sem = make(chan struct{}, max(runtime.NumCPU(), 1)) })
	select {
	case s.sem <- struct{}{}:
		defer func() { <-s.sem }()
	case <-ctx.Done():
		return nil, fmt.Errorf("llvm-symbolizer: %w", ctx.Err())
	}
	cmd := exec.CommandContext(ctx, bin, args...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		if ctx.Err() != nil {
			return nil, fmt.Errorf("llvm-symbolizer: %w", ctx.Err())
		}
		return nil, fmt.Errorf("llvm-symbolizer: %v: %s", err, strings.TrimSpace(stderr.String()))
	}
	return parseSymbolizerJSON(stdout.Bytes(), len(addrs))
}

// symbolizerEntry is one address of llvm-symbolizer's JSON output.
type symbolizerEntry struct {
	Address string `json:"Address"`
	Error   *struct {
		Message string `json:"Message"`
	} `json:"Error"`
	Symbol []struct {
		FunctionName string `json:"FunctionName"`
		FileName     string `json:"FileName"`
		Line         int    `json:"Line"`
	} `json:"Symbol"`
}

// parseSymbolizerJSON accepts both shapes LLVM has printed: one JSON array
// of entries, or one entry per line. The result has n elements, in input
// order.
func parseSymbolizerJSON(out []byte, n int) ([]DSYMResult, error) {
	var entries []symbolizerEntry
	dec := json.NewDecoder(bytes.NewReader(out))
	for {
		var v json.RawMessage
		if err := dec.Decode(&v); err == io.EOF {
			break
		} else if err != nil {
			return nil, fmt.Errorf("llvm-symbolizer output: %v: %.200s", err, out)
		}
		if bytes.HasPrefix(bytes.TrimSpace(v), []byte("[")) {
			var arr []symbolizerEntry
			if err := json.Unmarshal(v, &arr); err != nil {
				return nil, fmt.Errorf("llvm-symbolizer output: %v", err)
			}
			entries = append(entries, arr...)
			continue
		}
		var e symbolizerEntry
		if err := json.Unmarshal(v, &e); err != nil {
			return nil, fmt.Errorf("llvm-symbolizer output: %v", err)
		}
		entries = append(entries, e)
	}
	results := make([]DSYMResult, n)
	for i := 0; i < n && i < len(entries); i++ {
		e := entries[i]
		if e.Error != nil || len(e.Symbol) == 0 {
			continue
		}
		results[i] = DSYMResult{Function: e.Symbol[0].FunctionName, Filename: e.Symbol[0].FileName, Lineno: e.Symbol[0].Line}
	}
	return results, nil
}

func (s *Sidecar) servePut(w http.ResponseWriter, r *http.Request) {
	key := r.PathValue("key")
	if !symbolKey.MatchString(key) {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "bad key"})
		return
	}
	if err := os.MkdirAll(s.Dir, 0o755); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	tmp, err := os.CreateTemp(s.Dir, "."+key+".*")
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	n, err := io.Copy(tmp, http.MaxBytesReader(w, r.Body, MaxUpload+1<<20)) // the upload endpoint's bound
	tmp.Close()
	if err != nil || n == 0 {
		os.Remove(tmp.Name())
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "body: truncated, empty or too large"})
		return
	}
	if err := os.Rename(tmp.Name(), s.path(key)); err != nil {
		os.Remove(tmp.Name())
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	s.bases.Delete(s.path(key)) // a re-PUT under one key is the same file, but never serve a stale header
	s.evict(key)
	w.WriteHeader(http.StatusNoContent)
}

// evict drops the least recently used files until the cache fits MaxBytes
// (keep is never dropped: it was just written and is about to be used).
func (s *Sidecar) evict(keep string) {
	max := s.MaxBytes
	if max <= 0 {
		max = SidecarDefaultMaxBytes
	}
	entries, err := os.ReadDir(s.Dir)
	if err != nil {
		return
	}
	type file struct {
		name  string
		size  int64
		mtime time.Time
	}
	var files []file
	var total int64
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		if strings.HasPrefix(e.Name(), ".") { // a PUT's temp file; one left by a crash mid-write is swept
			if info, err := e.Info(); err == nil && time.Since(info.ModTime()) > time.Hour {
				os.Remove(s.path(e.Name()))
			}
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		files = append(files, file{e.Name(), info.Size(), info.ModTime()})
		total += info.Size()
	}
	slices.SortFunc(files, func(a, b file) int { return a.mtime.Compare(b.mtime) })
	for _, f := range files {
		if total <= max {
			return
		}
		if f.name == keep {
			continue
		}
		if err := os.Remove(s.path(f.name)); err == nil {
			total -= f.size
			s.log().Info("symbolicate: evicted", "symbol", f.name, "size", f.size)
		}
	}
}
