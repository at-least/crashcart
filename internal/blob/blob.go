// Package blob is the object store: event payloads, symbol files and
// sentry-cli upload chunks live here, never in Postgres. The only backend
// is an S3-compatible bucket (S3); Memory serves tests.
//
// Event payloads are batched into pack objects under events/<date>/…
// (Packer: one PUT per batch, one ranged GET per read; the event row holds
// the Ref). A symbol file is at symbols/<project>/<id>, an upload chunk at
// chunks/<sha1> — keys derived from the row. Retention is the bucket's
// lifecycle rules per prefix (retention.Reconcile sets them); an object
// that outlives its rows (a rolled-back envelope's pack bytes, a deleted
// symbol file) simply expires with the rest.
package blob

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
)

// ErrNotFound: no object at that key.
var ErrNotFound = errors.New("blob: not found")

// Store is what the rest of the code needs from the object store. Put /
// Get are whole objects (an implementation may compress them in transit);
// PutRaw / GetRange are the pack path: bytes stored exactly as given, read
// back by range.
type Store interface {
	Put(ctx context.Context, key string, data []byte) error
	Get(ctx context.Context, key string) ([]byte, error)
	PutRaw(ctx context.Context, key string, data []byte) error
	GetRange(ctx context.Context, key string, off, n int64) ([]byte, error)
	Delete(ctx context.Context, key string) error
}

// Key prefixes: one per kind of object, so lifecycle rules can differ.
const (
	PrefixEvents  = "events/"
	PrefixSymbols = "symbols/"
	PrefixChunks  = "chunks/"
)

// SymbolKey is where a symbol file's bytes live (symbol_files.id).
func SymbolKey(projectID, id int64) string {
	return fmt.Sprintf("%s%d/%d", PrefixSymbols, projectID, id)
}

// ChunkKey is where a sentry-cli upload chunk waits for assembly.
func ChunkKey(sha1 string) string { return PrefixChunks + sha1 }

// Memory is an in-process Store for tests.
type Memory struct {
	mu   sync.Mutex
	data map[string][]byte
}

// NewMemory returns an empty in-memory store.
func NewMemory() *Memory { return &Memory{data: map[string][]byte{}} }

func (m *Memory) Put(_ context.Context, key string, data []byte) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.data[key] = append([]byte(nil), data...)
	return nil
}

func (m *Memory) Get(_ context.Context, key string) ([]byte, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	b, ok := m.data[key]
	if !ok {
		return nil, ErrNotFound
	}
	return append([]byte(nil), b...), nil
}

func (m *Memory) PutRaw(ctx context.Context, key string, data []byte) error {
	return m.Put(ctx, key, data)
}

func (m *Memory) GetRange(_ context.Context, key string, off, n int64) ([]byte, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	b, ok := m.data[key]
	if !ok {
		return nil, ErrNotFound
	}
	if off < 0 || off+n > int64(len(b)) {
		return nil, fmt.Errorf("blob: range %d+%d beyond %s (%d bytes)", off, n, key, len(b))
	}
	return append([]byte(nil), b[off:off+n]...), nil
}

func (m *Memory) Delete(_ context.Context, key string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.data, key)
	return nil
}

// Len is the number of stored objects (tests).
func (m *Memory) Len() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.data)
}

// Keys lists the stored keys under prefix, sorted (tests).
func (m *Memory) Keys(prefix string) []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []string
	for k := range m.data {
		if strings.HasPrefix(k, prefix) {
			out = append(out, k)
		}
	}
	sort.Strings(out)
	return out
}
