// Package export streams every table as NDJSON and loads it back.
package export

import (
	"context"
	"io"

	"github.com/newlix/crashcart/internal/store"
)

// Options narrows an export.
type Options struct {
	Project string // slug; "" = all
}

// Export writes NDJSON to w.
func Export(ctx context.Context, st *store.Store, w io.Writer, opt Options) error {
	return nil // TODO(export)
}

// Report summarizes an import.
type Report struct {
	Rows map[string]int64
}

// Import loads NDJSON from r (idempotent).
func Import(ctx context.Context, st *store.Store, r io.Reader) (Report, error) {
	return Report{}, nil // TODO(export)
}
