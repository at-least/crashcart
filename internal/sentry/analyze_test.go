package sentry

import (
	"fmt"
	"strings"
	"testing"
)

func eventJSON(fields string) *Event {
	// One item body per line: strip the newlines the test literals use.
	fields = strings.Join(strings.Fields(fields), " ")
	env := Parse(envelope(`{"type":"event"}`, `{`+fields+`}`), now)
	if len(env.Events) != 1 {
		panic("no event")
	}
	return env.Events[0]
}

// Native frames whose address no debug image covers must not put the raw
// (ASLR-randomized) address in the fingerprint: two crashes at different
// load addresses are one issue.
func TestFingerprintUnmappedAddressIsStable(t *testing.T) {
	ev := func(addr string) *Event {
		return eventJSON(fmt.Sprintf(`"platform":"cocoa","exception":{"values":[{"type":"EXC_BAD_ACCESS","stacktrace":{"frames":[
			{"instruction_addr":%q,"package":"/usr/lib/system/libsystem_kernel.dylib","in_app":false},
			{"instruction_addr":%q,"package":"/private/var/containers/App.app/App","in_app":true}]}}]},
			"debug_meta":{"images":[]}`, addr, addr))
	}
	a, b := ev("0x1049e2b50"), ev("0x1f4e2b50")
	fa, fb := Fingerprint(a, a.Frames()), Fingerprint(b, b.Frames())
	if fa == "" || fa != fb {
		t.Fatalf("fingerprints differ across load addresses: %s vs %s", fa, fb)
	}
	// With the image known, the offset (stable) is part of the signature
	// and a different offset is a different issue.
	withImage := func(addr string) *Event {
		return eventJSON(fmt.Sprintf(`"platform":"cocoa","exception":{"values":[{"type":"EXC_BAD_ACCESS","stacktrace":{"frames":[
			{"instruction_addr":%q,"in_app":true}]}}]},
			"debug_meta":{"images":[{"type":"macho","debug_id":"D","image_addr":"0x104900000","image_size":1048576}]}`, addr))
	}
	c, d := withImage("0x104900010"), withImage("0x104900020")
	if Fingerprint(c, c.Frames()) == Fingerprint(d, d.Frames()) {
		t.Fatal("different offsets in a known image must differ")
	}
}

// "{{ default }}" in an SDK fingerprint stands for the default grouping.
func TestFingerprintSDKDefaultToken(t *testing.T) {
	base := `"platform":"javascript","exception":{"values":[{"type":"TypeError","value":"x is undefined","stacktrace":{"frames":[{"filename":"app.js","function":"load"}]}}]}`
	plain := eventJSON(base)
	def := eventJSON(base + `,"fingerprint":["{{ default }}"]`)
	if Fingerprint(plain, plain.Frames()) != Fingerprint(def, def.Frames()) {
		t.Fatal(`["{{ default }}"] must equal the default grouping`)
	}
	other := eventJSON(`"platform":"javascript","exception":{"values":[{"type":"RangeError","stacktrace":{"frames":[{"filename":"b.js","function":"f"}]}}]},"fingerprint":["{{default}}"]`)
	if Fingerprint(other, other.Frames()) == Fingerprint(def, def.Frames()) {
		t.Fatal("two different errors with {{ default }} must not collapse into one issue")
	}
	split := eventJSON(base + `,"fingerprint":["{{ default }}","tenant-a"]`)
	split2 := eventJSON(base + `,"fingerprint":["{{ default }}","tenant-b"]`)
	if Fingerprint(split, split.Frames()) == Fingerprint(plain, plain.Frames()) || Fingerprint(split, split.Frames()) == Fingerprint(split2, split2.Frames()) {
		t.Fatal("{{ default }} + key must split the default issue by key")
	}
	fixed := eventJSON(base + `,"fingerprint":["my-key"]`)
	fixed2 := eventJSON(`"platform":"javascript","message":"anything","level":"info","fingerprint":["my-key"]`)
	if Fingerprint(fixed, fixed.Frames()) != Fingerprint(fixed2, fixed2.Frames()) {
		t.Fatal("a literal SDK fingerprint groups regardless of content")
	}
	none := eventJSON(`"platform":"javascript","message":"info line","level":"info","fingerprint":["{{ default }}"]`)
	if Fingerprint(none, none.Frames()) != "" {
		t.Fatal("{{ default }} alone on an ungroupable event stays ungrouped")
	}
}

// A session with a status outside the enum is dropped (it would fail the
// whole envelope in Postgres); the counts per envelope are bounded.
func TestParseSessionStatusAndLimits(t *testing.T) {
	env := Parse(envelope(
		`{"type":"session"}`, `{"sid":"s1","status":"OK","attrs":{"release":"2.0"}}`,
		`{"type":"session"}`, `{"sid":"s2","status":"crashed","attrs":{"release":"2.0"}}`,
	), now)
	if len(env.Sessions) != 1 || env.Sessions[0].SID != "s2" {
		t.Fatalf("sessions = %+v", env.Sessions)
	}
	var items []string
	for i := 0; i < MaxEvents+50; i++ {
		items = append(items, `{"type":"event"}`, fmt.Sprintf(`{"event_id":"%032x","message":"m"}`, i))
	}
	env = Parse(envelope(items...), now)
	if len(env.Events) != MaxEvents+1 || env.Dropped != 49 {
		t.Fatalf("events parsed = %d dropped = %d", len(env.Events), env.Dropped)
	}
	var aggs []string
	for i := 0; i < MaxSessions+10; i++ {
		aggs = append(aggs, fmt.Sprintf(`{"started":"2026-08-28T%02d:00:00Z","exited":1}`, i%24))
	}
	env = Parse(envelope(`{"type":"sessions"}`, `{"attrs":{"release":"2.0"},"aggregates":[`+strings.Join(aggs, ",")+`]}`), now)
	if len(env.Sessions) != MaxSessions || env.Dropped != 10 {
		t.Fatalf("sessions parsed = %d dropped = %d", len(env.Sessions), env.Dropped)
	}
}
