package api

import (
	"net/url"
	"strconv"
	"time"
)

// Window limits.
const (
	DefaultDays = 7
	MaxDays     = 90
)

// ParseWindow reads the time window of a request: `days=N` (default 7,
// max 90) ending now, or explicit `from` / `to` RFC3339 bounds (`to` is
// exclusive and defaults to now; `from` defaults to `to` minus days). The
// span may not exceed MaxDays. Both bounds are returned in UTC; convert
// with pk.Lower(from) / pk.Upper(to) for id ranges.
func ParseWindow(q url.Values) (from, to time.Time, err error) {
	return parseWindowAt(q, time.Now())
}

func parseWindowAt(q url.Values, now time.Time) (from, to time.Time, err error) {
	now = now.UTC()
	days := DefaultDays
	if s := q.Get("days"); s != "" {
		n, perr := strconv.Atoi(s)
		if perr != nil || n < 1 {
			return from, to, badRequest("days must be a positive integer")
		}
		if n > MaxDays {
			return from, to, badRequest("days must be <= " + strconv.Itoa(MaxDays))
		}
		days = n
	}
	to = now
	if s := q.Get("to"); s != "" {
		if to, err = time.Parse(time.RFC3339, s); err != nil {
			return from, to, badRequest("to must be RFC3339")
		}
		to = to.UTC()
	}
	if s := q.Get("from"); s != "" {
		if from, err = time.Parse(time.RFC3339, s); err != nil {
			return from, to, badRequest("from must be RFC3339")
		}
		from = from.UTC()
	} else {
		from = to.AddDate(0, 0, -days)
	}
	if !from.Before(to) {
		return from, to, badRequest("from must be before to")
	}
	if to.Sub(from) > time.Duration(MaxDays)*24*time.Hour {
		return from, to, badRequest("window may not exceed " + strconv.Itoa(MaxDays) + " days")
	}
	return from, to, nil
}
