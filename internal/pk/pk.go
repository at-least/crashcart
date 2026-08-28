// Package pk encodes event time into the primary key of the time-series
// tables (events, sessions):
//
//	id = unix milliseconds × 1000 + random(0..999)
//
// The id doubles as the TimescaleDB time dimension, so a time range is an
// id range, ordering by id is chronological, and chunk exclusion works on
// the primary key alone. Values stay below 2^53 until the year 2255, so ids
// survive JSON → JavaScript numbers intact.
package pk

import (
	"math/rand/v2"
	"time"
)

// Scale is the number of ids per millisecond.
const Scale = 1000

// Micro is the number of id units in one second (ids are microseconds).
const Micro = 1_000_000

// Hour and Day are bucket widths in id units.
const (
	Hour int64 = 3600 * Micro
	Day  int64 = 24 * Hour
)

// New builds an id for t with a random low part.
func New(t time.Time) int64 { return t.UnixMilli()*Scale + rand.Int64N(Scale) }

// Lower is the smallest id at or after t (inclusive range start).
func Lower(t time.Time) int64 { return t.UnixMilli() * Scale }

// Upper is the smallest id at or after t — use as an exclusive range end.
func Upper(t time.Time) int64 { return Lower(t) }

// Time recovers the event time (ms precision) from an id.
func Time(id int64) time.Time { return time.UnixMilli(id / Scale).UTC() }

// Bucket truncates an id to the start of its width-sized bucket
// (the same arithmetic as time_bucket on the integer dimension).
func Bucket(id, width int64) int64 { return id - id%width }
