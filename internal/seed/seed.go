// Package seed writes demo data through the real ingest path.
package seed

import (
	"context"

	"github.com/newlix/crashcart/internal/ingest"
)

// Run creates the demo project (if missing) and a week of events/sessions.
func Run(ctx context.Context, in *ingest.Ingester, slug string) error {
	return nil // TODO(seed)
}
