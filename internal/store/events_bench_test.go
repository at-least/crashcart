package store

// Investigates one code-review finding: does binding InsertEvents' batch
// args via pgx.StrictStructArgs(r) (reflection + a map[string]any per row)
// cost meaningfully more than a hand-written []any slice? Isolates exactly
// the rewrite step, with no DB round trip on either side, so the delta is
// pgx's own overhead, not noise.

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/crashcartapp/crashcart/internal/sentry"
)

// sinkArgs exists only so BenchmarkInsertEventArgs_Positional's slice
// escapes and can't be dead-code-eliminated: an earlier version of this
// benchmark discarded the slice after a `len(args) != 22` check, which the
// compiler can prove without ever allocating the slice, so it measured
// 9ns/0 allocs — a fabricated baseline, not the real cost of building it.
// Assigning to a package-level var forces the work to actually happen.
var sinkArgs []any

// sampleEventInsert's field values duplicate EventInsert's own field list
// (already true of any fixture); its use of keyed struct-literal fields
// means a reorder or a typo'd key is a compile error, not silent drift.
// BenchmarkInsertEventArgs_Positional's []any slice below is the one place
// that still lists the 22 fields by hand in call order — it exists to
// measure what the old positional-args code cost, so it can't avoid that;
// keep it in sync with EventInsert by hand if a field is added or removed.
func sampleEventInsert() EventInsert {
	platform, env, release := "android", "production", "1.0.0"
	deviceID, deviceModel, osVersion := "d1", "Pixel 8", "14"
	transaction, errorType, culprit := "MainActivity.onCreate", "NullPointerException", "com.example.Main.onCreate"
	sdkName, userID := "sentry.java", "u1"
	handled := false
	fp := sentry.DerivedID([]byte("fingerprint"))
	return EventInsert{
		OccurredAt: time.Now(), ProjectID: 1, EventID: sentry.DerivedID([]byte("event")),
		Level: EventLevelError, Message: "boom",
		Platform: &platform, Environment: &env, Release: &release,
		DeviceID: &deviceID, DeviceModel: &deviceModel, OSVersion: &osVersion,
		Transaction: &transaction, ErrorType: &errorType, Culprit: &culprit,
		Handled: &handled, SDKName: &sdkName, UserID: &userID, Fingerprint: &fp,
		Symbolicated: true, Tags: json.RawMessage(`{"env":"prod"}`), Symbols: nil,
		Payload: []byte("a reasonably sized gzipped event payload placeholder"),
	}
}

// BenchmarkInsertEventArgs_StrictStructArgs is today's code: one
// pgx.StrictStructArgs(r).RewriteQuery per row — what SendBatch actually
// runs once per queued item (conn.go), not just Batch.Queue, which only
// stores the unevaluated QueryRewriter. A 50-row batch (roughly one
// envelope) costs ~50x this, linearly — SendBatch does nothing else that
// scales per row, so there's no separate batch benchmark to keep in sync.
func BenchmarkInsertEventArgs_StrictStructArgs(b *testing.B) {
	r := sampleEventInsert()
	ctx := context.Background()
	b.ReportAllocs()
	for b.Loop() {
		if _, _, err := pgx.StrictStructArgs(r).RewriteQuery(ctx, nil, insertEventSQL, nil); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkInsertEventArgs_Positional is what a hand-written []any slice
// costs — no reflection, no SQL rewrite — for comparison. It does not
// represent this diff's own before/after: InsertEvents already used
// StrictStructArgs before this file existed (2d331df); this is a
// reconstruction of what the positional code once cost, not a record of
// what changed here.
func BenchmarkInsertEventArgs_Positional(b *testing.B) {
	r := sampleEventInsert()
	b.ReportAllocs()
	for b.Loop() {
		sinkArgs = []any{r.OccurredAt, r.ProjectID, r.EventID, r.Level, r.Message, r.Platform, r.Environment, r.Release,
			r.DeviceID, r.DeviceModel, r.OSVersion, r.Transaction, r.ErrorType, r.Culprit, r.Handled, r.SDKName, r.UserID,
			r.Fingerprint, r.Symbolicated, r.Tags, r.Symbols, r.Payload}
	}
}
