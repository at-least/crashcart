package symbolicate

import (
	"context"

	"github.com/newlix/crashcart/internal/sentry"
	"github.com/newlix/crashcart/internal/store"
)

// Service resolves frames for a project: in-process ProGuard / source-map
// mappings (cached per project+release) and dSYM through the sidecar.
// It implements ingest.Symbolicator (Inline) and the job handlers.
type Service struct {
	Store *store.Store
	DSYM  *DSYMClient // Enabled() false when no sidecar
}

// Inline resolves ev's frames when a proguard/sourcemap mapping for its
// release is already cached in memory. ok=false otherwise.
func (s *Service) Inline(ctx context.Context, projectID int64, ev *sentry.Event) ([]sentry.Frame, bool) {
	return nil, false // TODO(symbolicate): implemented in cache.go
}

// Event symbolicates one stored event (job kind "symbolicate").
func (s *Service) Event(ctx context.Context, projectID, eventID int64) error {
	return nil // TODO(symbolicate)
}

// Release re-queues every unsymbolicated event of a release (job kind
// "resymbolicate"), called after a symbol upload.
func (s *Service) Release(ctx context.Context, projectID int64, release string) error {
	return nil // TODO(symbolicate)
}
