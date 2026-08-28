package sentry

import (
	"strconv"
	"strings"
	"testing"
	"time"
)

var now = time.Date(2026, 8, 29, 12, 0, 0, 0, time.UTC)

const crashEvent = `{"event_id":"a1b2c3d4e5f6a1b2c3d4e5f6a1b2c3d4","timestamp":"2026-08-29T10:15:30Z","level":"fatal","platform":"android","environment":"production","transaction":"CartFragment","tags":{"device_id":"did-1","build":"42"},"user":{"id":"user-001"},"sdk":{"name":"sentry.java.android"},"contexts":{"device":{"model":"Pixel 8"},"os":{"version":"14"},"app":{"app_version":"2.4.1"}},"exception":{"values":[{"type":"NullPointerException","value":"Attempt to invoke virtual method","mechanism":{"handled":false},"stacktrace":{"frames":[{"filename":"Looper.java","function":"loop","in_app":false,"lineno":10},{"filename":"com/example/CartFragment.java","function":"onCreateView","in_app":true,"lineno":142},{"filename":"Native.java","function":"call","in_app":false,"lineno":1}]}}]},"breadcrumbs":{"values":[{"timestamp":1787998500,"category":"navigation","message":"cart","level":"info","data":{"to":"/cart"}},{"timestamp":"2026-08-29T10:15:29Z","category":"http","message":"","data":{"method":"GET","url":"/api/cart","status_code":500}}]}}`

func envelope(items ...string) []byte {
	return []byte("{\"event_id\":\"h1\",\"sent_at\":\"2026-08-29T11:00:00Z\"}\n" + strings.Join(items, "\n") + "\n")
}

func TestParseCrashEvent(t *testing.T) {
	env := Parse(envelope(`{"type":"event"}`, crashEvent), now)
	if len(env.Events) != 1 {
		t.Fatalf("events = %d", len(env.Events))
	}
	e := env.Events[0]
	if e.EventID != "a1b2c3d4e5f6a1b2c3d4e5f6a1b2c3d4" || e.Level != "fatal" || e.Platform != "android" {
		t.Errorf("basic fields wrong: %+v", e)
	}
	if !e.Timestamp.Equal(time.Date(2026, 8, 29, 10, 15, 30, 0, time.UTC)) {
		t.Errorf("timestamp = %v", e.Timestamp)
	}
	if e.Message != "NullPointerException: Attempt to invoke virtual method" {
		t.Errorf("message = %q", e.Message)
	}
	if e.Release != "2.4.1" || e.DeviceModel != "Pixel 8" || e.OSVersion != "14" || e.Screen != "CartFragment" {
		t.Errorf("context fields wrong: %+v", e)
	}
	if e.ErrorType != "NullPointerException" || e.Handled == nil || *e.Handled || !e.IsCrash() {
		t.Errorf("exception fields wrong")
	}
	if e.UserID != "user-001" || e.DeviceID() != "did-1" || e.Tags["build"] != "42" || e.SDKName != "sentry.java.android" {
		t.Errorf("user/tags wrong: %+v", e)
	}
	if len(e.Breadcrumbs) != 2 || e.Breadcrumbs[0].Timestamp != "2026-08-29T10:15:00Z" || e.Breadcrumbs[1].Category != "http" {
		t.Errorf("breadcrumbs = %+v", e.Breadcrumbs)
	}
	if string(e.Raw) != crashEvent {
		t.Error("raw payload altered")
	}
	a := e.Analyze()
	if a.ErrorLocation != "CartFragment.java:142" || a.AppFrameCount != 1 || a.TotalFrames != 3 {
		t.Errorf("analysis = %+v", a)
	}
	if len(a.UserJourney) != 3 || a.UserJourney[0] != "→ /cart" || !strings.HasPrefix(a.UserJourney[1], "🌐 GET /api/cart → 500") || !strings.HasPrefix(a.UserJourney[2], "💥 NullPointerException at CartFragment.java:142") {
		t.Errorf("journey = %q", a.UserJourney)
	}
	if e.IssueTitle() != "NullPointerException in CartFragment" {
		t.Errorf("title = %q", e.IssueTitle())
	}
	fp := e.Fingerprint()
	if len(fp) != 32 {
		t.Errorf("fingerprint = %q", fp)
	}
	// Line numbers don't affect grouping.
	other := strings.Replace(crashEvent, `"lineno":142`, `"lineno":999`, 1)
	env2 := Parse(envelope(`{"type":"event"}`, other), now)
	if env2.Events[0].Fingerprint() != fp {
		t.Error("fingerprint should ignore line numbers")
	}
}

