package ingest

import (
	"testing"
	"time"

	"github.com/newlix/crashcart/internal/db/sqlc"
	"github.com/newlix/crashcart/internal/pk"
)

func TestAssignIDsUniqueWithinBatch(t *testing.T) {
	in := New(nil, Options{RandID: func() int64 { return 7 }}) // every event wants suffix 7
	ts := time.Date(2026, 8, 29, 10, 0, 0, 0, time.UTC)
	events := make([]sqlc.InsertEventsParams, 5)
	times := []time.Time{ts, ts, ts, ts.Add(time.Millisecond), ts}
	in.assignIDs(events, times)
	seen := map[int64]bool{}
	for i, e := range events {
		if seen[e.ID] {
			t.Fatalf("duplicate id %d", e.ID)
		}
		seen[e.ID] = true
		if !pk.Time(e.ID).Equal(times[i]) {
			t.Errorf("event %d: id %d does not encode %v", i, e.ID, times[i])
		}
	}
}
