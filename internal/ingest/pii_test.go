package ingest

import "testing"

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
