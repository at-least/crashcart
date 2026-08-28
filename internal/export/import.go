package export

import (
	"bufio"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Import loads NDJSON produced by All (from this or another CrashCart
// edition) into the database. Events are COPYed in batches; every other
// table is upserted on its primary key so importing twice, or on top of
// existing data, is safe. Aggregate rows are taken as-is — never
// recomputed from events.
type Import struct {
	Rows     map[string]int64 // per table
	Skipped  int64            // lines with an unknown table
	Conflict int64            // events whose id already existed
}

const eventBatch = 1000

// Load reads NDJSON from r.
func Load(ctx context.Context, pool *pgxpool.Pool, r io.Reader) (Import, error) {
	rep := Import{Rows: map[string]int64{}}
	cols, err := columns(ctx, pool)
	if err != nil {
		return rep, err
	}
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 1<<20), 64<<20)
	var events [][]any
	flush := func() error {
		if len(events) == 0 {
			return nil
		}
		n, conflicts, err := copyEvents(ctx, pool, cols["events"], events)
		rep.Rows["events"] += n
		rep.Conflict += conflicts
		events = events[:0]
		return err
	}
	for sc.Scan() {
		line := sc.Bytes()
		if len(strings.TrimSpace(string(line))) == 0 {
			continue
		}
		var rec map[string]any
		if err := json.Unmarshal(line, &rec); err != nil {
			return rep, fmt.Errorf("bad line: %w", err)
		}
		table, _ := rec["t"].(string)
		c, ok := cols[table]
		if !ok {
			rep.Skipped++
			continue
		}
		delete(rec, "t")
		vals := make([]any, len(c))
		for i, col := range c {
			vals[i] = fromNeutral(col, rec[col.name])
		}
		if table == "events" {
			events = append(events, vals)
			if len(events) >= eventBatch {
				if err := flush(); err != nil {
					return rep, err
				}
			}
			continue
		}
		if err := upsert(ctx, pool, table, c, vals); err != nil {
			return rep, fmt.Errorf("%s: %w", table, err)
		}
		rep.Rows[table]++
	}
	if err := sc.Err(); err != nil {
		return rep, err
	}
	return rep, flush()
}

type column struct {
	name, typ string
}

// columns reads each exported table's column list + types from the catalog
// (so the importer never hard-codes the schema).
func columns(ctx context.Context, pool *pgxpool.Pool) (map[string][]column, error) {
	out := map[string][]column{}
	for _, t := range Tables {
		rows, err := pool.Query(ctx, `SELECT column_name, data_type FROM information_schema.columns
			WHERE table_schema = current_schema() AND table_name = $1 ORDER BY ordinal_position`, t.Name)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var c column
			if err := rows.Scan(&c.name, &c.typ); err != nil {
				rows.Close()
				return nil, err
			}
			out[t.Name] = append(out[t.Name], c)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// fromNeutral reverses neutral(): unix ms → time, "YYYY-MM-DD" → date,
// embedded JSON → text for jsonb, base64 → bytes.
func fromNeutral(c column, v any) any {
	if v == nil {
		return nil
	}
	switch c.typ {
	case "timestamp with time zone":
		if f, ok := v.(float64); ok {
			return time.UnixMilli(int64(f)).UTC()
		}
	case "date":
		if s, ok := v.(string); ok {
			if t, err := time.Parse("2006-01-02", s); err == nil {
				return t
			}
		}
	case "jsonb":
		b, _ := json.Marshal(v)
		return string(b)
	case "bytea":
		if s, ok := v.(string); ok {
			if b, err := base64.StdEncoding.DecodeString(s); err == nil {
				return b
			}
		}
	case "bigint", "integer":
		if f, ok := v.(float64); ok {
			return int64(f)
		}
	}
	return v
}

// copyEvents COPYs a batch; on a duplicate id it falls back to row-by-row
// inserts that skip existing ids (re-importing a dump is idempotent).
func copyEvents(ctx context.Context, pool *pgxpool.Pool, cols []column, rows [][]any) (int64, int64, error) {
	names := make([]string, len(cols))
	for i, c := range cols {
		names[i] = c.name
	}
	n, err := pool.CopyFrom(ctx, pgx.Identifier{"events"}, names, pgx.CopyFromRows(rows))
	if err == nil {
		return n, 0, nil
	}
	if !strings.Contains(err.Error(), "23505") && !strings.Contains(err.Error(), "duplicate key") {
		return 0, 0, err
	}
	placeholders := make([]string, len(cols))
	for i := range cols {
		placeholders[i] = fmt.Sprintf("$%d", i+1)
	}
	stmt := "INSERT INTO events (" + strings.Join(names, ", ") + ") VALUES (" + strings.Join(placeholders, ", ") + ") ON CONFLICT (id) DO NOTHING"
	var inserted, conflicts int64
	for _, r := range rows {
		tag, err := pool.Exec(ctx, stmt, r...)
		if err != nil {
			return inserted, conflicts, err
		}
		if tag.RowsAffected() == 0 {
			conflicts++
		} else {
			inserted++
		}
	}
	return inserted, conflicts, nil
}

// primaryKeys of the non-events tables (for ON CONFLICT).
var primaryKeys = map[string][]string{
	"user_devices":   {"user_id", "device_id"},
	"hourly_stats":   {"hour", "level"},
	"issues":         {"fingerprint"},
	"releases":       {"version"},
	"release_health": {"release", "day"},
	"alert_types":    {"type"},
	"symbol_files":   {"platform", "release", "filename"},
}

func upsert(ctx context.Context, pool *pgxpool.Pool, table string, cols []column, vals []any) error {
	pkCols := primaryKeys[table]
	isPK := map[string]bool{}
	for _, k := range pkCols {
		isPK[k] = true
	}
	names := make([]string, len(cols))
	placeholders := make([]string, len(cols))
	var sets []string
	for i, c := range cols {
		names[i] = c.name
		placeholders[i] = fmt.Sprintf("$%d", i+1)
		if !isPK[c.name] {
			sets = append(sets, c.name+" = EXCLUDED."+c.name)
		}
	}
	stmt := "INSERT INTO " + table + " (" + strings.Join(names, ", ") + ") VALUES (" + strings.Join(placeholders, ", ") + ") ON CONFLICT (" + strings.Join(pkCols, ", ") + ") DO UPDATE SET " + strings.Join(sets, ", ")
	_, err := pool.Exec(ctx, stmt, vals...)
	return err
}
