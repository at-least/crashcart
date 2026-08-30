// Package alerts delivers notifications (webhook, Telegram) for new issues,
// regressions and crash spikes.
package alerts

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/crashcartapp/crashcart/internal/metrics"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strings"
	"sync"
	"syscall"
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

// AlertsTotal counts deliveries by alert type, channel kind and outcome.
var AlertsTotal = metrics.NewCounter("crashcart_alerts_total", "Alert deliveries by type, channel kind and outcome (sent, failed).", "type", "kind", "outcome")

// AlertsSuppressed counts alerts not sent because the rule was cooling
// down or disabled.
var AlertsSuppressed = metrics.NewCounter("crashcart_alerts_suppressed_total", "Alerts not sent: the rule was disabled or cooling down.", "type")

// TelegramAPI is the Telegram Bot API base; tests override it.
var TelegramAPI = "https://api.telegram.org"

// Notifier sends alerts to a project's channels, honoring rule cooldowns.
type Notifier struct {
	Store *store.Store
	Cfg   config.Config
	Log   *slog.Logger
	HTTP  *http.Client // tests; nil = the hardened client (see client)

	once sync.Once
	http *http.Client
}

// Payload is the JSON body of a webhook call (and the source of the
// Telegram text). Fields are snake_case like the rest of the API.
type Payload struct {
	Type          string  `json:"type"`
	Project       string  `json:"project"`
	ProjectSlug   string  `json:"project_slug"`
	Title         string  `json:"title"`
	Fingerprint   string  `json:"fingerprint,omitempty"`
	Level         string  `json:"level,omitempty"`
	EventCount    int64   `json:"event_count,omitempty"`
	FirstRelease  *string `json:"first_release,omitempty"`
	LastRelease   *string `json:"last_release,omitempty"`
	MoreSinceLast *int64  `json:"more_since_last,omitempty"` // new_issue: other issues that appeared since the last alert (suppressed by the cooldown)
	Recent        *int64  `json:"recent,omitempty"`          // crash_spike: crashes in the last hour
	Baseline      *int64  `json:"baseline,omitempty"`        // crash_spike: crashes in the 24 h before
	URL           string  `json:"url"`
}

// EnsureRules creates the project's three default rules (all enabled,
// 60 min cooldown) when they do not exist yet. Existing rows are kept.
func EnsureRules(ctx context.Context, st *store.Store, projectID int64) error {
	return st.EnsureAlertRules(ctx, sqlc.EnsureAlertRulesParams{ProjectID: projectID, CooldownMinutes: defaultCooldown})
}

