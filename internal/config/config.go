// Package config reads CrashCart's configuration from environment variables.
package config

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

// CustomTag adds a searchable column to the viewer for a Sentry tag key.
type CustomTag struct {
	Key   string
	Label string
}

// Config is the fully parsed process configuration.
type Config struct {
	Addr        string // LISTEN_ADDR (default ":8080")
	DatabaseURL string // DATABASE_URL (required)

	APIKeys     []string // API_KEYS: bearer keys for /api/* (empty = open)
	IngestToken string   // INGEST_TOKEN: token for /ingest (empty = open)
	RateLimit   int      // RATE_LIMIT: requests / minute / key (0 disables)
	CORSOrigin  string   // CORS_ORIGIN (default "*")

	PIIRedact  bool    // PII_REDACT
	SampleRate float64 // SAMPLE_RATE: info/debug keep ratio in [0,1]

	RetentionDays     int           // RETENTION_DAYS (default 30)
	RetentionInterval time.Duration // RETENTION_INTERVAL (default 1h)
	AlertInterval     time.Duration // ALERT_INTERVAL (default 10m)

	TelegramBotToken string
	TelegramChatIDs  []string
	AlertWebhooks    []string
	AlertEmails      []string
	EmailFrom        string
	SMTPAddr         string // SMTP_ADDR host:port (STARTTLS)
	SMTPUser         string
	SMTPPassword     string

	CustomTags     []CustomTag
	Deployments    string // DEPLOYMENTS raw ("iOS|https://…|key,…")
	SymbolicateURL string // SYMBOLICATE_URL: dSYM container base URL
}

// FromEnv builds a Config from the process environment.
func FromEnv() (Config, error) {
	get := func(k, def string) string {
		if v, ok := os.LookupEnv(k); ok && strings.TrimSpace(v) != "" {
			return strings.TrimSpace(v)
		}
		return def
	}
	c := Config{
		Addr:             get("LISTEN_ADDR", ":8080"),
		DatabaseURL:      get("DATABASE_URL", ""),
		APIKeys:          SplitCSV(get("API_KEYS", "")),
		IngestToken:      get("INGEST_TOKEN", ""),
		CORSOrigin:       get("CORS_ORIGIN", "*"),
		PIIRedact:        get("PII_REDACT", "false") == "true",
		SampleRate:       ParseSampleRate(get("SAMPLE_RATE", "1.0")),
		TelegramBotToken: get("TELEGRAM_BOT_TOKEN", ""),
		TelegramChatIDs:  SplitCSV(get("TELEGRAM_CHAT_IDS", "")),
		AlertWebhooks:    SplitCSV(get("ALERT_WEBHOOKS", "")),
		AlertEmails:      SplitCSV(get("ALERT_EMAILS", "")),
		EmailFrom:        get("EMAIL_FROM", ""),
		SMTPAddr:         get("SMTP_ADDR", ""),
		SMTPUser:         get("SMTP_USER", ""),
		SMTPPassword:     get("SMTP_PASSWORD", ""),
		CustomTags:       ParseCustomTags(get("CUSTOM_TAGS", "")),
		Deployments:      get("DEPLOYMENTS", ""),
		SymbolicateURL:   strings.TrimSuffix(get("SYMBOLICATE_URL", ""), "/"),
	}
	if c.DatabaseURL == "" {
		return c, fmt.Errorf("DATABASE_URL is required")
	}
	var err error
	if c.RateLimit, err = intEnv("RATE_LIMIT", 100); err != nil {
		return c, err
	}
	if c.RetentionDays, err = intEnv("RETENTION_DAYS", 30); err != nil {
		return c, err
	}
	if c.RetentionDays < 1 {
		return c, fmt.Errorf("RETENTION_DAYS must be >= 1")
	}
	if c.RetentionInterval, err = durEnv("RETENTION_INTERVAL", time.Hour); err != nil {
		return c, err
	}
	if c.AlertInterval, err = durEnv("ALERT_INTERVAL", 10*time.Minute); err != nil {
		return c, err
	}
	return c, nil
}

func intEnv(key string, def int) (int, error) {
	v := strings.TrimSpace(os.Getenv(key))
	if v == "" {
		return def, nil
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return 0, fmt.Errorf("%s: %w", key, err)
	}
	return n, nil
}

func durEnv(key string, def time.Duration) (time.Duration, error) {
	v := strings.TrimSpace(os.Getenv(key))
	if v == "" {
		return def, nil
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		return 0, fmt.Errorf("%s: %w", key, err)
	}
	if d < time.Second {
		return 0, fmt.Errorf("%s: must be >= 1s", key)
	}
	return d, nil
}

// SplitCSV splits a comma-separated value into trimmed, non-empty parts.
func SplitCSV(s string) []string {
	if strings.TrimSpace(s) == "" {
		return nil
	}
	var out []string
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// ParseSampleRate clamps a SAMPLE_RATE string into [0, 1]; unparseable → 1.
func ParseSampleRate(s string) float64 {
	f, err := strconv.ParseFloat(strings.TrimSpace(s), 64)
	if err != nil {
		return 1
	}
	return min(max(f, 0), 1)
}

// ParseCustomTags parses "device_id:Device,build:Build Number".
func ParseCustomTags(s string) []CustomTag {
	var out []CustomTag
	for _, part := range strings.Split(s, ",") {
		key, label, found := strings.Cut(part, ":")
		key = strings.TrimSpace(key)
		if key == "" {
			continue
		}
		if !found || strings.TrimSpace(label) == "" {
			label = key
		}
		out = append(out, CustomTag{Key: key, Label: strings.TrimSpace(label)})
	}
	return out
}
