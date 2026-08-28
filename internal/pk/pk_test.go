package pk

import (
	"testing"
	"time"
)

func TestRoundTrip(t *testing.T) {
	ts := time.Date(2026, 8, 29, 10, 15, 30, 123_000_000, time.UTC)
	id := New(ts, func() int64 { return 999 })
	if id != ts.UnixMilli()*1000+999 {
		t.Fatalf("id = %d", id)
	}
	if !Time(id).Equal(ts) {
		t.Errorf("Time(id) = %v", Time(id))
	}
	if Lower(ts) > id || Upper(ts.Add(time.Millisecond)) <= id {
		t.Error("range bounds must bracket the id")
	}
	if id >= 1<<53 {
		t.Error("ids must stay JSON/JavaScript safe")
	}
	// suffix wraps into [0, Scale)
	if New(ts, func() int64 { return 1234 }) != ts.UnixMilli()*1000+234 {
		t.Error("suffix modulo")
	}
}