// Issue handles job kind "alert" ({type: new_issue|regression, fingerprint}).
// The cooldown is claimed first (atomically, so replicas cannot both
// send); when nothing could be delivered — every channel failed, or there
// is none — the claim is given back, so one outage does not also eat the
// next alert of the hour. Per-channel failures are logged and nil is
// returned. The cooldown is per project and type: the payload says how
// many other issues appeared since the last alert, so the ones it
// suppresses are not invisible.
func (n *Notifier) Issue(ctx context.Context, projectID int64, typ, fingerprint string) error {
	if typ != TypeNewIssue && typ != TypeRegression {
		return fmt.Errorf("alert: unknown type %q", typ)
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
	previous, claimed, err := n.claim(ctx, projectID, sqlc.AlertType(typ))
	if err != nil || !claimed {
		if err == nil {
			AlertsSuppressed.Inc(typ)
		}
		return err // disabled, cooling down, or no rule row
	}
	payload := Payload{
		Type: typ, Project: p.Name, ProjectSlug: p.Slug, Title: issue.Title, Fingerprint: string(issue.Fingerprint),
		Level: string(issue.Level), EventCount: issue.EventCount, FirstRelease: issue.FirstRelease, LastRelease: issue.LastRelease,
		URL: n.link(p.Slug, "/issues/"+url.PathEscape(fingerprint)),
	}
	if typ == TypeNewIssue && previous != nil {
		if more, err := n.Store.CountNewIssues(ctx, sqlc.CountNewIssuesParams{ProjectID: projectID, FirstSeen: *previous}); err == nil && more > 1 {
			m := more - 1 // this one
			payload.MoreSinceLast = &m
		}
	}
	if n.notify(ctx, projectID, payload) == 0 {
		if err := n.Store.UnclaimAlertRule(ctx, sqlc.UnclaimAlertRuleParams{ProjectID: projectID, Type: sqlc.AlertType(typ), Previous: previous}); err != nil {
			return err
		}
	}
	return nil
}

// CheckSpikes evaluates the crash_spike rule of every project (scheduler).
func (n *Notifier) CheckSpikes(ctx context.Context) error {
	// "recent" is the exact last hour from the raw table; the baseline is
	// the 24 full hourly buckets before the bucket the recent hour starts
	// in (that partial bucket would count its crashes on both sides).
	// One query covers every project.
	now := time.Now().UTC()
	recentFrom := now.Add(-time.Hour)
	baselineTo := recentFrom.Truncate(time.Hour)
	rows, err := n.Store.CrashSpikeInputs(ctx, sqlc.CrashSpikeInputsParams{
		RecentFrom: recentFrom, BaselineFrom: baselineTo.Add(-24 * time.Hour), BaselineTo: baselineTo,
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
	previous, claimed, err := n.claim(ctx, projectID, TypeCrashSpike)
	if err != nil || !claimed {
		if err == nil {
			AlertsSuppressed.Inc(TypeCrashSpike)
		}
		return err
	}
	p, err := n.Store.GetProjectByID(ctx, projectID)
	if err != nil {
		return err
	}
	sent := n.notify(ctx, p.ID, Payload{
		Type: TypeCrashSpike, Project: p.Name, ProjectSlug: p.Slug,
		Title:  fmt.Sprintf("Crash spike: %d crashes in the last hour (baseline %.1f/h)", recent, float64(baseline)/24),
		Level:  "fatal",
		Recent: &recent, Baseline: &baseline,
		URL: n.link(p.Slug, "?crash=1&range=1h"),
	})
	if sent == 0 {
		return n.Store.UnclaimAlertRule(ctx, sqlc.UnclaimAlertRuleParams{ProjectID: projectID, Type: TypeCrashSpike, Previous: previous})
	}
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

// claim takes the rule's cooldown; previous is last_triggered before the
// claim, for UnclaimAlertRule when nothing could be delivered.
func (n *Notifier) claim(ctx context.Context, projectID int64, typ sqlc.AlertType) (previous *time.Time, claimed bool, err error) {
	rule, err := n.Store.GetAlertRule(ctx, sqlc.GetAlertRuleParams{ProjectID: projectID, Type: typ})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	rows, err := n.Store.TouchAlertRule(ctx, sqlc.TouchAlertRuleParams{ProjectID: projectID, Type: typ})
	if err != nil {
		return nil, false, err
	}
	return rule.LastTriggered, rows > 0, nil
}

// notify sends payload to every channel of the project; failures are
// logged. Returns how many channels took it.
func (n *Notifier) notify(ctx context.Context, projectID int64, payload Payload) int {
	channels, err := n.Store.ListAlertChannels(ctx, projectID)
	if err != nil {
		n.log().Error("alert: list channels", "project", projectID, "err", err)
		return 0
	}
	sent := 0
	for _, ch := range channels {
		if err := n.send(ctx, ch, payload); err != nil {
			outcome := "failed"
			if errors.Is(err, ErrBlockedURL) {
				outcome = "blocked"
			}
			AlertsTotal.Inc(payload.Type, string(ch.Kind), outcome)
			n.log().Error("alert: channel failed", "project", projectID, "channel", ch.ID, "kind", ch.Kind, "type", payload.Type, "err", err)
			continue
		}
		AlertsTotal.Inc(payload.Type, string(ch.Kind), "sent")
		sent++
		n.log().Info("alert: sent", "project", projectID, "channel", ch.ID, "kind", ch.Kind, "type", payload.Type, "fingerprint", payload.Fingerprint)
	}
	return sent
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
	if p.MoreSinceLast != nil {
		fmt.Fprintf(&b, "\n+%d more new issues since the last alert", *p.MoreSinceLast)
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
		// Not the *url.Error itself: it carries the URL, and the Telegram
		// URL carries the bot token.
		var ue *url.Error
		if errors.As(err, &ue) {
			return fmt.Errorf("%s: %w", ue.Op, ue.Err)
		}
		return errors.New("request failed")
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("%s", resp.Status)
	}
	return nil
}

// ErrBlockedURL: a webhook target this server will not connect to.
var ErrBlockedURL = errors.New("webhook target not allowed")

// ValidateWebhookURL checks a webhook URL as entered: http(s), a host,
// and — for a literal address — not one CheckWebhookAddr refuses. A name
// is checked again at connect time, after resolution.
func ValidateWebhookURL(raw string, allowPrivate bool) error {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return errors.New("webhook url must be http(s)")
	}
	host := u.Hostname()
	if strings.EqualFold(host, "localhost") || strings.HasSuffix(strings.ToLower(host), ".localhost") {
		return fmt.Errorf("%w: %s", ErrBlockedURL, host)
	}
	if ip, err := netip.ParseAddr(strings.Trim(host, "[]")); err == nil {
		return CheckWebhookAddr(ip, allowPrivate)
	}
	return nil
}

// CheckWebhookAddr refuses loopback, link-local (169.254.169.254 is the
// cloud metadata service), unspecified and multicast addresses always,
// and private ranges unless allowPrivate.
func CheckWebhookAddr(ip netip.Addr, allowPrivate bool) error {
	ip = ip.Unmap()
	switch {
	case ip.IsLoopback(), ip.IsLinkLocalUnicast(), ip.IsLinkLocalMulticast(), ip.IsUnspecified(), ip.IsMulticast(), ip.IsInterfaceLocalMulticast():
		return fmt.Errorf("%w: %s", ErrBlockedURL, ip)
	case ip.IsPrivate() && !allowPrivate:
		return fmt.Errorf("%w: %s is a private address (WEBHOOK_ALLOW_PRIVATE=true to allow)", ErrBlockedURL, ip)
	}
	return nil
}

// client is the alert HTTP client: the dialer refuses the addresses
// CheckWebhookAddr does — after DNS resolution, so a name that resolves
// to the metadata service is caught too — and redirects are not followed
// (a public URL redirecting inward would bypass the check).
func (n *Notifier) client() *http.Client {
	if n.HTTP != nil {
		return n.HTTP
	}
	n.once.Do(func() {
		allow := n.Cfg.WebhookAllowPrivate
		dialer := &net.Dialer{Timeout: 10 * time.Second, Control: func(network, address string, c syscall.RawConn) error {
			host, _, err := net.SplitHostPort(address)
			if err != nil {
				return err
			}
			ip, err := netip.ParseAddr(host)
			if err != nil {
				return err
			}
			return CheckWebhookAddr(ip, allow)
		}}
		n.http = &http.Client{
			Timeout:   15 * time.Second,
			Transport: &http.Transport{DialContext: dialer.DialContext, TLSHandshakeTimeout: 10 * time.Second, Proxy: http.ProxyFromEnvironment},
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return fmt.Errorf("%w: redirects are not followed", ErrBlockedURL)
			},
		}
	})
	return n.http
}

func (n *Notifier) log() *slog.Logger {
	if n.Log != nil {
		return n.Log
	}
	return slog.Default()
}
