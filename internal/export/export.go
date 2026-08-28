// Package export streams every table as newline-delimited JSON in a
// storage-neutral encoding, so the data can be loaded into another
// CrashCart (Postgres or a future SQLite/D1 edition) without touching
// pg_dump. It doubles as the backup format.
//
// One object per line, `t` names the table, the other keys are the
// columns in snake_case:
//
//	{"t":"events","id":1787998530123456,"level":"error","tags":{"build":"42"},...}
//
// Encoding rules (chosen to be trivial on SQLite):
//   - TIMESTAMPTZ → integer unix milliseconds; DATE → "YYYY-MM-DD"
//   - JSONB → embedded JSON value (object/array), never a string
//   - BOOLEAN → JSON true/false; BYTEA → base64 string
//   - rows come in primary-key order; aggregate tables are included so an
//     importer never has to recompute them from events (each recompute is
//     ~0.5 extra writes per event on a per-row-billed store)
package export

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Tables lists what is exported, with the ORDER BY that makes output
// deterministic. The only hand-written SQL outside internal/db/queries:
// a full-table scan with generic column decoding doesn't fit sqlc.
var Tables = []struct{ Name, Order string }{
	{"events", "id"},
	{"user_devices", "user_id, device_id"},
	{"hourly_stats", "hour, level"},
	{"issues", "fingerprint"},
	{"releases", "version"},
	{"release_health", "release, day"},
	{"alert_types", "type"},
	{"symbol_files", "platform, release, filename"},
}

// All writes every table to w.
func All(ctx context.Context, pool *pgxpool.Pool, w io.Writer) error {
	for _, t := range Tables {
		if err := Table(ctx, pool, w, t.Name, t.Order); err != nil {
			return fmt.Errorf("export %s: %w", t.Name, err)
		}
	}
	return nil
}

// Table streams one table.
func Table(ctx context.Context, pool *pgxpool.Pool, w io.Writer, name, order string) error {
	rows, err := pool.Query(ctx, "SELECT * FROM "+name+" ORDER BY "+order)
	if err != nil {
		return err
	}
	defer rows.Close()
	enc := json.NewEncoder(w)
	fields := rows.FieldDescriptions()
	for rows.Next() {
		vals, err := rows.Values()
		if err != nil {
			return err
		}
		rec := make(map[string]any, len(fields)+1)
		rec["t"] = name
		for i, f := range fields {
			rec[f.Name] = neutral(vals[i])
		}
		if err := enc.Encode(rec); err != nil {
			return err
		}
	}
	return rows.Err()
}

// neutral converts a pgx-decoded value to the interchange encoding.
func neutral(v any) any {
	switch x := v.(type) {
	case time.Time:
		return x.UnixMilli()
	case pgtype.Date:
		if !x.Valid {
			return nil
		}
		return x.Time.Format("2006-01-02")
	case pgtype.Timestamptz:
		if !x.Valid {
			return nil
		}
		return x.Time.UnixMilli()
	case pgtype.Numeric:
		f, _ := x.Float64Value()
		return f.Float64
	default:
		return v // int64, string, bool, []byte (base64), map/[]any from jsonb, nil
	}
}
