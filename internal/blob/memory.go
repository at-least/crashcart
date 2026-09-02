package blob

import (
	"context"
	"fmt"
	"slices"
	"sync"
)

// Memory is an in-process Store for tests.
type Memory struct {
	mu   sync.Mutex
	data map[string][]byte
	gets int
}

func (m *Memory) Put(_ context.Context, key string, data []byte) error {
	if err := checkKey(key); err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.data == nil {
		m.data = map[string][]byte{}
	}
	m.data[key] = slices.Clone(data)
	return nil
}

func (m *Memory) Get(_ context.Context, key string) ([]byte, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	d, ok := m.data[key]
	if !ok {
		return nil, ErrNotFound
	}
	m.gets++
	return slices.Clone(d), nil
}

func (m *Memory) GetRange(_ context.Context, key string, off, n int64) ([]byte, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	d, ok := m.data[key]
	if !ok {
		return nil, ErrNotFound
	}
	if off < 0 || n < 0 || off+n > int64(len(d)) {
		return nil, fmt.Errorf("blob: range %d+%d outside %d bytes of %s", off, n, len(d), key)
	}
	m.gets++
	return slices.Clone(d[off : off+n]), nil
}

// Gets counts Get and GetRange calls — for tests asserting how many
// round trips a read path takes.
func (m *Memory) Gets() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.gets
}

func (m *Memory) Delete(_ context.Context, key string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.data, key)
	return nil
}

// Keys lists the stored keys, sorted — for assertions.
func (m *Memory) Keys() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	keys := make([]string, 0, len(m.data))
	for k := range m.data {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	return keys
}
