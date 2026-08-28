package ingest

// keep decides whether an event of `level` survives sampling. error and
// fatal are always kept; warning keeps at least 50%; info/debug follow rate.
func keep(level string, rate float64, rnd func() float64) bool {
	switch level {
	case "error", "fatal":
		return true
	case "warning":
		return rnd() < max(rate, 0.5)
	default:
		return rnd() < rate
	}
}
