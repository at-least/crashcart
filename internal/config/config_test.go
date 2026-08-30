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
