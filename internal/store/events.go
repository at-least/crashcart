package store

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/crashcartapp/crashcart/internal/db/sqlc"
)

// EventInsert is one row for InsertEvents.
type EventInsert struct {
	OccurredAt    time.Time
	ProjectID     int64
	EventID       string
	Level         string
	Message       string
	Platform      *string
	Environment   *string
	Release       *string
	DeviceID      *string
	DeviceModel   *string
	OSVersion     *string
	Screen        *string
	ErrorType     *string
	ErrorLocation *string
	Handled       *bool
	SDKName       *string
	UserID        *string
	Fingerprint   *string
	Symbolicated  bool
	Tags          json.RawMessage
	Breadcrumbs   json.RawMessage
	Payload       json.RawMessage
	Symbols       json.RawMessage
}

const insertEventSQL = `INSERT INTO events (occurred_at, project_id, event_id, level, message, platform, environment, release,
	device_id, device_model, os_version, screen, error_type, error_location, handled, sdk_name, user_id,
	fingerprint, symbolicated, tags, breadcrumbs, payload, symbols)
	VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,$19,$20,$21,$22,$23)
	ON CONFLICT (project_id, event_id, occurred_at) DO NOTHING`

// InsertEvents writes a batch in one round trip (pipelined). Duplicate keys
// are skipped, so re-delivery is safe.
func InsertEvents(ctx context.Context, tx pgx.Tx, rows []EventInsert) error {
	if len(rows) == 0 {
		return nil
	}
	b := &pgx.Batch{}
	for _, r := range rows {
		b.Queue(insertEventSQL, r.OccurredAt, r.ProjectID, r.EventID, r.Level, r.Message, r.Platform, r.Environment, r.Release,
			r.DeviceID, r.DeviceModel, r.OSVersion, r.Screen, r.ErrorType, r.ErrorLocation, r.Handled, r.SDKName, r.UserID,
			r.Fingerprint, r.Symbolicated, r.Tags, r.Breadcrumbs, r.Payload, r.Symbols)
	}
	res := tx.SendBatch(ctx, b)
	for range rows {
		if _, err := res.Exec(); err != nil {
			res.Close()
			return err
		}
	}
	return res.Close()
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
	Screen      string
	Fingerprint string
	Location    string
	Query       string            // message ILIKE %q%
	Tags        map[string]string // tags->>k = v
	Crash       bool              // fatal or unhandled only
	Before      Cursor            // keyset cursor: rows after it in newest-first order
	Limit       int
}

// Columns that may be filtered by exact match; the map is the allowlist.
var filterColumns = map[string]string{
	"level": "level", "release": "release", "environment": "environment", "platform": "platform",
	"error_type": "error_type", "user_id": "user_id", "device_id": "device_id", "device_model": "device_model",
	"os_version": "os_version", "screen": "screen", "fingerprint": "fingerprint", "error_location": "error_location",
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
	for col, v := range map[string]string{
		"level": f.Level, "release": f.Release, "environment": f.Environment, "platform": f.Platform,
		"error_type": f.ErrorType, "user_id": f.UserID, "device_id": f.DeviceID, "device_model": f.DeviceModel,
		"os_version": f.OSVersion, "screen": f.Screen, "fingerprint": f.Fingerprint, "error_location": f.Location,
	} {
		if v != "" {
			add(filterColumns[col]+" = ?", v)
		}
	}
	if f.Query != "" {
		add("message ILIKE ?", "%"+escapeLike(f.Query)+"%")
	}
	if f.Crash {
		w = append(w, "crashcart_is_crash(level, handled)")
	}
	for k, v := range f.Tags {
		args = append(args, k, v)
		w = append(w, fmt.Sprintf("tags->>$%d = $%d", len(args)-1, len(args)))
	}
	return strings.Join(w, " AND "), args
}

func escapeLike(s string) string {
	r := strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`)
	return r.Replace(s)
}

const eventListColumns = `occurred_at, project_id, event_id, level, message, platform, environment, release, device_id,
	device_model, os_version, screen, error_type, error_location, handled, sdk_name, user_id, fingerprint, symbolicated, tags`

// EventRow is the list projection (no payload / breadcrumbs / symbols).
type EventRow struct {
	OccurredAt    time.Time       `json:"occurred_at"`
	ProjectID     int64           `json:"project_id"`
	EventID       string          `json:"event_id"`
	Level         string          `json:"level"`
	Message       string          `json:"message"`
	Platform      *string         `json:"platform"`
	Environment   *string         `json:"environment"`
	Release       *string         `json:"release"`
	DeviceID      *string         `json:"device_id"`
	DeviceModel   *string         `json:"device_model"`
	OSVersion     *string         `json:"os_version"`
	Screen        *string         `json:"screen"`
	ErrorType     *string         `json:"error_type"`
	ErrorLocation *string         `json:"error_location"`
	Handled       *bool           `json:"handled"`
	SDKName       *string         `json:"sdk_name"`
	UserID        *string         `json:"user_id"`
	Fingerprint   *string         `json:"fingerprint"`
	Symbolicated  bool            `json:"symbolicated"`
	Tags          json.RawMessage `json:"tags"`
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
	"screen": true, "error_location": true, "level": true, "user_id": true, "sdk_name": true,
}

// Breakdown returns the top values of column among rows matching f.
func (s *Store) Breakdown(ctx context.Context, f EventFilter, column string, limit int) ([]Breakdown, error) {
	var expr string
	switch {
	case breakdownColumns[column]:
		expr = column
	case strings.HasPrefix(column, "tags."):
		key := strings.TrimPrefix(column, "tags.")
		expr = "tags->>" + quoteLiteral(key)
	default:
		return nil, fmt.Errorf("breakdown: column %q not allowed", column)
	}
	if limit <= 0 {
		limit = 5
	}
	where, args := f.where()
	sql := fmt.Sprintf("SELECT COALESCE(%s, '') AS v, count(*) FROM events WHERE %s GROUP BY 1 ORDER BY 2 DESC LIMIT %d", expr, where, limit)
	r, err := s.Pool.Query(ctx, sql, args...)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(r, pgx.RowToStructByPos[Breakdown])
}

func quoteLiteral(s string) string { return "'" + strings.ReplaceAll(s, "'", "''") + "'" }

// EventDetail is the full row.
type EventDetail = sqlc.Event
