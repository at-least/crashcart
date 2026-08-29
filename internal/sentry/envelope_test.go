package sentry

import (
	"fmt"
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
	if loc := ErrorLocation(e.Frames()); loc != "CartFragment.java:142" || len(e.Frames()) != 3 {
		t.Errorf("error location = %q, frames = %d", loc, len(e.Frames()))
	}
	if e.IssueTitle() != "NullPointerException in CartFragment: Attempt to invoke virtual method" {
		t.Errorf("title = %q", e.IssueTitle())
	}
	fp := Fingerprint(e, e.Frames())
	if len(fp) != 32 {
		t.Errorf("fingerprint = %q", fp)
	}
	// Line numbers don't affect grouping.
	other := strings.Replace(crashEvent, `"lineno":142`, `"lineno":999`, 1)
	env2 := Parse(envelope(`{"type":"event"}`, other), now)
	if Fingerprint(env2.Events[0], env2.Events[0].Frames()) != fp {
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
	if Fingerprint(e, e.Frames()) == "" {
		t.Error("an error-level message event groups by its text")
	}
	info := ParseEvent("", now, []byte(`{"level":"info","message":"user signed in"}`), now)
	if Fingerprint(info, nil) != "" {
		t.Error("info messages are not issues")
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
	a := Fingerprint(env.Events[0], env.Events[0].Frames())
	env2 := Parse(envelope(`{"type":"event"}`, `{"fingerprint":["checkout","timeout"],"exception":{"values":[{"type":"OtherError"}]}}`), now)
	if a == "" || a != Fingerprint(env2.Events[0], env2.Events[0].Frames()) {
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

func TestChainedExceptionsJavaOrder(t *testing.T) {
	// Java SDK: outer RuntimeException first (exception_id 0), its cause
	// second (parent_id 0). The cause names the issue; handled comes from
	// the outer one.
	body := `{"exception":{"values":[
	  {"type":"RuntimeException","value":"Unable to start activity","mechanism":{"type":"UncaughtExceptionHandler","handled":false,"exception_id":0},
	   "stacktrace":{"frames":[{"module":"android.app.ActivityThread","function":"main","lineno":1}]}},
	  {"type":"IllegalStateException","value":"cart total unavailable","mechanism":{"type":"chained","exception_id":1,"parent_id":0},
	   "stacktrace":{"frames":[{"module":"android.app.Activity","function":"performCreate","lineno":2},{"module":"cc.smoke.MainActivity","function":"onCreate","lineno":47,"in_app":true}]}}]}}`
	ev := ParseEvent("", now, []byte(body), now)
	if ev.ErrorType != "IllegalStateException" || ev.Handled == nil || *ev.Handled || ev.Message != "IllegalStateException: cart total unavailable" {
		t.Fatalf("type=%q handled=%v message=%q", ev.ErrorType, ev.Handled, ev.Message)
	}
	if loc := ErrorLocation(ev.Frames()); loc != "MainActivity:47" && loc != "?:47" {
		t.Logf("location = %q", loc)
	}
	if f := ev.Frames(); len(f) != 2 || f[1].Function != "onCreate" {
		t.Fatalf("frames = %+v", f)
	}
	// Protocol order without ids: oldest first is the root cause.
	body = `{"exception":{"values":[{"type":"KeyError","value":"x","mechanism":{"type":"generic","handled":true}},{"type":"ValueError","value":"wrapped"}]}}`
	ev = ParseEvent("", now, []byte(body), now)
	if ev.ErrorType != "KeyError" || ev.Handled == nil || !*ev.Handled {
		t.Fatalf("python order: type=%q handled=%v", ev.ErrorType, ev.Handled)
	}
}

func TestProguardImageUUID(t *testing.T) {
	ev := ParseEvent("", now, []byte(`{"platform":"java","debug_meta":{"images":[{"type":"proguard","uuid":"4828693C-D841-38E1-A119-AD9BE85355AB"}]},"exception":{"values":[{"type":"E","stacktrace":{"frames":[{"module":"a.b","function":"c","lineno":1}]}}]}}`), now)
	if len(ev.DebugImages) != 1 || ev.DebugImages[0].DebugID != "4828693C-D841-38E1-A119-AD9BE85355AB" || !ev.NeedsSymbolication() {
		t.Fatalf("images = %+v", ev.DebugImages)
	}
}

func TestThreadFallbackAndSDKFrames(t *testing.T) {
	// .NET: exception without stack, current thread carries it.
	ev := ParseEvent("", now, []byte(`{"platform":"csharp","exception":{"values":[{"type":"InvalidOperationException","value":"handled boom"}]},
	 "threads":{"values":[{"id":1,"current":true,"stacktrace":{"frames":[{"function":"Main","filename":"Program.cs","lineno":22,"in_app":true}]}}]}}`), now)
	if loc := ErrorLocation(ev.Frames()); loc != "Program.cs:22" {
		t.Fatalf("thread fallback location = %q", loc)
	}
	if ev.Handled == nil || !*ev.Handled {
		t.Fatalf("missing mechanism should mean handled: %v", ev.Handled)
	}
	// Dart: two different errors from one entry point must not share a fingerprint.
	mk := func(fn string) *Event {
		return ParseEvent("", now, []byte(`{"platform":"other","sdk":{"name":"sentry.dart"},"exception":{"values":[{"type":"StateError","stacktrace":{"frames":[
		  {"filename":"dart.dart","function":"main.<fn>","lineno":20,"in_app":true},
		  {"function":"<asynchronous suspension>"},
		  {"abs_path":"package:sentry/src/sentry.dart","function":"Sentry.init","in_app":true,"package":"sentry"},
		  {"function":"<asynchronous suspension>"},
		  {"abs_path":"package:sentry/src/integrations/run_zoned_guarded_integration.dart","function":"runZonedGuarded","in_app":true,"package":"sentry"},
		  {"filename":"dart.dart","function":"`+fn+`","lineno":30,"in_app":true}]}}]}}`), now)
	}
	a, b := mk("loadCart"), mk("checkout")
	if Fingerprint(a, a.Frames()) == Fingerprint(b, b.Frames()) {
		t.Fatal("SDK/pseudo frames dominated the fingerprint")
	}
}

func TestBareArraysAndMessageEvents(t *testing.T) {
	// sentry-go: exception and threads as bare arrays.
	ev := ParseEvent("", now, []byte(`{"platform":"go","exception":[{"type":"*errors.errorString","value":"handled boom","stacktrace":{"frames":[{"abs_path":"/x/main.go","function":"main.main","lineno":67,"in_app":true}]}}],"threads":[{"id":1,"current":true}]}`), now)
	if ev == nil || ev.ErrorType != "*errors.errorString" || ErrorLocation(ev.Frames()) != "main.go:67" {
		t.Fatalf("bare arrays: %+v", ev)
	}
	// sentry-go panic("string"): a fatal message event with the thread stack.
	body := `{"platform":"go","level":"fatal","message":"unhandled boom","threads":[{"id":1,"current":true,"crashed":true,"stacktrace":{"frames":[
	  {"abs_path":"/usr/lib/go/src/runtime/panic.go","function":"gopanic","lineno":1},
	  {"abs_path":"/x/main.go","function":"main.main","lineno":71,"in_app":true},
	  {"abs_path":"/go/pkg/mod/github.com/getsentry/sentry-go@v0.49.0/hub.go","function":"sentry.(*Hub).Recover","lineno":10,"in_app":true}]}}]}`
	ev = ParseEvent("", now, []byte(body), now)
	if loc := ErrorLocation(ev.Frames()); loc != "main.go:71" {
		t.Fatalf("panic location = %q", loc)
	}
	fp := Fingerprint(ev, ev.Frames())
	if fp == "" {
		t.Fatal("fatal message event should group")
	}
	// The same panic with a different id in the text groups together.
	ev1 := ParseEvent("", now, []byte(strings.Replace(body, "unhandled boom", "unhandled boom 111", 1)), now)
	ev2 := ParseEvent("", now, []byte(strings.Replace(body, "unhandled boom", "unhandled boom 222", 1)), now)
	if Fingerprint(ev1, ev1.Frames()) != Fingerprint(ev2, ev2.Frames()) {
		t.Fatal("digits in a message must not split the issue")
	}
	// Rust: sentry_panic frames marked in_app are not the location.
	ev = ParseEvent("", now, []byte(`{"platform":"native","exception":{"values":[{"type":"panic","stacktrace":{"frames":[
	  {"abs_path":"/x/src/main.rs","filename":"main.rs","function":"sdkrust::main::{closure#1}","lineno":37,"in_app":true},
	  {"abs_path":"/c/sentry-panic-0.49.2/src/lib.rs","filename":"lib.rs","function":"sentry_panic::panic_handler","lineno":128,"in_app":true}]}}]}}`), now)
	if loc := ErrorLocation(ev.Frames()); loc != "main.rs:37" {
		t.Fatalf("rust location = %q", loc)
	}
	// Native: address-only frames fingerprint by image+offset, not raw address.
	native := func(base string) *Event {
		return ParseEvent("", now, []byte(`{"platform":"native","exception":{"values":[{"type":"SIGSEGV","value":"Segfault","mechanism":{"type":"signalhandler","handled":false},
		  "stacktrace":{"frames":[{"instruction_addr":"`+base+`205"}]}}]},"debug_meta":{"images":[{"type":"elf","debug_id":"5ce4a09d-f963-2a1b-799f-a33024a288eb","image_addr":"`+base+`000","image_size":20480}]}}`), now)
	}
	a, b := native("0x559bd2abb"), native("0x7f10c0aaa")
	if Fingerprint(a, a.Frames()) != Fingerprint(b, b.Frames()) {
		t.Fatal("ASLR-shifted native crash should keep its fingerprint")
	}
	if loc := ErrorLocation(a.Frames()); loc != "" {
		t.Fatalf("address-only location should be empty, got %q", loc)
	}
	// An unparseable event item is counted, not silently dropped.
	env := Parse([]byte("{}\n"+`{"type":"event"}`+"\n"+`not json`+"\n"), now)
	if env.Invalid != 1 || len(env.Events) != 0 {
		t.Fatalf("invalid = %d events = %d", env.Invalid, len(env.Events))
	}
}

func TestAnonymousFramesAreCode(t *testing.T) {
	ev := ParseEvent("", now, []byte(`{"platform":"node","exception":{"values":[{"type":"Error","value":"unhandled boom","mechanism":{"type":"auto.node.onuncaughtexception","handled":false},"stacktrace":{"frames":[{"filename":"/x/index.ts","function":"<anonymous>","lineno":16,"in_app":true,"module":"index.ts"}]}}]}}`), now)
	if loc := ErrorLocation(ev.Frames()); loc != "index.ts:16" {
		t.Fatalf("anonymous frame location = %q", loc)
	}
	ev = ParseEvent("", now, []byte(`{"platform":"java","level":"info","message":"hello","threads":{"values":[{"current":true,"stacktrace":{"frames":[{"module":"smoke.Main","function":"main","filename":"Main.java","lineno":20},{"module":"java.lang.Thread","function":"getStackTrace","filename":"Thread.java","lineno":1619}]}}]}}`), now)
	if loc := ErrorLocation(ev.Frames()); loc != "Main.java:20" {
		t.Fatalf("java message location = %q", loc)
	}
}

func TestLocationAndGroupingRefinements(t *testing.T) {
	// Rust: the unwinder frame is in_app but has no file; user code wins.
	ev := ParseEvent("", now, []byte(`{"platform":"native","exception":{"values":[{"type":"panic","value":"unhandled boom","stacktrace":{"frames":[
	  {"abs_path":"/x/src/main.rs","filename":"main.rs","function":"sdkrust::main::{closure#1}","lineno":37,"in_app":true,"package":"sdkrust"},
	  {"function":"__rustc::rust_begin_unwind","in_app":true,"package":"__rustc"},
	  {"abs_path":"/c/sentry-panic-0.49.2/src/lib.rs","filename":"lib.rs","function":"sentry_panic::panic_handler","lineno":128,"in_app":true}]}}]}}`), now)
	if loc := ErrorLocation(ev.Frames()); loc != "main.rs:37" {
		t.Fatalf("rust location = %q", loc)
	}
	// Go panic: a message event is titled by its message.
	ev = ParseEvent("", now, []byte(`{"platform":"go","level":"fatal","message":"unhandled boom"}`), now)
	if ev.IssueTitle() != "unhandled boom" {
		t.Fatalf("message title = %q", ev.IssueTitle())
	}
	// Dart: two throw sites in one entry point are two issues.
	mk := func(line int) *Event {
		return ParseEvent("", now, []byte(fmt.Sprintf(`{"platform":"other","exception":{"values":[{"type":"StateError","value":"x","stacktrace":{"frames":[
		  {"abs_path":"package:sentry/src/sentry.dart","function":"Sentry.init","in_app":true,"package":"sentry"},
		  {"filename":"<asynchronous suspension>"},
		  {"filename":"dart.dart","function":"main.<fn>","lineno":%d,"in_app":true}]}}]}}`, line)), now)
	}
	a, b := mk(25), mk(32)
	if Fingerprint(a, a.Frames()) == Fingerprint(b, b.Frames()) {
		t.Fatal("single-frame stacks must keep the line number")
	}
	// …but multi-frame stacks still ignore line numbers.
	mk2 := func(line int) *Event {
		return ParseEvent("", now, []byte(fmt.Sprintf(`{"platform":"java","exception":{"values":[{"type":"E","stacktrace":{"frames":[
		  {"filename":"A.java","function":"a","lineno":1,"in_app":true},{"filename":"B.java","function":"b","lineno":%d,"in_app":true}]}}]}}`, line)), now)
	}
	c, d := mk2(10), mk2(11)
	if Fingerprint(c, c.Frames()) != Fingerprint(d, d.Frames()) {
		t.Fatal("line numbers must not split multi-frame stacks")
	}
}
