package config

import "testing"

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
		t.Errorf("RATE_LIMIT default = %d, want 6000 (100/s: a burst of cached crashes must fit)", c.RateLimit)
	}
	if c.TrustProxy || c.WebhookAllowPrivate {
		t.Error("TRUST_PROXY and WEBHOOK_ALLOW_PRIVATE default off")
	}
}
