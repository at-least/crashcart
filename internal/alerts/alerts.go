// Package alerts delivers notifications (webhook, Telegram) for new issues,
// regressions and crash spikes.
package alerts

import (
	"context"
	"log/slog"
	"net/http"

	"github.com/newlix/crashcart/internal/config"
	"github.com/newlix/crashcart/internal/store"
)

// Notifier sends alerts to a project's channels, honoring rule cooldowns.
type Notifier struct {
	Store *store.Store
	Cfg   config.Config
	Log   *slog.Logger
	HTTP  *http.Client
}

// Issue handles job kind "alert" ({type: new_issue|regression, fingerprint}).
func (n *Notifier) Issue(ctx context.Context, projectID int64, typ, fingerprint string) error {
	return nil // TODO(alerts)
}

// CheckSpikes evaluates the crash_spike rule of every project (scheduler).
func (n *Notifier) CheckSpikes(ctx context.Context) error {
	return nil // TODO(alerts)
}
