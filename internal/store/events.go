package store

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/at-least/crashcart/internal/db/sqlc"
	"github.com/at-least/crashcart/internal/sentry"
)

// EventInsert is one row for InsertEvents.
type EventInsert struct {
	OccurredAt   time.Time
	ProjectID    int64
	EventID      sentry.ID
	Level        string
	Message      string
	Platform     *string
	Environment  *string
	Release      *string
	DeviceID     *string
	DeviceModel  *string
	OSVersion    *string
	Transaction  *string
	ErrorType    *string
	Culprit      *string
	Handled      *bool
	SDKName      *string
	UserID       *string
	Fingerprint  *sentry.ID
	Symbolicated bool
	Tags         json.RawMessage
	Symbols      json.RawMessage
	Payload      []byte // the raw event, gzipped (nil: none)
}

const insertEventSQL = `INSERT INTO events (occurred_at, project_id, event_id, level, message, platform, environment, release,
	device_id, device_model, os_version, transaction, error_type, culprit, handled, sdk_name, user_id,
	fingerprint, symbolicated, tags, symbols, payload)
	VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,$19,$20,$21,$22)
	ON CONFLICT (project_id, event_id, occurred_at) DO NOTHING`

// insertEventSpoolSQL is insertEventSQL for a process with a blob store:
// payload NULL on the row, the bytes ($22) spooled — only when the row
// was inserted, and only when there are bytes (an import without one).
const insertEventSpoolSQL = `WITH ins AS (INSERT INTO events (occurred_at, project_id, event_id, level, message, platform, environment, release,
	device_id, device_model, os_version, transaction, error_type, culprit, handled, sdk_name, user_id,
	fingerprint, symbolicated, tags, symbols, payload)
	VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,$19,$20,$21,NULL)
	ON CONFLICT (project_id, event_id, occurred_at) DO NOTHING
	RETURNING project_id, event_id, occurred_at)
	INSERT INTO payload_spool (project_id, event_id, occurred_at, data)
	SELECT project_id, event_id, occurred_at, $22::bytea FROM ins WHERE $22::bytea IS NOT NULL`

// InsertEvents writes a batch in one round trip (pipelined) and marks
// the hours it touched dirty in the same batch — the one rule every
// writer of events must follow, so it lives with the insert. Duplicate
// keys are skipped, so re-delivery is safe.
func (s *Store) InsertEvents(ctx context.Context, tx pgx.Tx, rows []EventInsert) error {
	if len(rows) == 0 {
		return nil
	}
	// With a blob store the payload goes to the spool instead of the
	// column, in the same statement: a spool row exists only for an event
	// row that was actually inserted (a resend conflicts and spools
	// nothing), and both roll back together (packs.go).
	sql := insertEventSQL
	if s.Blobs != nil {
		sql = insertEventSpoolSQL
	}
	b := &pgx.Batch{}
	hours := map[int64][]time.Time{}
	for _, r := range rows {
		b.Queue(sql, r.OccurredAt, r.ProjectID, r.EventID, r.Level, r.Message, r.Platform, r.Environment, r.Release,
			r.DeviceID, r.DeviceModel, r.OSVersion, r.Transaction, r.ErrorType, r.Culprit, r.Handled, r.SDKName, r.UserID,
			r.Fingerprint, r.Symbolicated, r.Tags, r.Symbols, r.Payload)
		hours[r.ProjectID] = addHour(hours[r.ProjectID], r.OccurredAt)
	}
	return runBatch(ctx, tx, b, markEventHours, hours)
}

