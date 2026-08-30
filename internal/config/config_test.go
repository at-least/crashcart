package config

import (
	"strings"
	"testing"
	"time"
)

func TestLoadRejectsZeroAlertInterval(t *testing.T) {
	t.Setenv("ALERT_INTERVAL", "0")
	if _, err := Load(); err == nil {
		t.Fatal("ALERT_INTERVAL=0 must be rejected (time.NewTicker would panic)")
	}
	t.Setenv("ALERT_INTERVAL", "5m")
	if c, err := Load(); err != nil || c.AlertInterval.Minutes() != 5 {
		t.Fatalf("ALERT_INTERVAL=5m: %v %v", c.AlertInterval, err)
	}
}

func TestDefaults(t *testing.T) {
	c, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if c.RateLimit != 6000 {
		t.Errorf("RATE_LIMIT default = %d, want 6000 (100/s: a burst of cached unhandled must fit)", c.RateLimit)
	}
	if c.TrustProxy || c.WebhookAllowPrivate {
		t.Error("TRUST_PROXY and WEBHOOK_ALLOW_PRIVATE default off")
	}
}

func TestLoadRejectsBadValues(t *testing.T) {
	for _, c := range []struct{ k, v string }{
		{"RATE_LIMIT", "abc"}, {"RETENTION_DAYS", "0"}, {"SYMBOLICATE_CACHE_MAX_MB", "0"}, {"WORKERS", "1.5"}, {"ALERT_INTERVAL", "soon"},
	} {
		t.Run(c.k, func(t *testing.T) {
			t.Setenv(c.k, c.v)
			if _, err := Load(); err == nil || !strings.Contains(err.Error(), c.k) {
				t.Fatalf("%s=%q: err = %v, want one naming the variable", c.k, c.v, err)
			}
		})
	}
	// Whitespace around a value is not a parse error; a blank value is the default.
	t.Setenv("WORKERS", " 3 ")
	t.Setenv("RATE_LIMIT", "   ")
	t.Setenv("PUBLIC_URL", "https://crash.example.com/")
	t.Setenv("CUSTOM_TAGS", " build, , flavor ")
	c, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if c.Workers != 3 || c.RateLimit != 6000 || c.PublicURL != "https://crash.example.com" {
		t.Errorf("workers=%d rate=%d public=%q", c.Workers, c.RateLimit, c.PublicURL)
	}
	if len(c.CustomTags) != 2 || c.CustomTags[0] != "build" || c.CustomTags[1] != "flavor" {
		t.Errorf("custom tags = %q", c.CustomTags)
	}
}

func TestRetention(t *testing.T) {
	if d := (Config{}).Retention(); d != 30*24*time.Hour {
		t.Errorf("zero config = %v, want the 30-day default", d)
	}
	if d := (Config{RetentionDays: 7}).Retention(); d != 7*24*time.Hour {
		t.Errorf("7 days = %v", d)
	}
	t.Setenv("RETENTION_DAYS", "45")
	c, err := Load()
	if err != nil || c.Retention() != 45*24*time.Hour {
		t.Errorf("RETENTION_DAYS=45: %v %v", c.Retention(), err)
	}
}
