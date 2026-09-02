// Package monitors is the schedule math for Sentry's cron monitoring
// (the check_in envelope item): parsing and validating a monitor_config's
// schedule, and computing the next expected check-in from it. Shared by
// internal/sentry (validates a schedule at parse time — a bad one drops
// the item rather than creating a half-configured monitor) and
// internal/ingest / internal/alerts (compute Next from the columns a
// monitor was upserted with), so the two never drift apart.
//
// Go's stdlib has no crontab parser; robfig/cron/v3 is CrashCart's only
// dependency beyond pgx/templ/x-crypto/x-sync, added for this reason.
package monitors

import (
	"fmt"
	"strconv"
	"time"

	"github.com/robfig/cron/v3"
)

// Schedule computes the next expected time strictly after t (t is assumed
// to already be in the monitor's timezone: cron.Schedule.Next honors
// t.Location()).
type Schedule interface {
	Next(t time.Time) time.Time
}

// cronParser accepts the standard 5-field crontab (minute hour dom month
// dow) — the form Sentry's crons integrations send — not the 6-field
// seconds variant some cron libraries default to.
var cronParser = cron.NewParser(cron.Minute | cron.Hour | cron.Dom | cron.Month | cron.Dow)

// IntervalUnits are the accepted interval schedule units.
var IntervalUnits = map[string]time.Duration{
	"minute": time.Minute, "hour": time.Hour, "day": 24 * time.Hour, "week": 7 * 24 * time.Hour,
}

// ParseSchedule validates and parses a monitor_config's schedule: type
// "crontab" with a standard 5-field expression, or "interval" with a
// positive count and a minute/hour/day/week unit. The error is safe to
// report back (it never echoes more than the caller passed in).
func ParseSchedule(typ, value, unit string) (Schedule, error) {
	switch typ {
	case "crontab":
		s, err := cronParser.Parse(value)
		if err != nil {
			return nil, fmt.Errorf("invalid crontab expression: %w", err)
		}
		return s, nil
	case "interval":
		n, err := strconv.Atoi(value)
		if err != nil || n <= 0 {
			return nil, fmt.Errorf("interval value must be a positive integer")
		}
		d, ok := IntervalUnits[unit]
		if !ok {
			return nil, fmt.Errorf("interval unit must be minute, hour, day or week")
		}
		return intervalSchedule{d * time.Duration(n)}, nil
	default:
		return nil, fmt.Errorf("schedule type must be crontab or interval")
	}
}

// intervalSchedule fires every d after the reference time: the run's own
// completion, not a fixed origin (Sentry anchors interval schedules to the
// monitor's creation; CrashCart anchors each occurrence to the previous
// one instead — simpler, and DST-transparent for day/week units expressed
// as wall-clock-independent durations).
type intervalSchedule struct{ d time.Duration }

func (s intervalSchedule) Next(t time.Time) time.Time { return t.Add(s.d) }
