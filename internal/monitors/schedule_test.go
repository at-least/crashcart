package monitors

import (
	"testing"
	"time"
)

func TestParseScheduleCrontab(t *testing.T) {
	s, err := ParseSchedule("crontab", "0 * * * *", "")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	got := s.Next(time.Date(2026, 1, 1, 10, 15, 0, 0, time.UTC))
	want := time.Date(2026, 1, 1, 11, 0, 0, 0, time.UTC)
	if !got.Equal(want) {
		t.Fatalf("next = %v, want %v", got, want)
	}
}

func TestParseScheduleCrontabInvalid(t *testing.T) {
	if _, err := ParseSchedule("crontab", "not a cron expression", ""); err == nil {
		t.Fatal("want error for invalid crontab")
	}
}

func TestParseScheduleInterval(t *testing.T) {
	s, err := ParseSchedule("interval", "2", "hour")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	now := time.Date(2026, 1, 1, 10, 15, 0, 0, time.UTC)
	got := s.Next(now)
	if want := now.Add(2 * time.Hour); !got.Equal(want) {
		t.Fatalf("next = %v, want %v", got, want)
	}
}

func TestParseScheduleIntervalInvalid(t *testing.T) {
	cases := []struct{ value, unit string }{
		{"0", "hour"}, {"-1", "hour"}, {"abc", "hour"}, {"1", "fortnight"}, {"1", ""},
	}
	for _, c := range cases {
		if _, err := ParseSchedule("interval", c.value, c.unit); err == nil {
			t.Errorf("value=%q unit=%q: want error", c.value, c.unit)
		}
	}
}

func TestParseScheduleUnknownType(t *testing.T) {
	if _, err := ParseSchedule("weekly", "1", ""); err == nil {
		t.Fatal("want error for unknown schedule type")
	}
}

func TestRecordFailureThresholdFiresOnce(t *testing.T) {
	var failures, successes int32
	var alerting bool
	for i := 1; i <= 3; i++ {
		tr := Record(failures, successes, alerting, 3, 1, false)
		failures, successes, alerting = tr.ConsecutiveFailures, tr.ConsecutiveSuccesses, tr.Alerting
		wantFired := i == 3
		if tr.Failed != wantFired {
			t.Fatalf("failure %d: fired=%v, want %v", i, tr.Failed, wantFired)
		}
	}
	// A fourth failure must not re-fire: already alerting.
	if tr := Record(failures, successes, alerting, 3, 1, false); tr.Failed {
		t.Fatal("fourth consecutive failure re-fired monitor_failed")
	}
}

func TestRecordRecoveryRequiresPriorAlert(t *testing.T) {
	// Never alerting: a bare "ok" must not fire monitor_recovered.
	if tr := Record(0, 0, false, 1, 1, true); tr.Recovered {
		t.Fatal("recovered fired without a prior failure")
	}
}

func TestRecordRecoveryAfterThreshold(t *testing.T) {
	// Already alerting (one failure crossed threshold 1); two oks needed.
	tr := Record(1, 0, true, 1, 2, true)
	if tr.Recovered {
		t.Fatal("recovered fired after only one ok (threshold 2)")
	}
	if !tr.Alerting {
		t.Fatal("alerting cleared before recovery threshold was reached")
	}
	tr = Record(tr.ConsecutiveFailures, tr.ConsecutiveSuccesses, tr.Alerting, 1, 2, true)
	if !tr.Recovered || tr.Alerting {
		t.Fatalf("second ok: recovered=%v alerting=%v, want recovered=true alerting=false", tr.Recovered, tr.Alerting)
	}
}

func TestRecordSuccessResetsFailures(t *testing.T) {
	tr := Record(5, 0, false, 3, 1, true)
	if tr.ConsecutiveFailures != 0 {
		t.Fatalf("consecutive failures = %d after an ok, want 0", tr.ConsecutiveFailures)
	}
}
