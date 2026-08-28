package alerts

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/smtp"
	"strings"
	"time"

	"github.com/newlix/crashcart/internal/config"
)

// MultiChannel sends to Telegram, webhooks (Slack/Discord-compatible) and
// SMTP email — every configured destination, failures logged per target.
type MultiChannel struct {
	cfg  config.Config
	log  *slog.Logger
	http *http.Client
}

// NewMultiChannel builds the notifier from config.
func NewMultiChannel(cfg config.Config, log *slog.Logger) *MultiChannel {
	return &MultiChannel{cfg: cfg, log: log, http: &http.Client{Timeout: 10 * time.Second}}
}

// Send delivers message to all channels.
func (m *MultiChannel) Send(ctx context.Context, message string) {
	if m.cfg.TelegramBotToken != "" {
		for _, chat := range m.cfg.TelegramChatIDs {
			url := "https://api.telegram.org/bot" + m.cfg.TelegramBotToken + "/sendMessage"
			if err := m.postJSON(ctx, url, map[string]string{"chat_id": chat, "text": message}); err != nil {
				m.log.Error("telegram alert failed", "chat", chat, "err", err)
			}
		}
	}
	for _, url := range m.cfg.AlertWebhooks {
		if err := m.postJSON(ctx, url, map[string]string{"text": message, "content": message, "username": "CrashCart Alerts"}); err != nil {
			m.log.Error("webhook alert failed", "url", url, "err", err)
		}
	}
	if m.cfg.EmailFrom != "" && m.cfg.SMTPAddr != "" {
		for _, to := range m.cfg.AlertEmails {
			if err := m.sendMail(to, "CrashCart Alert", message); err != nil {
				m.log.Error("email alert failed", "to", to, "err", err)
			}
		}
	}
}

func (m *MultiChannel) postJSON(ctx context.Context, url string, body any) error {
	b, _ := json.Marshal(body)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(b))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := m.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		return fmt.Errorf("status %s", resp.Status)
	}
	return nil
}

// sendMail speaks SMTP with STARTTLS (or implicit TLS on port 465).
func (m *MultiChannel) sendMail(to, subject, body string) error {
	host, port, err := net.SplitHostPort(m.cfg.SMTPAddr)
	if err != nil {
		return fmt.Errorf("SMTP_ADDR must be host:port: %w", err)
	}
	msg := strings.Join([]string{
		"From: " + m.cfg.EmailFrom,
		"To: " + to,
		"Subject: " + subject,
		"MIME-Version: 1.0",
		"Content-Type: text/plain; charset=utf-8",
		"",
		body,
	}, "\r\n")

	var client *smtp.Client
	if port == "465" {
		conn, err := tls.Dial("tcp", m.cfg.SMTPAddr, &tls.Config{ServerName: host})
		if err != nil {
			return err
		}
		if client, err = smtp.NewClient(conn, host); err != nil {
			return err
		}
	} else {
		if client, err = smtp.Dial(m.cfg.SMTPAddr); err != nil {
			return err
		}
		if ok, _ := client.Extension("STARTTLS"); ok {
			if err := client.StartTLS(&tls.Config{ServerName: host}); err != nil {
				return err
			}
		}
	}
	defer client.Close()
	if m.cfg.SMTPUser != "" {
		if err := client.Auth(smtp.PlainAuth("", m.cfg.SMTPUser, m.cfg.SMTPPassword, host)); err != nil {
			return err
		}
	}
	if err := client.Mail(m.cfg.EmailFrom); err != nil {
		return err
	}
	if err := client.Rcpt(to); err != nil {
		return err
	}
	wc, err := client.Data()
	if err != nil {
		return err
	}
	if _, err := wc.Write([]byte(msg)); err != nil {
		return err
	}
	if err := wc.Close(); err != nil {
		return err
	}
	return client.Quit()
}