// The dirty marks insert in bucket order (ORDER BY): the upsert locks one
// row per hour until commit, and two envelopes spanning the same hours in
// opposite orders would otherwise deadlock. The sqlc MarkEventStatsDirty /
// MarkSessionStatsDirty are the same statements for callers without a batch.
const (
	markEventHours   = `INSERT INTO event_stats_dirty (project_id, bucket) SELECT $1, b FROM unnest($2::timestamptz[]) AS b ORDER BY b ON CONFLICT (project_id, bucket) DO UPDATE SET gen = event_stats_dirty.gen + 1`
	markSessionHours = `INSERT INTO session_stats_dirty (project_id, bucket) SELECT $1, b FROM unnest($2::timestamptz[]) AS b ORDER BY b ON CONFLICT (project_id, bucket) DO UPDATE SET gen = session_stats_dirty.gen + 1`
)

// addHour appends t's UTC hour to hs unless it is there (a batch spans
// one or two hours: a slice beats a map).
func addHour(hs []time.Time, t time.Time) []time.Time {
	h := t.UTC().Truncate(time.Hour)
	if slices.ContainsFunc(hs, h.Equal) {
		return hs
	}
	return append(hs, h)
}

// runBatch queues one dirty mark per project after the inserts and runs
// the batch, reading every result.
func runBatch(ctx context.Context, tx pgx.Tx, b *pgx.Batch, markSQL string, hours map[int64][]time.Time) error {
	for pid, hs := range hours {
		b.Queue(markSQL, pid, hs)
	}
	res := tx.SendBatch(ctx, b)
	for range b.Len() {
		if _, err := res.Exec(); err != nil {
			res.Close()
			return err
		}
	}
	return res.Close()
}

// SessionInsert is one row for InsertSessions.
type SessionInsert struct {
	StartedAt   time.Time
	ProjectID   int64
	Sid         string
	Release     string
	Environment *string
	Status      string
	Count       int32
}

// Updates of one session (same sid, same start) overwrite the status,
// except that a terminal status is never downgraded to 'ok'.
const insertSessionSQL = `INSERT INTO sessions (started_at, project_id, sid, release, environment, status, count)
	VALUES ($1, $2, $3, $4, $5, $6, $7)
	ON CONFLICT (project_id, sid, started_at) DO UPDATE SET
	    status = CASE WHEN sessions.status = 'ok' OR EXCLUDED.status <> 'ok' THEN EXCLUDED.status ELSE sessions.status END`

// InsertSessions writes a batch in one round trip (pipelined), marking
// the hours it touched dirty like InsertEvents.
func InsertSessions(ctx context.Context, tx pgx.Tx, rows []SessionInsert) error {
	if len(rows) == 0 {
		return nil
	}
	b := &pgx.Batch{}
	hours := map[int64][]time.Time{}
	for _, r := range rows {
		b.Queue(insertSessionSQL, r.StartedAt, r.ProjectID, r.Sid, r.Release, r.Environment, r.Status, r.Count)
		hours[r.ProjectID] = addHour(hours[r.ProjectID], r.StartedAt)
	}
	return runBatch(ctx, tx, b, markSessionHours, hours)
}

// EventFilter is the optional WHERE of ListEvents. Zero values are ignored.
type EventFilter struct {
	ProjectID   int64
	From, To    time.Time // occurred_at range [From, To); zero = unbounded
	Level       string
	Release     string
	Environment string
	Platform    string
	ErrorType   string
	UserID      string
	DeviceID    string
	DeviceModel string
	OSVersion   string
	Transaction string
	Fingerprint sentry.ID
	Location    string
	Query       string            // message ILIKE %q%
	Tags        map[string]string // tags->>k = v
	Handled     string            // "true" | "false" (exception.mechanism.handled) | "" = any
	Before      Cursor            // keyset cursor: rows after it in newest-first order
	Limit       int
}

// Columns that may be filtered by exact match; the map is the allowlist.
var filterColumns = map[string]string{
	"level": "level", "release": "release", "environment": "environment", "platform": "platform",
	"error_type": "error_type", "user_id": "user_id", "device_id": "device_id", "device_model": "device_model",
	"os_version": "os_version", "transaction": "transaction", "fingerprint": "fingerprint", "culprit": "culprit",
}

