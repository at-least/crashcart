package store

import (
	"strings"
	"time"

	"github.com/crashcartapp/crashcart/internal/sentry"
)

// Cursor is a keyset position in the newest-first event order
// (occurred_at DESC, event_id DESC): the page after it holds rows that
// sort strictly after it. Zero = start from the newest event.
type Cursor struct {
	At      time.Time
	EventID sentry.ID
}

// IsZero reports whether the cursor is unset.
func (c Cursor) IsZero() bool { return c.At.IsZero() }

// String encodes the cursor for a URL: "<RFC3339Nano>_<event_id>" (the
// timestamp has no underscore, so the first one splits).
func (c Cursor) String() string {
	if c.IsZero() {
		return ""
	}
	return c.At.UTC().Format(time.RFC3339Nano) + "_" + string(c.EventID)
}

// ParseCursor decodes String(); "" is the zero cursor.
func ParseCursor(s string) (Cursor, bool) {
	if s == "" {
		return Cursor{}, true
	}
	ts, id, ok := strings.Cut(s, "_")
	if !ok || id == "" {
		return Cursor{}, false
	}
	t, err := time.Parse(time.RFC3339Nano, ts)
	if err != nil {
		return Cursor{}, false
	}
	eid, ok := sentry.ParseID(id)
	if !ok {
		return Cursor{}, false
	}
	return Cursor{At: t.UTC(), EventID: eid}, true
}

// CursorOf is the cursor pointing at row r.
func CursorOf(r EventRow) Cursor { return Cursor{At: r.OccurredAt, EventID: r.EventID} }
