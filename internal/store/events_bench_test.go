package store

// Investigates one code-review finding: does binding InsertEvents' batch
// args via pgx.StrictStructArgs(r) (reflection + a map[string]any per row)
// cost meaningfully more than building the old plain []any slice by hand?
// Isolates exactly the part that changed — the rewrite step — with no DB
// round trip on either side, so the delta is pgx's own overhead, not noise.

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/at-least/crashcart/internal/sentry"
)

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
// pgx.StrictStructArgs(r).RewriteQuery per row.
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

// BenchmarkInsertEventArgs_Positional is the pre-refactor code: a
// hand-written []any slice, no reflection, no rewrite.
func BenchmarkInsertEventArgs_Positional(b *testing.B) {
	r := sampleEventInsert()
	b.ReportAllocs()
	for b.Loop() {
		args := []any{r.OccurredAt, r.ProjectID, r.EventID, r.Level, r.Message, r.Platform, r.Environment, r.Release,
			r.DeviceID, r.DeviceModel, r.OSVersion, r.Transaction, r.ErrorType, r.Culprit, r.Handled, r.SDKName, r.UserID,
			r.Fingerprint, r.Symbolicated, r.Tags, r.Symbols, r.Payload}
		if len(args) != 22 {
			b.Fatal("unreachable")
		}
	}
}

// BenchmarkInsertEventsBatch_50 is the realistic shape: one envelope's
// worth of rows, with the RewriteQuery step SendBatch actually runs per
// queued item (conn.go's SendBatch calls it once per QueuedQuery) —
// Batch.Queue alone only stores the unevaluated QueryRewriter, so a
// benchmark stopping there would understate the real per-row cost.
func BenchmarkInsertEventsBatch_50(b *testing.B) {
	rows := make([]EventInsert, 50)
	for i := range rows {
		rows[i] = sampleEventInsert()
	}
	ctx := context.Background()
	b.ReportAllocs()
	for b.Loop() {
		for _, r := range rows {
			if _, _, err := pgx.StrictStructArgs(r).RewriteQuery(ctx, nil, insertEventSQL, nil); err != nil {
				b.Fatal(err)
			}
		}
	}
}
