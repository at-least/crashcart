// Package alerts delivers notifications (webhook, Telegram) for new issues,
// regressions, unhandled-error spikes and escalating issues, and runs the
// ignored-issue check (time / count expiry, escalation).
package alerts

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
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

	"github.com/at-least/crashcart/internal/config"
	"github.com/at-least/crashcart/internal/db/sqlc"
	"github.com/at-least/crashcart/internal/sentry"
	"github.com/at-least/crashcart/internal/store"
)

// Alert types (alert_rules.type).
const (
	TypeNewIssue       = "new_issue"
	TypeRegression     = "regression"
	TypeUnhandledSpike = "unhandled_spike"
	TypeEscalating     = "escalating" // an issue ignored until escalating came back
)

// Spike thresholds: at least MinSpikeUnhandled in the last hour and
// SpikeFactor × the hourly mean of the 24 h before. The same rule, applied
// to one issue's stored events against the 24 h before it was ignored,
// is what makes an ignored-until-escalating issue escalate.
const (
	MinSpikeUnhandled = 10
	SpikeFactor       = 3
	defaultCooldown   = 60
)

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
	Recent        *int64  `json:"recent,omitempty"`          // unhandled_spike: unhandled in the last hour; escalating: the issue's events in the last hour
	Baseline      *int64  `json:"baseline,omitempty"`        // unhandled_spike: unhandled in the 24 h before; escalating: the issue's events in the 24 h before it was ignored
	URL           string  `json:"url"`
}

// EnsureRules creates the project's four default rules (all enabled,
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
	return n.deliver(ctx, projectID, sqlc.AlertType(typ), func(previous *time.Time) Payload {
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
		return payload
	})
}

