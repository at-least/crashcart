// Package pk encodes event time into the events primary key.
//
//	id = unix milliseconds × 1000 + random(0..999)
//
// So a time range is an id range, ordering by id is chronological, and the
// table needs no secondary index. Values stay below 2^53 until the year
// 2255, so ids survive JSON → JavaScript numbers intact.
package pk

import "time"

// Scale is the number of ids per millisecond.
const Scale = 1000

// New builds an id for t; rnd must return a value in [0, Scale).
func New(t time.Time, rnd func() int64) int64 {
	return t.UnixMilli()*Scale + rnd()%Scale
}

// Lower is the smallest id at or after t (inclusive range start).
func Lower(t time.Time) int64 { return t.UnixMilli() * Scale }

// Upper is the smallest id at or after t — use as an exclusive range end.
func Upper(t time.Time) int64 { return Lower(t) }

// Time recovers the event time (ms precision) from an id.
func Time(id int64) time.Time { return time.UnixMilli(id / Scale).UTC() }