func (f EventFilter) where() (string, []any) {
	var w []string
	var args []any
	add := func(cond string, v any) {
		args = append(args, v)
		w = append(w, strings.ReplaceAll(cond, "?", "$"+strconv.Itoa(len(args))))
	}
	add("project_id = ?", f.ProjectID)
	if !f.From.IsZero() {
		add("occurred_at >= ?", f.From)
	}
	if !f.To.IsZero() {
		add("occurred_at < ?", f.To)
	}
	if !f.Before.IsZero() {
		args = append(args, f.Before.At, f.Before.EventID)
		w = append(w, fmt.Sprintf("(occurred_at, event_id) < ($%d, $%d)", len(args)-1, len(args)))
	}
	// A fixed order (and sorted tag keys below): the same filters must
	// produce the same SQL text, or the statement cache misses each time.
	for _, cv := range [...]struct{ col, v string }{
		{"level", f.Level}, {"release", f.Release}, {"environment", f.Environment}, {"platform", f.Platform},
		{"error_type", f.ErrorType}, {"user_id", f.UserID}, {"device_id", f.DeviceID}, {"device_model", f.DeviceModel},
		{"os_version", f.OSVersion}, {"transaction", f.Transaction}, {"culprit", f.Location},
	} {
		if cv.v != "" {
			add(filterColumns[cv.col]+" = ?", cv.v)
		}
	}
	if f.Fingerprint != "" {
		add("fingerprint = ?", f.Fingerprint)
	}
	if q := clip(f.Query, MaxFilterLen); q != "" {
		add("message ILIKE ?", "%"+escapeLike(q)+"%")
	}
	switch f.Handled {
	case "false":
		w = append(w, "handled = false")
	case "true":
		w = append(w, "handled = true")
	}
	for _, k := range slices.Sorted(maps.Keys(f.Tags)) {
		// Containment, so the GIN index (jsonb_path_ops) serves it.
		kv, _ := json.Marshal(map[string]string{k: f.Tags[k]})
		add("tags @> ?::jsonb", kv)
	}
	return strings.Join(w, " AND "), args
}

// clip bounds a filter value (rune-safe).
func clip(s string, n int) string {
	if len(s) <= n {
		return s
	}
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n])
}