func TestParseFallbacks(t *testing.T) {
	env := Parse(envelope(`{"type":"event"}`, `{"message":"hello","timestamp":1787998530.5,"release":"1.0","server_name":"web-1","contexts":{"runtime":{"name":"node","version":"22"}},"tags":[["k","v"],["n",1]]}`), now)
	e := env.Events[0]
	if e.Level != "error" || e.Message != "hello" || e.Release != "1.0" || e.DeviceModel != "web-1" || e.OSVersion != "node/22" {
		t.Errorf("fallbacks wrong: %+v", e)
	}
	if e.Tags["k"] != "v" || e.Tags["n"] != "1" {
		t.Errorf("array tags = %v", e.Tags)
	}
	if e.Timestamp.Unix() != 1787998530 {
		t.Errorf("unix ts = %v", e.Timestamp)
	}
	if e.EventID != "h1" {
		t.Errorf("header event id fallback = %q", e.EventID)
	}
	if e.Fingerprint() != "" {
		t.Error("no exception → no fingerprint")
	}
}

func TestParseTimestampVariants(t *testing.T) {
	cases := map[string]string{
		`"2026-08-29T10:15:30+02:00"`: "2026-08-29T08:15:30Z",
		`1787998530000`:               "2026-08-29T10:15:30Z",
		`"garbage"`:                   "2026-08-29T11:00:00Z", // sent_at fallback
		`null`:                        "2026-08-29T11:00:00Z",
	}
	for in, want := range cases {
		env := Parse(envelope(`{"type":"event"}`, `{"message":"x","timestamp":`+in+`}`), now)
		if got := env.Events[0].Timestamp.Format(time.RFC3339); got != want {
			t.Errorf("%s: got %s want %s", in, got, want)
		}
	}
}

func TestParseSessionsAndSkips(t *testing.T) {
	body := envelope(
		`{"type":"attachment","length":5}`, `hello`,
		`{"type":"session"}`, `{"sid":"s1","status":"crashed","attrs":{"release":"2.0"},"started":"2026-08-28T01:00:00Z"}`,
		`{"type":"sessions"}`, `{"items":[{"status":"exited","release":"2.0"},{"status":"errored","release":"2.0"}]}`,
		`not json at all`,
		`{"type":"event"}`, `{"message":"after resync"}`,
	)
	env := Parse(body, now)
	if len(env.Sessions) != 3 || env.Sessions[0].Status != "crashed" || env.Sessions[0].Release != "2.0" {
		t.Errorf("sessions = %+v", env.Sessions)
	}
	if !env.Sessions[1].StartedAt.Equal(now) {
		t.Error("missing started → now")
	}
	if len(env.Events) != 1 || env.Events[0].Message != "after resync" {
		t.Errorf("events = %+v", env.Events)
	}
}

func TestParseLengthWithNewlines(t *testing.T) {
	item := "{\"message\":\"multi\\nline\",\n\"level\":\"info\"}"
	body := envelope(`{"type":"event","length":`+itoa(len(item))+`}`, item, `{"type":"event"}`, `{"message":"second"}`)
	env := Parse(body, now)
	if len(env.Events) != 2 || env.Events[0].Level != "info" || env.Events[1].Message != "second" {
		t.Errorf("events = %+v", env.Events)
	}
}

func TestSDKFingerprint(t *testing.T) {
	env := Parse(envelope(`{"type":"event"}`, `{"fingerprint":["checkout","timeout"],"exception":{"values":[{"type":"TimeoutError"}]}}`), now)
	a := env.Events[0].Fingerprint()
	env2 := Parse(envelope(`{"type":"event"}`, `{"fingerprint":["checkout","timeout"],"exception":{"values":[{"type":"OtherError"}]}}`), now)
	if a == "" || a != env2.Events[0].Fingerprint() {
		t.Error("SDK fingerprint should win over the stack signature")
	}
}

func TestLevelNormalization(t *testing.T) {
	for in, want := range map[string]string{"warn": "warning", "CRITICAL": "fatal", "": "error", "Info": "info"} {
		if got := normalizeLevel(in); got != want {
			t.Errorf("%q → %q, want %q", in, got, want)
		}
	}
}

func itoa(n int) string { return strconv.Itoa(n) }