// CheckSpikes evaluates the unhandled_spike rule of every project (scheduler).
func (n *Notifier) CheckSpikes(ctx context.Context) error {
	// "recent" is the exact last hour from the raw table; the baseline is
	// the 24 full hourly buckets before the bucket the recent hour starts
	// in (that partial bucket would count its unhandled on both sides).
	// One query covers every project.
	now := time.Now().UTC()
	recentFrom := now.Add(-time.Hour)
	baselineTo := recentFrom.Truncate(time.Hour)
	rows, err := n.Store.UnhandledSpikeInputs(ctx, sqlc.UnhandledSpikeInputsParams{
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

// spike claims the unhandled_spike cooldown for the project and notifies.
func (n *Notifier) spike(ctx context.Context, projectID, recent, baseline int64) error {
	if err := EnsureRules(ctx, n.Store, projectID); err != nil {
		return err
	}
	p, err := n.Store.GetProjectByID(ctx, projectID)
	if err != nil {
		return err
	}
	return n.deliver(ctx, projectID, TypeUnhandledSpike, func(*time.Time) Payload {
		return Payload{
			Type: TypeUnhandledSpike, Project: p.Name, ProjectSlug: p.Slug,
			Title:  fmt.Sprintf("Unhandled error spike: %d unhandled errors in the last hour (baseline %.1f/h)", recent, float64(baseline)/24),
			Level:  "fatal",
			Recent: &recent, Baseline: &baseline,
			URL: n.link(p.Slug, "?handled=false&range=1h"),
		}
	})
}

// IsSpike is the unhandled-spike rule: unhandled errors in the last hour versus
// the hourly mean of the 24 h before it. Also the escalation rule of an
// ignored issue (its own events, its own baseline).
func IsSpike(recent, baseline int64) bool {
	mean := float64(baseline) / 24
	return recent >= MinSpikeUnhandled && float64(recent) >= SpikeFactor*mean
}

// CheckIgnored (scheduler, every minute) puts ignored issues whose
// condition is met back to unresolved. Time and count are decided from
// the row alone; escalation is IsSpike on the issue's stored events in
// the exact last hour against the 24 h baseline recorded when it was
// ignored, and sends an `escalating` alert per issue — under the
// project's cooldown for that type, like the other alerts, so a bad hour
// yields one message.
func (n *Notifier) CheckIgnored(ctx context.Context) error {
	due, err := n.Store.UnignoreDue(ctx)
	if err != nil {
		return fmt.Errorf("unignore: %w", err)
	}
	for _, d := range due {
		n.log().Info("issue unignored", "project", d.ProjectID, "fingerprint", d.Fingerprint, "reason", d.Reason)
	}
	rows, err := n.Store.EscalationInputs(ctx, time.Now().UTC().Add(-time.Hour))
	if err != nil {
		return fmt.Errorf("escalation inputs: %w", err)
	}
	var errs []error
	for _, in := range rows {
		if !IsSpike(in.Recent, in.Baseline) {
			continue
		}
		issue, err := n.Store.EscalateIssue(ctx, sqlc.EscalateIssueParams{ProjectID: in.ProjectID, Fingerprint: in.Fingerprint})
		if errors.Is(err, pgx.ErrNoRows) {
			continue // its status changed meanwhile
		}
		if err != nil {
			errs = append(errs, err)
			continue
		}
		n.log().Info("issue escalating", "project", issue.ProjectID, "fingerprint", issue.Fingerprint, "recent", in.Recent, "baseline", in.Baseline)
		if err := n.escalate(ctx, issue, in.Recent, in.Baseline); err != nil {
			errs = append(errs, fmt.Errorf("issue %s: %w", issue.Fingerprint, err))
		}
	}
	return errors.Join(errs...)
}

// escalate claims the escalating cooldown for the project and notifies.
func (n *Notifier) escalate(ctx context.Context, issue sqlc.Issue, recent, baseline int64) error {
	if err := EnsureRules(ctx, n.Store, issue.ProjectID); err != nil {
		return err
	}
	p, err := n.Store.GetProjectByID(ctx, issue.ProjectID)
	if err != nil {
		return err
	}
	return n.deliver(ctx, issue.ProjectID, TypeEscalating, func(*time.Time) Payload {
		return Payload{
			Type: TypeEscalating, Project: p.Name, ProjectSlug: p.Slug, Title: issue.Title, Fingerprint: string(issue.Fingerprint),
			Level: string(issue.Level), EventCount: issue.EventCount, FirstRelease: issue.FirstRelease, LastRelease: issue.LastRelease,
			Recent: &recent, Baseline: &baseline,
			URL: n.link(p.Slug, "/issues/"+url.PathEscape(string(issue.Fingerprint))),
		}
	})
}

// link builds the viewer URL for a project path; relative when PUBLIC_URL
// is not configured.
func (n *Notifier) link(slug, path string) string {
	return n.Cfg.PublicURL + "/p/" + url.PathEscape(slug) + path
}

// deliver claims the rule's cooldown (atomically: replicas cannot both
// send), builds the payload — previous is last_triggered before the
// claim, for "N more since the last alert" — and sends it; when no
// channel took it, the claim is given back so one outage does not also
// eat the next alert of the hour.
func (n *Notifier) deliver(ctx context.Context, projectID int64, typ sqlc.AlertType, build func(previous *time.Time) Payload) error {
	previous, err := n.Store.ClaimAlertRule(ctx, sqlc.ClaimAlertRuleParams{ProjectID: projectID, Type: typ})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil // disabled, cooling down, or no rule row
	}
	if err != nil {
		return err
	}
	if n.notify(ctx, projectID, build(previous)) == 0 {
		return n.Store.UnclaimAlertRule(ctx, sqlc.UnclaimAlertRuleParams{ProjectID: projectID, Type: typ, Previous: previous})
	}
	return nil
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
			n.log().Error("alert: channel failed", "project", projectID, "channel", ch.ID, "kind", ch.Kind, "type", payload.Type, "err", err)
			continue
		}
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
	case TypeUnhandledSpike:
		b.WriteString("Unhandled error spike")
	case TypeEscalating:
		b.WriteString("Escalating")
	default:
		b.WriteString(p.Type)
	}
	fmt.Fprintf(&b, " in %s\n%s", p.Project, p.Title)
	if p.Type != TypeUnhandledSpike {
		fmt.Fprintf(&b, "\n%s · %d events", p.Level, p.EventCount)
		if p.LastRelease != nil && *p.LastRelease != "" {
			fmt.Fprintf(&b, " · release %s", *p.LastRelease)
		}
	}
	if p.Type == TypeEscalating && p.Recent != nil && p.Baseline != nil {
		fmt.Fprintf(&b, "\n%d in the last hour (was %.1f/h when ignored)", *p.Recent, float64(*p.Baseline)/24)
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

// ChannelConfig validates a channel as entered (kind, and its one
// setting read through get) and returns the config to store. The error
// is the user-facing message.
func ChannelConfig(kind string, get func(string) string, allowPrivate bool) (json.RawMessage, error) {
	switch kind {
	case "webhook":
		u := strings.TrimSpace(get("url"))
		if err := ValidateWebhookURL(u, allowPrivate); err != nil {
			return nil, err
		}
		return json.Marshal(map[string]string{"url": u})
	case "telegram":
		id := strings.TrimSpace(get("chat_id"))
		if id == "" {
			return nil, errors.New("telegram needs a chat_id")
		}
		return json.Marshal(map[string]string{"chat_id": id})
	}
	return nil, errors.New("kind must be webhook or telegram")
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
			Timeout: 15 * time.Second,
			// No proxy: through one the dialer would check the proxy's
			// address, and the proxy would fetch the inward URL for us.
			Transport: &http.Transport{DialContext: dialer.DialContext, TLSHandshakeTimeout: 10 * time.Second, Proxy: nil},
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
