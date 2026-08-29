// Package alerts delivers notifications (webhook, Telegram) for new issues,
// regressions and crash spikes.
package alerts

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/crashcartapp/crashcart/internal/config"
	"github.com/crashcartapp/crashcart/internal/db/sqlc"
	"github.com/crashcartapp/crashcart/internal/sentry"
	"github.com/crashcartapp/crashcart/internal/store"
)

// Alert types (alert_rules.type).
const (
	TypeNewIssue   = "new_issue"
	TypeRegression = "regression"
	TypeCrashSpike = "crash_spike"
)

// Spike thresholds: at least MinSpikeCrashes in the last hour and
// SpikeFactor × the hourly mean of the 24 h before.
const (
	MinSpikeCrashes = 10
	SpikeFactor     = 3
	defaultCooldown = 60
)

// TelegramAPI is the Telegram Bot API base; tests override it.
var TelegramAPI = "https://api.telegram.org"

// Notifier sends alerts to a project's channels, honoring rule cooldowns.
type Notifier struct {
	Store *store.Store
	Cfg   config.Config
	Log   *slog.Logger
	HTTP  *http.Client
}

// Payload is the JSON body of a webhook call (and the source of the
// Telegram text). Fields are snake_case like the rest of the API.
type Payload struct {
	Type         string  `json:"type"`
	Project      string  `json:"project"`
	ProjectSlug  string  `json:"project_slug"`
	Title        string  `json:"title"`
	Fingerprint  string  `json:"fingerprint,omitempty"`
	Level        string  `json:"level,omitempty"`
	EventCount   int64   `json:"event_count,omitempty"`
	FirstRelease *string `json:"first_release,omitempty"`
	LastRelease  *string `json:"last_release,omitempty"`
	Recent       *int64  `json:"recent,omitempty"`   // crash_spike: crashes in the last hour
	Baseline     *int64  `json:"baseline,omitempty"` // crash_spike: crashes in the 24 h before
	URL          string  `json:"url"`
}

// EnsureRules creates the project's three default rules (all enabled,
// 60 min cooldown) when they do not exist yet. Existing rows are kept.
func EnsureRules(ctx context.Context, st *store.Store, projectID int64) error {
	return st.EnsureAlertRules(ctx, sqlc.EnsureAlertRulesParams{ProjectID: projectID, CooldownMinutes: defaultCooldown})
}

// Issue handles job kind "alert" ({type: new_issue|regression, fingerprint}).
// Delivery is best effort: the cooldown is claimed first, so a retry could
// not re-send anyway; per-channel failures are logged and nil is returned.
func (n *Notifier) Issue(ctx context.Context, projectID int64, typ, fingerprint string) error {
	if typ != TypeNewIssue && typ != TypeRegression {
		return fmt.Errorf("alert: unknown type %q", typ)
	}
	claimed, err := n.Store.TouchAlertRule(ctx, sqlc.TouchAlertRuleParams{ProjectID: projectID, Type: sqlc.AlertType(typ)})
	if err != nil {
		return err
	}
	if claimed == 0 {
		return nil // disabled, cooling down, or no rule row
	}
	fp, ok := sentry.ParseID(fingerprint)
	if !ok {
		return nil
	}
	issue, err := n.Store.GetIssue(ctx, sqlc.GetIssueParams{ProjectID: projectID, Fingerprint: fp})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	p, err := n.Store.GetProjectByID(ctx, projectID)
	if err != nil {
		return err
	}
	payload := Payload{
		Type: typ, Project: p.Name, ProjectSlug: p.Slug, Title: issue.Title, Fingerprint: string(issue.Fingerprint),
		Level: string(issue.Level), EventCount: issue.EventCount, FirstRelease: issue.FirstRelease, LastRelease: issue.LastRelease,
		URL: n.link(p.Slug, "/issues/"+url.PathEscape(fingerprint)),
	}
	n.notify(ctx, projectID, payload)
	return nil
}

// CheckSpikes evaluates the crash_spike rule of every project (scheduler).
func (n *Notifier) CheckSpikes(ctx context.Context) error {
	// "recent" is the exact last hour from the raw table; the baseline is
	// the ~24 hourly buckets before it. Bucket keys are start times, so
	// `bucket < recentFrom` includes the partial bucket the recent hour
	// starts in: no gap between the two. One query covers every project.
	now := time.Now().UTC()
	recentFrom := now.Add(-time.Hour)
	rows, err := n.Store.CrashSpikeInputs(ctx, sqlc.CrashSpikeInputsParams{
		RecentFrom: recentFrom, BaselineFrom: recentFrom.Truncate(time.Hour).Add(-24 * time.Hour), BaselineTo: recentFrom,
	})
	if err != nil {
		return err
	}
	var errs []error
	for _, in := range rows {
		if !IsSpike(in.Recent, in.Baseline) {
			continue
		}
		if err := n.spike(ctx, in.ProjectID, in.Recent, in.Baseline); err != nil {
			errs = append(errs, fmt.Errorf("project %d: %w", in.ProjectID, err))
		}
	}
	return errors.Join(errs...)
}

