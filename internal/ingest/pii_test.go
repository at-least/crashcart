package ingest

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/crashcartapp/crashcart/internal/sentry"
)

func TestRedactText(t *testing.T) {
	cases := map[string]string{
		"contact john@example.com now":           "contact [REDACTED] now",
		"call +1 555-123-4567 or (555) 123-4567": "call [REDACTED] or [REDACTED]",
		"card 4111-1111-1111-1111 declined":      "card [REDACTED] declined",
		"order 1756462530123 failed code 500123": "order 1756462530123 failed code 500123",
		"id 12345-678-9012":                      "id 12345-678-9012", // preceded by digit run → not a phone
	}
	for in, want := range cases {
		if got := RedactText(in); got != want {
			t.Errorf("%q → %q, want %q", in, got, want)
		}
	}
}

func TestRedactUserID(t *testing.T) {
	if got := RedactUserID("user-0000123"); got != "user****0123" {
		t.Errorf("got %q", got)
	}
	if got := RedactUserID("short"); got != "****" {
		t.Errorf("got %q", got)
	}
	if got := RedactUserID(""); got != "" {
		t.Errorf("got %q", got)
	}
}

func TestRedactTags(t *testing.T) {
	out := RedactTags(map[string]string{"email": "a@b.co", "note": "mail me at a@b.co", "build": "42"})
	if out["email"] != "[REDACTED]" || out["note"] != "mail me at [REDACTED]" || out["build"] != "42" {
		t.Errorf("got %v", out)
	}
}

func TestRedactEventFields(t *testing.T) {
	ev := &sentry.Event{
		Message: "hi alice@example.com", Screen: "/users/bob@example.com/cart",
		Exceptions: []sentry.Exception{{Type: "Error", Value: "charge failed for carol@example.com"}},
		Raw:        []byte(`{"user":{"id":"u1","ip_address":"203.0.113.9","email":"dave@example.com"},"message":"x"}`),
	}
	redact(ev)
	for name, got := range map[string]string{"message": ev.Message, "screen": ev.Screen, "exception": ev.Exceptions[0].Value, "raw": string(ev.Raw)} {
		if strings.Contains(got, "@example.com") {
			t.Errorf("%s not redacted: %s", name, got)
		}
	}
	if strings.Contains(string(ev.Raw), "203.0.113.9") || !strings.Contains(string(ev.Raw), `"ip_address":"[redacted]"`) {
		t.Errorf("ip_address not redacted: %s", ev.Raw)
	}
	var js map[string]any
	if err := json.Unmarshal(ev.Raw, &js); err != nil {
		t.Errorf("redacted raw is not JSON: %v", err)
	}
}
