package monitors

// Transition is the result of recording one outcome (a terminal ok/error
// check-in, a missed check-in, or a timed-out one) against a monitor's
// running state. Failed/Recovered are one-shot: true only on the tick
// that crosses the threshold, never on every tick while past it — the
// same shape as an issue's escalation, applied to a plain counter instead
// of a stats rollup.
type Transition struct {
	ConsecutiveFailures  int32
	ConsecutiveSuccesses int32
	Alerting             bool
	Failed, Recovered    bool
}

// Record advances (failures, successes, alerting) for one outcome — ok is
// the only one that counts as success; error, missed and timeout all
// count as failure alike. Called from both ingest (a terminal check-in)
// and alerts.CheckMonitors (a missed or timed-out one), so the two agree
// on what "one more failure" means.
func Record(failures, successes int32, alerting bool, failureThreshold, recoveryThreshold int32, ok bool) Transition {
	var t Transition
	if ok {
		t.ConsecutiveSuccesses = successes + 1
	} else {
		t.ConsecutiveFailures = failures + 1
	}
	t.Alerting = alerting
	switch {
	case !alerting && t.ConsecutiveFailures >= failureThreshold:
		t.Alerting, t.Failed = true, true
	case alerting && t.ConsecutiveSuccesses >= recoveryThreshold:
		t.Alerting, t.Recovered = false, true
	}
	return t
}
