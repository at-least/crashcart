package pk

import (
	"testing"
	"time"
)

func TestRoundTrip(t *testing.T) {
	ts := time.Date(2026, 8, 29, 12, 34, 56, 789_000_000, time.UTC)
	id := New(ts)
	if got := Time(id); !got.Equal(ts) {
		t.Fatalf("Time(New(ts)) = %v, want %v", got, ts)
	}
	if id < Lower(ts) || id >= Lower(ts.Add(time.Millisecond)) {
		t.Fatalf("id %d outside its millisecond", id)
	}
	if Bucket(id, Hour) != Lower(ts.Truncate(time.Hour)) {
		t.Fatalf("Bucket mismatch")
	}
}
