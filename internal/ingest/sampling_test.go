package ingest

import "testing"

// TestSampleStore pins the deterministic edges of the sampling decision:
// rate 0 and rate 1 always answer the same way, and the unhandled boost
// caps at 1 rather than overshooting into a >1 "probability".
func TestSampleStore(t *testing.T) {
	cases := []struct {
		rate      float64
		unhandled bool
		want      bool
	}{
		{0, false, false},
		{0, true, false}, // 0 × UnhandledKeepFactor is still 0
		{1, false, true},
		{1, true, true},
		{1.0 / UnhandledKeepFactor, true, true}, // boosted rate hits exactly 1: capped, not rolled
	}
	for _, c := range cases {
		if got := sampleStore(c.rate, c.unhandled); got != c.want {
			t.Errorf("sampleStore(%v, %v) = %v, want %v", c.rate, c.unhandled, got, c.want)
		}
	}
}