func escapeLike(s string) string {
	r := strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`)
	return r.Replace(s)
}

const eventListColumns = `occurred_at, project_id, event_id, level, message, platform, environment, release, device_id,
	device_model, os_version, transaction, error_type, culprit, handled, sdk_name, user_id, fingerprint, symbolicated, tags`

// EventRow is the list projection (no payload / breadcrumbs / symbols).
type EventRow struct {
	OccurredAt   time.Time       `json:"occurred_at"`
	ProjectID    int64           `json:"project_id"`
	EventID      sentry.ID       `json:"event_id"`
	Level        string          `json:"level"`
	Message      string          `json:"message"`
	Platform     *string         `json:"platform"`
	Environment  *string         `json:"environment"`
	Release      *string         `json:"release"`
	DeviceID     *string         `json:"device_id"`
	DeviceModel  *string         `json:"device_model"`
	OSVersion    *string         `json:"os_version"`
	Transaction  *string         `json:"transaction"`
	ErrorType    *string         `json:"error_type"`
	Culprit      *string         `json:"culprit"`
	Handled      *bool           `json:"handled"`
	SDKName      *string         `json:"sdk_name"`
	UserID       *string         `json:"user_id"`
	Fingerprint  *sentry.ID      `json:"fingerprint"`
	Symbolicated bool            `json:"symbolicated"`
	Tags         json.RawMessage `json:"tags"`
}

// ListEvents returns newest-first rows matching f. Limit defaults to 50 and
// caps at 500; one extra row is fetched so callers can tell if more exist.
func (s *Store) ListEvents(ctx context.Context, f EventFilter) (rows []EventRow, more bool, err error) {
	limit := f.Limit
	if limit <= 0 {
		limit = 50
	}
	if limit > 500 {
		limit = 500
	}
	where, args := f.where()
	sql := "SELECT " + eventListColumns + " FROM events WHERE " + where + " ORDER BY occurred_at DESC, event_id DESC LIMIT " + strconv.Itoa(limit+1)
	r, err := s.Pool.Query(ctx, sql, args...)
	if err != nil {
		return nil, false, err
	}
	rows, err = pgx.CollectRows(r, pgx.RowToStructByPos[EventRow])
	if err != nil {
		return nil, false, err
	}
	for i := range rows {
		rows[i].OccurredAt = rows[i].OccurredAt.UTC() // API times are UTC
	}
	if len(rows) > limit {
		rows, more = rows[:limit], true
	}
	return rows, more, nil
}

// CountEvents counts rows matching f (bounded windows only — callers pass From).
func (s *Store) CountEvents(ctx context.Context, f EventFilter) (int64, error) {
	where, args := f.where()
	var n int64
	err := s.Pool.QueryRow(ctx, "SELECT count(*) FROM events WHERE "+where, args...).Scan(&n)
	return n, err
}

// Breakdown is one value of a column with its share.
type Breakdown struct {
	Value string `json:"value"`
	Count int64  `json:"count"`
}

// breakdownColumns is the allowlist for Breakdown.
var breakdownColumns = map[string]bool{
	"release": true, "os_version": true, "device_model": true, "environment": true, "platform": true,
	"transaction": true, "culprit": true, "level": true, "user_id": true, "sdk_name": true,
}

// Breakdown returns the top values of one column among rows matching f.
func (s *Store) Breakdown(ctx context.Context, f EventFilter, column string, limit int) ([]Breakdown, error) {
	m, err := s.Breakdowns(ctx, f, []string{column}, limit)
	if err != nil {
		return nil, err
	}
	return m[column], nil
}

// Breakdowns returns, per column, its top values among rows matching f —
// one scan of the events, unpivoted with a LATERAL VALUES and ranked with
// a window function. Columns are the allowlisted names or "tags.<key>";
// every requested column is present in the result (possibly empty).
func (s *Store) Breakdowns(ctx context.Context, f EventFilter, columns []string, limit int) (map[string][]Breakdown, error) {
	if limit <= 0 {
		limit = 5
	}
	where, args := f.where()
	vals := make([]string, 0, len(columns))
	out := make(map[string][]Breakdown, len(columns))
	for _, column := range columns {
		var expr string
		switch {
		case breakdownColumns[column]:
			expr = column
		case strings.HasPrefix(column, "tags."):
			args = append(args, strings.TrimPrefix(column, "tags."))
			expr = fmt.Sprintf("tags->>$%d", len(args))
		default:
			return nil, fmt.Errorf("breakdown: column %q not allowed", column)
		}
		args = append(args, column)
		vals = append(vals, fmt.Sprintf("($%d::text, COALESCE(%s::text, ''))", len(args), expr)) // ::text — level is an enum
		out[column] = []Breakdown{}
	}
	if len(vals) == 0 {
		return out, nil
	}
	args = append(args, limit)
	sql := fmt.Sprintf(`SELECT col, v, n FROM (
		SELECT x.col, x.v, count(*) AS n, row_number() OVER (PARTITION BY x.col ORDER BY count(*) DESC, x.v) AS rn
		FROM events CROSS JOIN LATERAL (VALUES %s) AS x(col, v)
		WHERE %s GROUP BY x.col, x.v) t
		WHERE rn <= $%d ORDER BY col, n DESC, v`, strings.Join(vals, ", "), where, len(args))
	rows, err := s.Pool.Query(ctx, sql, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var col string
		var b Breakdown
		if err := rows.Scan(&col, &b.Value, &b.Count); err != nil {
			return nil, err
		}
		out[col] = append(out[col], b)
	}
	return out, rows.Err()
}

// EventDetail is the full row.
type EventDetail = sqlc.Event