// spike claims the crash_spike cooldown for the project and notifies.
func (n *Notifier) spike(ctx context.Context, projectID, recent, baseline int64) error {
	if err := EnsureRules(ctx, n.Store, projectID); err != nil {
		return err
	}
	claimed, err := n.Store.TouchAlertRule(ctx, sqlc.TouchAlertRuleParams{ProjectID: projectID, Type: TypeCrashSpike})
	if err != nil {
		return err
	}
	if claimed == 0 {
		return nil
	}
	p, err := n.Store.GetProjectByID(ctx, projectID)
	if err != nil {
		return err
	}
	n.notify(ctx, p.ID, Payload{
		Type: TypeCrashSpike, Project: p.Name, ProjectSlug: p.Slug,
		Title:  fmt.Sprintf("Crash spike: %d crashes in the last hour (baseline %.1f/h)", recent, float64(baseline)/24),
		Level:  "fatal",
		Recent: &recent, Baseline: &baseline,
		URL: n.link(p.Slug, "?crash=1&range=1h"),
	})
	return nil
}

// IsSpike is the crash-spike rule: recent crashes in the last hour versus
// the hourly mean of the 24 h before it.
func IsSpike(recent, baseline int64) bool {
	mean := float64(baseline) / 24
	return recent >= MinSpikeCrashes && float64(recent) >= SpikeFactor*mean
}

// link builds the viewer URL for a project path; relative when PUBLIC_URL
// is not configured.
func (n *Notifier) link(slug, path string) string {
	return n.Cfg.PublicURL + "/p/" + url.PathEscape(slug) + path
}

// notify sends payload to every channel of the project; failures are logged.
func (n *Notifier) notify(ctx context.Context, projectID int64, payload Payload) {
	channels, err := n.Store.ListAlertChannels(ctx, projectID)
	if err != nil {
		n.log().Error("alert: list channels", "project", projectID, "err", err)
		return
	}
	for _, ch := range channels {
		if err := n.send(ctx, ch, payload); err != nil {
			n.log().Error("alert: channel failed", "project", projectID, "channel", ch.ID, "kind", ch.Kind, "type", payload.Type, "err", err)
			continue
		}
		n.log().Info("alert: sent", "project", projectID, "channel", ch.ID, "kind", ch.Kind, "type", payload.Type, "fingerprint", payload.Fingerprint)
	}
}

func (n *Notifier) send(ctx context.Context, ch sqlc.AlertChannel, payload Payload) error {
	var cfg struct {
		URL    string `json:"url"`
		ChatID string `json:"chat_id"`
	}
	if err := json.Unmarshal(ch.Config, &cfg); err != nil {
		return fmt.Errorf("config: %w", err)
	}
	switch ch.Kind {
	case "webhook":
		if cfg.URL == "" {
			return errors.New("webhook: missing url")
		}
		body, _ := json.Marshal(payload)
		return n.post(ctx, cfg.URL, body)
	case "telegram":
		if n.Cfg.TelegramBotToken == "" {
			return errors.New("telegram: TELEGRAM_BOT_TOKEN not set")
		}
		if cfg.ChatID == "" {
			return errors.New("telegram: missing chat_id")
		}
		body, _ := json.Marshal(map[string]any{"chat_id": cfg.ChatID, "text": TelegramText(payload), "disable_web_page_preview": true})
		return n.post(ctx, TelegramAPI+"/bot"+n.Cfg.TelegramBotToken+"/sendMessage", body)
	default:
		return fmt.Errorf("unknown channel kind %q", ch.Kind)
	}
}

// TelegramText renders the short plain-text message.
func TelegramText(p Payload) string {
	var b strings.Builder
	switch p.Type {
	case TypeNewIssue:
		b.WriteString("New issue")
	case TypeRegression:
		b.WriteString("Regression")
	case TypeCrashSpike:
		b.WriteString("Crash spike")
	default:
		b.WriteString(p.Type)
	}
	fmt.Fprintf(&b, " in %s\n%s", p.Project, p.Title)
	if p.Type != TypeCrashSpike {
		fmt.Fprintf(&b, "\n%s · %d events", p.Level, p.EventCount)
		if p.LastRelease != nil && *p.LastRelease != "" {
			fmt.Fprintf(&b, " · release %s", *p.LastRelease)
		}
	}
	if p.URL != "" {
		b.WriteString("\n" + p.URL)
	}
	return b.String()
}

func (n *Notifier) post(ctx context.Context, target string, body []byte) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, target, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "crashcart-alerts")
	resp, err := n.client().Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("%s", resp.Status)
	}
	return nil
}

func (n *Notifier) client() *http.Client {
	if n.HTTP != nil {
		return n.HTTP
	}
	return &http.Client{Timeout: 15 * time.Second}
}

func (n *Notifier) log() *slog.Logger {
	if n.Log != nil {
		return n.Log
	}
	return slog.Default()
}
