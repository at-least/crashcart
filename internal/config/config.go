// Package config reads the process configuration from the environment.
package config

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

// Config is everything the binary needs; loaded once in main.
type Config struct {
	Addr        string
	DatabaseURL string
	PublicURL   string // externally visible base URL (DSN display); "" = derive from request

	CORSOrigin    string // SDK ingest endpoints (browser SDKs); "*" by default
	APICORSOrigin string // /api/* JSON API; "" = no CORS headers (same-origin / non-browser callers)
	RateLimit     int    // requests / minute / credential; 0 = off

	RetentionDays    int
	AlertInterval    time.Duration
	Workers          int
	SymbolicateURL   string // dSYM sidecar
	TelegramBotToken string
	PIIRedact        bool
	CustomTags       []string // tag keys the viewer shows as filters
}

// Load reads the environment. It fails only on unparseable values; missing
// values fall back to defaults and DATABASE_URL is validated by the caller.
func Load() (Config, error) {
	c := Config{
		Addr:             get("LISTEN_ADDR", ":8080"),
		DatabaseURL:      get("DATABASE_URL", ""),
		PublicURL:        strings.TrimSuffix(get("PUBLIC_URL", ""), "/"),
		CORSOrigin:       get("CORS_ORIGIN", "*"),
		APICORSOrigin:    get("API_CORS_ORIGIN", ""),
		SymbolicateURL:   strings.TrimSuffix(get("SYMBOLICATE_URL", ""), "/"),
		TelegramBotToken: get("TELEGRAM_BOT_TOKEN", ""),
		PIIRedact:        get("PII_REDACT", "false") == "true",
		CustomTags:       SplitCSV(get("CUSTOM_TAGS", "")),
	}
	var err error
	if c.RateLimit, err = intEnv("RATE_LIMIT", 600); err != nil {
		return c, err
	}
	if c.RetentionDays, err = intEnv("RETENTION_DAYS", 30); err != nil {
		return c, err
	}
	if c.Workers, err = intEnv("WORKERS", 4); err != nil {
		return c, err
	}
	if c.AlertInterval, err = durEnv("ALERT_INTERVAL", 10*time.Minute); err != nil {
		return c, err
	}
	if c.RetentionDays < 1 {
		return c, fmt.Errorf("RETENTION_DAYS must be >= 1")
	}
	return c, nil
}

func get(k, def string) string {
	if v, ok := os.LookupEnv(k); ok && strings.TrimSpace(v) != "" {
		return strings.TrimSpace(v)
	}
	return def
}

func intEnv(k string, def int) (int, error) {
	v := get(k, "")
	if v == "" {
		return def, nil
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return 0, fmt.Errorf("%s: %w", k, err)
	}
	return n, nil
}

func durEnv(k string, def time.Duration) (time.Duration, error) {
	v := get(k, "")
	if v == "" {
		return def, nil
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		return 0, fmt.Errorf("%s: %w", k, err)
	}
	return d, nil
}

// SplitCSV splits a comma-separated list, trimming blanks.
func SplitCSV(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}
