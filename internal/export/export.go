// Package export streams every table as NDJSON and loads it back.
//
// The format is specified in docs/reference/export-format.md; change it
// first.
//
// Format (one JSON object per line):
//
//	{"t":"_meta","format":1,"exported_at":"<RFC3339>","app":"crashcart"}
//	{"t":"projects", ...}   then issues, events, sessions, symbol_files,
//	                        alert_rules, alert_channels (see Tables)
//
// Rows refer to their project by "project": "<slug>" — never by id, so a
// dump loads into any database. Events and sessions carry their natural
// keys (event_id + occurred_at, sid + started_at); identity ids (projects,
// symbol_files, alert_channels) are not exported because their natural
// keys are. TIMESTAMPTZ columns are RFC3339 strings (UTC, nanosecond
// precision), JSONB columns are embedded JSON, BYTEA is base64, NULL
// columns are omitted. Aggregates, jobs and rate limits are not exported:
// they recompute or expire.
//
// Import is idempotent: events and sessions are inserted with
// ON CONFLICT DO NOTHING, everything else is upserted on its natural key
// (issue counts are replaced, not added), alert channels are inserted only
// when no identical (project, kind, config) row exists.
package export

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"slices"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/crashcartapp/crashcart/internal/auth"
	"github.com/crashcartapp/crashcart/internal/db/sqlc"
	"github.com/crashcartapp/crashcart/internal/sentry"
	"github.com/crashcartapp/crashcart/internal/store"
)

// Format is the NDJSON format version written in the _meta line.
const Format = 3 // 3: no triaged status; 2: transaction / culprit / unhandled_spike (was screen / error_location / crash_spike)

// Tables lists the exported tables in the order they are written (and the
// order import expects: projects first so later rows can reference them).
var Tables = []string{"users", "api_keys", "projects", "releases", "issues", "events", "sessions", "symbol_files", "alert_rules", "alert_channels"}

// Options narrows an export.
type Options struct {
	Project string // slug; "" = all
}

// maxLine is the longest NDJSON line import accepts: a symbol file is at
// most symbolicate.MaxUpload (50 MB) → ~67 MB of base64 on one line; an
// event payload is at most a 20 MB envelope.
const maxLine = 96 << 20

// batchSize is how many events / sessions / upserts go in one round trip.
const batchSize = 500

// ── row shapes ─────────────────────────────────────────────────────────

type metaRow struct {
	T          string    `json:"t"`
	Format     int       `json:"format"`
	ExportedAt time.Time `json:"exported_at"`
	App        string    `json:"app"`
}

// users and api_keys are global (not per project) and are written only
// by a full export: an instance moved with export / import keeps its
// accounts and its keys (sentry-cli, scripts) working.
type userRow struct {
	T            string `json:"t"`
	Email        string `json:"email"`
	Name         string `json:"name"`
	PasswordHash string `json:"password_hash"` // bcrypt
	CreatedAt    ts     `json:"created_at"`
}

type apiKeyRow struct {
	T          string  `json:"t"`
	Name       string  `json:"name"`
	KeyHash    []byte  `json:"key_hash"` // base64 of the sha256
	Prefix     string  `json:"prefix"`
	CreatedBy  *string `json:"created_by,omitempty"` // user email
	CreatedAt  ts      `json:"created_at"`
	LastUsedAt *ts     `json:"last_used_at,omitempty"`
	RevokedAt  *ts     `json:"revoked_at,omitempty"`
}

type projectRow struct {
	T               string  `json:"t"`
	Slug            string  `json:"slug"`
	Name            string  `json:"name"`
	Platform        *string `json:"platform,omitempty"`
	PublicKey       string  `json:"public_key"`
	SampleKeepFirst int32   `json:"sample_keep_first"`
	SampleRate      float64 `json:"sample_rate"`
	DailyQuota      *int32  `json:"daily_quota,omitempty"`
	CreatedAt       ts      `json:"created_at"`
}

type releaseRow struct {
	T         string   `json:"t"`
	Project   string   `json:"project"`
	Release   string   `json:"release"`
	Platforms []string `json:"platforms"`
	FirstSeen ts       `json:"first_seen"`
}

type issueRow struct {
	T                string   `json:"t"`
	Project          string   `json:"project"`
	Fingerprint      string   `json:"fingerprint"`
	Title            string   `json:"title"`
	Level            string   `json:"level"`
	ErrorType        *string  `json:"error_type,omitempty"`
	Transaction      *string  `json:"transaction,omitempty"`
	Screen           *string  `json:"screen,omitempty"` // format 1 name of transaction (read only)
	Platform         *string  `json:"platform,omitempty"`
	Status           string   `json:"status"`
	StatusBy         *string  `json:"status_by,omitempty"`
	EventCount       int64    `json:"event_count"`
	StoredCount      int64    `json:"stored_count"`
	FirstSeen        ts       `json:"first_seen"`
	LastSeen         ts       `json:"last_seen"`
	FirstRelease     *string  `json:"first_release,omitempty"`
	LastRelease      *string  `json:"last_release,omitempty"`
	Releases         []string `json:"releases,omitempty"`
	ResolvedReleases []string `json:"resolved_releases,omitempty"`
	CreatedAt        ts       `json:"created_at"`
	UpdatedAt        ts       `json:"updated_at"`
}

type eventRow struct {
	T             string          `json:"t"`
	Project       string          `json:"project"`
	OccurredAt    ts              `json:"occurred_at"`
	EventID       string          `json:"event_id"`
	Level         string          `json:"level"`
	Message       string          `json:"message"`
	Platform      *string         `json:"platform,omitempty"`
	Environment   *string         `json:"environment,omitempty"`
	Release       *string         `json:"release,omitempty"`
	DeviceID      *string         `json:"device_id,omitempty"`
	DeviceModel   *string         `json:"device_model,omitempty"`
	OSVersion     *string         `json:"os_version,omitempty"`
	Transaction   *string         `json:"transaction,omitempty"`
	Screen        *string         `json:"screen,omitempty"` // format 1 name of transaction (read only)
	ErrorType     *string         `json:"error_type,omitempty"`
	Culprit       *string         `json:"culprit,omitempty"`
	ErrorLocation *string         `json:"error_location,omitempty"` // format 1 name of culprit (read only)
	Handled       *bool           `json:"handled,omitempty"`
	SDKName       *string         `json:"sdk_name,omitempty"`
	UserID        *string         `json:"user_id,omitempty"`
	Fingerprint   *string         `json:"fingerprint,omitempty"`
	Symbolicated  bool            `json:"symbolicated"`
	Tags          json.RawMessage `json:"tags"`
	Payload       json.RawMessage `json:"payload,omitempty"`
	Symbols       json.RawMessage `json:"symbols,omitempty"`
}

type sessionRow struct {
	T           string  `json:"t"`
	Project     string  `json:"project"`
	StartedAt   ts      `json:"started_at"`
	SID         string  `json:"sid"`
	Release     string  `json:"release"`
	Environment *string `json:"environment,omitempty"`
	Status      string  `json:"status"`
	Count       int32   `json:"count"`
}

type symbolFileRow struct {
	T          string  `json:"t"`
	Project    string  `json:"project"`
	Kind       string  `json:"kind"`
	Release    string  `json:"release"`
	DebugID    *string `json:"debug_id,omitempty"`
	Filename   string  `json:"filename"`
	Size       int64   `json:"size"`
	Data       []byte  `json:"data"` // base64
	UploadedAt ts      `json:"uploaded_at"`
}

type alertRuleRow struct {
	T               string `json:"t"`
	Project         string `json:"project"`
	Type            string `json:"type"`
	Enabled         bool   `json:"enabled"`
	CooldownMinutes int32  `json:"cooldown_minutes"`
	LastTriggered   *ts    `json:"last_triggered,omitempty"`
}

type alertChannelRow struct {
	T         string          `json:"t"`
	Project   string          `json:"project"`
	Kind      string          `json:"kind"`
	Config    json.RawMessage `json:"config"`
	CreatedAt ts              `json:"created_at"`
}

// ts is a TIMESTAMPTZ in the file: RFC3339 with nanoseconds, UTC. A zero
// value is omitted-or-missing and becomes "now" on import (tsOrNow).
type ts struct{ time.Time }

func (t ts) MarshalJSON() ([]byte, error) {
	return json.Marshal(t.UTC().Format(time.RFC3339Nano))
}

func (t *ts) UnmarshalJSON(b []byte) error {
	var s string
	if err := json.Unmarshal(b, &s); err != nil {
		return err
	}
	if s == "" {
		t.Time = time.Time{}
		return nil
	}
	v, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		return err
	}
	t.Time = v.UTC()
	return nil
}

func at(t time.Time) ts { return ts{t.UTC()} }

// ── export ─────────────────────────────────────────────────────────────

const (
	selectIssues = `SELECT project_id, fingerprint, title, level, error_type, transaction, platform, status, status_by, event_count, stored_count,
	first_seen, last_seen, first_release, last_release, releases, resolved_releases, created_at, updated_at
	FROM issues WHERE project_id = $1 ORDER BY fingerprint`
	selectEvents = `SELECT occurred_at, project_id, event_id, level, message, platform, environment, release, device_id, device_model,
	os_version, transaction, error_type, culprit, handled, sdk_name, user_id, fingerprint, symbolicated, tags,
	symbols, payload FROM events WHERE project_id = $1 ORDER BY occurred_at, event_id`
	selectSessions    = `SELECT started_at, project_id, sid, release, environment, status, count FROM sessions WHERE project_id = $1 ORDER BY started_at, sid`
	selectReleases    = `SELECT project_id, release, platforms, first_seen FROM releases WHERE project_id = $1 ORDER BY release`
	selectSymbolFiles = `SELECT id, project_id, kind, release, debug_id, filename, size, data, uploaded_at
	FROM symbol_files WHERE project_id = $1 ORDER BY kind, release, filename`
	selectAlertRules    = `SELECT project_id, type, enabled, cooldown_minutes, last_triggered FROM alert_rules WHERE project_id = $1 ORDER BY type`
	selectAlertChannels = `SELECT id, project_id, kind, config, created_at FROM alert_channels WHERE project_id = $1 ORDER BY id`
	selectUsers         = `SELECT email, name, password_hash, created_at FROM users ORDER BY email`
	selectAPIKeys       = `SELECT k.name, k.key_hash, k.prefix, u.email, k.created_at, k.last_used_at, k.revoked_at
	FROM api_keys k LEFT JOIN users u ON u.id = k.created_by ORDER BY k.id`
)

// Export writes NDJSON to w. Tables are streamed row by row per project
// (projects in slug order), never loaded into memory.
func Export(ctx context.Context, st *store.Store, w io.Writer, opt Options) error {
	bw := bufio.NewWriterSize(w, 256<<10)
	enc := json.NewEncoder(bw)
	enc.SetEscapeHTML(false)

	// One repeatable-read snapshot for every table: an ingest committing
	// while the export runs cannot leave the file with events whose issue
	// or release row is missing.
	tx, err := st.Pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return err
	}
	defer tx.Rollback(context.WithoutCancel(ctx))

	projects, err := exportProjects(ctx, tx, opt)
	if err != nil {
		return err
	}
	if err := enc.Encode(metaRow{T: "_meta", Format: Format, ExportedAt: time.Now().UTC(), App: "crashcart"}); err != nil {
		return err
	}
	if opt.Project == "" {
		if err := exportAccounts(ctx, tx, enc); err != nil {
			return err
		}
	}
	for _, p := range projects {
		if err := enc.Encode(projectRow{
			T: "projects", Slug: p.Slug, Name: p.Name, Platform: p.Platform, PublicKey: p.PublicKey,
			SampleKeepFirst: p.SampleKeepFirst, SampleRate: p.SampleRate, DailyQuota: &p.DailyQuota, CreatedAt: at(p.CreatedAt),
		}); err != nil {
			return err
		}
	}
	// Per table, per project: the order of Tables is the order of the file.
	for _, p := range projects {
		if err := stream(ctx, tx, selectReleases, func(r sqlc.Release) error {
			return enc.Encode(releaseRow{T: "releases", Project: p.Slug, Release: r.Release, Platforms: r.Platforms, FirstSeen: at(r.FirstSeen)})
		}, p.ID); err != nil {
			return fmt.Errorf("export releases: %w", err)
		}
	}
	for _, p := range projects {
		if err := stream(ctx, tx, selectIssues, func(r sqlc.Issue) error {
			return enc.Encode(issueRow{
				T: "issues", Project: p.Slug, Fingerprint: string(r.Fingerprint), Title: r.Title, Level: string(r.Level),
				ErrorType: r.ErrorType, Transaction: r.Transaction, Platform: r.Platform, Status: string(r.Status), StatusBy: r.StatusBy,
				EventCount: r.EventCount, StoredCount: r.StoredCount, FirstSeen: at(r.FirstSeen), LastSeen: at(r.LastSeen),
				FirstRelease: r.FirstRelease, LastRelease: r.LastRelease, Releases: r.Releases, ResolvedReleases: r.ResolvedReleases,
				CreatedAt: at(r.CreatedAt), UpdatedAt: at(r.UpdatedAt),
			})
		}, p.ID); err != nil {
			return fmt.Errorf("export issues: %w", err)
		}
	}
	for _, p := range projects {
		if err := stream(ctx, tx, selectEvents, func(r sqlc.Event) error {
			payload, err := store.Payload(r)
			if err != nil {
				return fmt.Errorf("payload %s: %w", r.EventID, err)
			}
			return enc.Encode(eventRow{
				T: "events", Project: p.Slug, OccurredAt: at(r.OccurredAt), EventID: string(r.EventID), Level: string(r.Level), Message: r.Message,
				Platform: r.Platform, Environment: r.Environment, Release: r.Release, DeviceID: r.DeviceID,
				DeviceModel: r.DeviceModel, OSVersion: r.OsVersion, Transaction: r.Transaction, ErrorType: r.ErrorType,
				Culprit: r.Culprit, Handled: r.Handled, SDKName: r.SdkName, UserID: r.UserID,
				Fingerprint: idStr(r.Fingerprint), Symbolicated: r.Symbolicated, Tags: r.Tags,
				Payload: json.RawMessage(payload), Symbols: r.Symbols,
			})
		}, p.ID); err != nil {
			return fmt.Errorf("export events: %w", err)
		}
	}
	for _, p := range projects {
		if err := stream(ctx, tx, selectSessions, func(r sqlc.Session) error {
			return enc.Encode(sessionRow{T: "sessions", Project: p.Slug, StartedAt: at(r.StartedAt), SID: r.Sid, Release: r.Release, Environment: r.Environment, Status: string(r.Status), Count: r.Count})
		}, p.ID); err != nil {
			return fmt.Errorf("export sessions: %w", err)
		}
	}
	for _, p := range projects {
		if err := stream(ctx, tx, selectSymbolFiles, func(r sqlc.SymbolFile) error {
			return enc.Encode(symbolFileRow{
				T: "symbol_files", Project: p.Slug, Kind: string(r.Kind), Release: strOr(r.Release), DebugID: r.DebugID,
				Filename: r.Filename, Size: r.Size, Data: r.Data, UploadedAt: at(r.UploadedAt),
			})
		}, p.ID); err != nil {
			return fmt.Errorf("export symbol_files: %w", err)
		}
	}
	for _, p := range projects {
		if err := stream(ctx, tx, selectAlertRules, func(r sqlc.AlertRule) error {
			var lt *ts
			if r.LastTriggered != nil {
				v := at(*r.LastTriggered)
				lt = &v
			}
			return enc.Encode(alertRuleRow{T: "alert_rules", Project: p.Slug, Type: string(r.Type), Enabled: r.Enabled, CooldownMinutes: r.CooldownMinutes, LastTriggered: lt})
		}, p.ID); err != nil {
			return fmt.Errorf("export alert_rules: %w", err)
		}
	}
	for _, p := range projects {
		if err := stream(ctx, tx, selectAlertChannels, func(r sqlc.AlertChannel) error {
			return enc.Encode(alertChannelRow{T: "alert_channels", Project: p.Slug, Kind: string(r.Kind), Config: r.Config, CreatedAt: at(r.CreatedAt)})
		}, p.ID); err != nil {
			return fmt.Errorf("export alert_channels: %w", err)
		}
	}
	return bw.Flush()
}

func exportProjects(ctx context.Context, tx pgx.Tx, opt Options) ([]sqlc.Project, error) {
	if opt.Project != "" {
		p, err := sqlc.New(tx).GetProject(ctx, opt.Project)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, fmt.Errorf("project %q not found", opt.Project)
		}
		return []sqlc.Project{p}, err
	}
	r, err := tx.Query(ctx, "SELECT id, slug, name, platform, public_key, sample_keep_first, sample_rate, daily_quota, created_at FROM projects ORDER BY slug")
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(r, pgx.RowToStructByPos[sqlc.Project])
}

// exportAccounts writes the users and API keys (full exports only).
func exportAccounts(ctx context.Context, tx pgx.Tx, enc *json.Encoder) error {
	type user struct {
		Email, Name, PasswordHash string
		CreatedAt                 time.Time
	}
	if err := stream(ctx, tx, selectUsers, func(u user) error {
		return enc.Encode(userRow{T: "users", Email: u.Email, Name: u.Name, PasswordHash: u.PasswordHash, CreatedAt: at(u.CreatedAt)})
	}); err != nil {
		return fmt.Errorf("export users: %w", err)
	}
	type key struct {
		Name      string
		KeyHash   []byte
		Prefix    string
		CreatedBy *string
		CreatedAt time.Time
		LastUsed  *time.Time
		Revoked   *time.Time
	}
	if err := stream(ctx, tx, selectAPIKeys, func(k key) error {
		r := apiKeyRow{T: "api_keys", Name: k.Name, KeyHash: k.KeyHash, Prefix: k.Prefix, CreatedBy: k.CreatedBy, CreatedAt: at(k.CreatedAt)}
		if k.LastUsed != nil {
			v := at(*k.LastUsed)
			r.LastUsedAt = &v
		}
		if k.Revoked != nil {
			v := at(*k.Revoked)
			r.RevokedAt = &v
		}
		return enc.Encode(r)
	}); err != nil {
		return fmt.Errorf("export api_keys: %w", err)
	}
	return nil
}

// querier is what stream reads from: the export's snapshot transaction.
type querier interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
}

// stream runs sql and hands each row (scanned by position into T) to fn,
// one at a time: nothing is loaded whole.
func stream[T any](ctx context.Context, q querier, sql string, fn func(T) error, args ...any) error {
	rows, err := q.Query(ctx, sql, args...)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		r, err := pgx.RowToStructByPos[T](rows)
		if err != nil {
			return err
		}
		if err := fn(r); err != nil {
			return err
		}
	}
	return rows.Err()
}

// ── import ─────────────────────────────────────────────────────────────

// Report summarizes an import: rows read per table, plus "skipped" for
// lines whose "t" is unknown.
type Report struct {
	Committed int              `json:"committed_lines"` // lines committed (all of them on success)
	Rows      map[string]int64 `json:"rows"`
}

const (
	// Accounts: an existing user (by email) or key (by hash) is kept as is.
	insertUser = `INSERT INTO users (email, name, password_hash, created_at) VALUES ($1, $2, $3, $4)
	ON CONFLICT (email) DO NOTHING`
	insertAPIKey = `INSERT INTO api_keys (name, key_hash, prefix, created_by, created_at, last_used_at, revoked_at)
	VALUES ($1, $2, $3, (SELECT id FROM users WHERE email = $4), $5, $6, $7)
	ON CONFLICT (key_hash) DO NOTHING`
	upsertProject = `INSERT INTO projects (slug, name, platform, public_key, sample_keep_first, sample_rate, daily_quota, created_at)
	VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
	ON CONFLICT (slug) DO UPDATE SET name = EXCLUDED.name, platform = EXCLUDED.platform,
	    sample_keep_first = EXCLUDED.sample_keep_first, sample_rate = EXCLUDED.sample_rate,
	    daily_quota = EXCLUDED.daily_quota, created_at = EXCLUDED.created_at
	RETURNING id`
	upsertIssue = `INSERT INTO issues (project_id, fingerprint, title, level, error_type, transaction, platform, status, status_by, event_count,
	stored_count, first_seen, last_seen, first_release, last_release, releases, resolved_releases, created_at, updated_at)
	VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,$19)
	ON CONFLICT (project_id, fingerprint) DO UPDATE SET title = EXCLUDED.title, level = EXCLUDED.level, status_by = EXCLUDED.status_by,
	    error_type = EXCLUDED.error_type, transaction = EXCLUDED.transaction, platform = EXCLUDED.platform, status = EXCLUDED.status,
	    event_count = GREATEST(issues.event_count, EXCLUDED.event_count), stored_count = GREATEST(issues.stored_count, EXCLUDED.stored_count), first_seen = EXCLUDED.first_seen,
	    last_seen = EXCLUDED.last_seen, first_release = EXCLUDED.first_release, last_release = EXCLUDED.last_release,
	    releases = EXCLUDED.releases, resolved_releases = EXCLUDED.resolved_releases, created_at = EXCLUDED.created_at, updated_at = EXCLUDED.updated_at`
	upsertRelease = `INSERT INTO releases (project_id, release, platforms, first_seen) VALUES ($1,$2,$3,$4)
	ON CONFLICT (project_id, release) DO UPDATE SET
	    platforms = (SELECT array_agg(DISTINCT x ORDER BY x) FROM unnest(releases.platforms || EXCLUDED.platforms) AS x),
	    first_seen = LEAST(releases.first_seen, EXCLUDED.first_seen)`
	insertSession = `INSERT INTO sessions (started_at, project_id, sid, release, environment, status, count) VALUES ($1,$2,$3,$4,$5,$6,$7)
	ON CONFLICT (project_id, sid, started_at) DO NOTHING`
	upsertSymbolFile = `INSERT INTO symbol_files (project_id, kind, release, debug_id, filename, size, data, uploaded_at)
	VALUES ($1,$2,$3,$4,$5,$6,$7,$8)
	ON CONFLICT (project_id, kind, release, filename) DO UPDATE SET debug_id = EXCLUDED.debug_id, size = EXCLUDED.size,
	    data = EXCLUDED.data, uploaded_at = EXCLUDED.uploaded_at`
	upsertAlertRule = `INSERT INTO alert_rules (project_id, type, enabled, cooldown_minutes, last_triggered) VALUES ($1,$2,$3,$4,$5)
	ON CONFLICT (project_id, type) DO UPDATE SET enabled = EXCLUDED.enabled, cooldown_minutes = EXCLUDED.cooldown_minutes,
	    last_triggered = EXCLUDED.last_triggered`
	insertAlertChannel = `INSERT INTO alert_channels (project_id, kind, config, created_at)
	SELECT $1, $2, $3::jsonb, $4
	WHERE NOT EXISTS (SELECT 1 FROM alert_channels WHERE project_id = $1 AND kind = $2 AND config = $3::jsonb)`
)

// importer carries the state of one Import call. Everything is written
// through one transaction, so a failed import leaves the database as it was.
type importer struct {
	ctx      context.Context
	st       *store.Store
	tx       pgx.Tx
	q        *sqlc.Queries
	projects map[string]int64 // slug → id
	events   []store.EventInsert
	batch    *pgx.Batch                   // sessions / issues / alert rows
	dirtyS   map[int64]map[time.Time]bool // project → hours of sessions written (stats rollup)
	report   Report
}

// CommitEvery is how many lines an import writes per transaction: a
// dump of a month at the target rate is tens of millions of rows, and one
// transaction over all of them is a snapshot, WAL and dead-tuple pile the
// database has to carry to the end. Import is idempotent, so a failed run
// is re-run, not rolled back; the error names the first uncommitted line.
var CommitEvery = 20000

// Import loads NDJSON from r, committing every CommitEvery lines. Rows
// referencing a project slug that does not exist create it (name = slug,
// fresh public key). The hours written are marked dirty at each commit,
// so the stats are exact as soon as the rows are and rolled up by the
// next rollup run.
func Import(ctx context.Context, st *store.Store, r io.Reader) (Report, error) {
	im := &importer{ctx: ctx, st: st, projects: map[string]int64{}, batch: &pgx.Batch{},
		dirtyS: map[int64]map[time.Time]bool{}, report: Report{Rows: map[string]int64{}}}
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 1<<20), maxLine)
	line, committed := 0, 0
	for {
		// One transaction per chunk of lines.
		more := true
		err := st.Tx(ctx, func(ctx context.Context, tx pgx.Tx, q *sqlc.Queries) error {
			im.tx, im.q = tx, q
			for n := 0; n < CommitEvery; {
				if !sc.Scan() {
					more = false
					break
				}
				line++
				b := sc.Bytes()
				if len(b) == 0 {
					continue
				}
				n++
				if err := im.line(b); err != nil {
					return fmt.Errorf("line %d: %w", line, err)
				}
			}
			if err := sc.Err(); err != nil {
				return err
			}
			return im.flush()
		})
		if err != nil {
			if committed > 0 {
				err = fmt.Errorf("%w (lines 1-%d were committed; import is idempotent, re-run the file)", err, committed)
			}
			return im.report, err
		}
		committed = line
		im.report.Committed = committed
		if !more {
			return im.report, nil
		}
	}
}

func (im *importer) line(b []byte) error {
	var head struct {
		T      string `json:"t"`
		Format int    `json:"format"`
	}
	if err := json.Unmarshal(b, &head); err != nil {
		return err
	}
	switch head.T {
	case "_meta":
		if head.Format > Format {
			return fmt.Errorf("unsupported export format %d (this build reads ≤ %d)", head.Format, Format)
		}
		return nil
	case "users":
		var r userRow
		if err := json.Unmarshal(b, &r); err != nil {
			return err
		}
		if r.Email == "" || r.PasswordHash == "" {
			return errors.New("users row without email or password_hash")
		}
		im.batch.Queue(insertUser, strings.ToLower(r.Email), r.Name, r.PasswordHash, tsOrNow(r.CreatedAt))
	case "api_keys":
		var r apiKeyRow
		if err := json.Unmarshal(b, &r); err != nil {
			return err
		}
		if len(r.KeyHash) == 0 {
			return errors.New("api_keys row without key_hash")
		}
		var lastUsed, revoked *time.Time
		if r.LastUsedAt != nil {
			lastUsed = &r.LastUsedAt.Time
		}
		if r.RevokedAt != nil {
			revoked = &r.RevokedAt.Time
		}
		im.batch.Queue(insertAPIKey, r.Name, r.KeyHash, r.Prefix, r.CreatedBy, tsOrNow(r.CreatedAt), lastUsed, revoked)
	case "projects":
		var r projectRow
		if err := json.Unmarshal(b, &r); err != nil {
			return err
		}
		if r.Slug == "" {
			return errors.New("projects row without slug")
		}
		if r.PublicKey == "" {
			r.PublicKey = auth.NewProjectKey()
		}
		if r.SampleRate <= 0 {
			r.SampleRate = 1
		}
		var id int64
		quota := int32(0)
		if r.DailyQuota != nil {
			quota = *r.DailyQuota
		}
		err := im.tx.QueryRow(im.ctx, upsertProject, r.Slug, r.Name, r.Platform, r.PublicKey, r.SampleKeepFirst, r.SampleRate, quota, tsOrNow(r.CreatedAt)).Scan(&id)
		if err != nil {
			return fmt.Errorf("upsert project %q: %w", r.Slug, err)
		}
		im.projects[r.Slug] = id
	case "releases":
		var r releaseRow
		if err := json.Unmarshal(b, &r); err != nil {
			return err
		}
		pid, err := im.project(r.Project)
		if err != nil {
			return err
		}
		if r.Release == "" {
			return errors.New("releases row without release")
		}
		if r.Platforms == nil {
			r.Platforms = []string{}
		}
		im.batch.Queue(upsertRelease, pid, r.Release, r.Platforms, tsOrNow(r.FirstSeen))
	case "issues":
		var r issueRow
		if err := json.Unmarshal(b, &r); err != nil {
			return err
		}
		if r.Transaction == nil {
			r.Transaction = r.Screen // format 1
		}
		pid, err := im.project(r.Project)
		if err != nil {
			return err
		}
		if r.Status == "" || r.Status == "triaged" { // triaged: a state before format 3, not one of Sentry's
			r.Status = "unresolved"
		}
		fp, ok := sentry.ParseID(r.Fingerprint)
		if !ok {
			return fmt.Errorf("issues row: fingerprint %q is not a 32-hex id", r.Fingerprint)
		}
		im.batch.Queue(upsertIssue, pid, fp, r.Title, r.Level, r.ErrorType, r.Transaction, r.Platform, r.Status, r.StatusBy,
			r.EventCount, r.StoredCount, tsOrNow(r.FirstSeen), tsOrNow(r.LastSeen), r.FirstRelease, r.LastRelease,
			nonNilStrings(r.Releases), r.ResolvedReleases, tsOrNow(r.CreatedAt), tsOrNow(r.UpdatedAt))
	case "events":
		var r eventRow
		if err := json.Unmarshal(b, &r); err != nil {
			return err
		}
		if r.Transaction == nil {
			r.Transaction = r.Screen // format 1
		}
		if r.Culprit == nil {
			r.Culprit = r.ErrorLocation
		}
		pid, err := im.project(r.Project)
		if err != nil {
			return err
		}
		if r.OccurredAt.IsZero() || r.EventID == "" {
			return errors.New("events row needs occurred_at and event_id")
		}
		eid, ok := sentry.ParseID(r.EventID)
		if !ok {
			return fmt.Errorf("events row: event_id %q is not a 32-hex id", r.EventID)
		}
		var fp *sentry.ID
		if r.Fingerprint != nil {
			id, ok := sentry.ParseID(*r.Fingerprint)
			if !ok {
				return fmt.Errorf("events row: fingerprint %q is not a 32-hex id", *r.Fingerprint)
			}
			fp = &id
		}
		var gz []byte
		if len(r.Payload) > 0 && string(r.Payload) != "null" {
			gz = store.Gzip([]byte(r.Payload))
		}
		im.events = append(im.events, store.EventInsert{
			OccurredAt: r.OccurredAt.Time, ProjectID: pid, EventID: eid, Level: r.Level, Message: r.Message, Platform: r.Platform,
			Environment: r.Environment, Release: r.Release, DeviceID: r.DeviceID, DeviceModel: r.DeviceModel,
			OSVersion: r.OSVersion, Transaction: r.Transaction, ErrorType: r.ErrorType, Culprit: r.Culprit,
			Handled: r.Handled, SDKName: r.SDKName, UserID: r.UserID, Fingerprint: fp,
			Symbolicated: r.Symbolicated, Tags: orJSON(r.Tags, "{}"),
			Symbols: r.Symbols, Payload: gz,
		})
		if len(im.events) >= batchSize {
			if err := im.flushEvents(); err != nil {
				return err
			}
		}
	case "sessions":
		var r sessionRow
		if err := json.Unmarshal(b, &r); err != nil {
			return err
		}
		pid, err := im.project(r.Project)
		if err != nil {
			return err
		}
		if r.StartedAt.IsZero() || r.SID == "" {
			return errors.New("sessions row needs started_at and sid")
		}
		im.batch.Queue(insertSession, r.StartedAt.Time, pid, r.SID, r.Release, r.Environment, r.Status, max(r.Count, 1))
		im.mark(im.dirtyS, pid, r.StartedAt.Time)
	case "symbol_files":
		var r symbolFileRow
		if err := json.Unmarshal(b, &r); err != nil {
			return err
		}
		pid, err := im.project(r.Project)
		if err != nil {
			return err
		}
		if r.Size == 0 {
			r.Size = int64(len(r.Data))
		}
		im.batch.Queue(upsertSymbolFile, pid, r.Kind, nilIfEmptyStr(r.Release), r.DebugID, r.Filename, r.Size, r.Data, tsOrNow(r.UploadedAt))
	case "alert_rules":
		var r alertRuleRow
		if err := json.Unmarshal(b, &r); err != nil {
			return err
		}
		if r.Type == "crash_spike" {
			r.Type = "unhandled_spike" // format 1
		}
		pid, err := im.project(r.Project)
		if err != nil {
			return err
		}
		var lt *time.Time
		if r.LastTriggered != nil {
			lt = &r.LastTriggered.Time
		}
		im.batch.Queue(upsertAlertRule, pid, r.Type, r.Enabled, r.CooldownMinutes, lt)
	case "alert_channels":
		var r alertChannelRow
		if err := json.Unmarshal(b, &r); err != nil {
			return err
		}
		pid, err := im.project(r.Project)
		if err != nil {
			return err
		}
		im.batch.Queue(insertAlertChannel, pid, r.Kind, orJSON(r.Config, "{}"), tsOrNow(r.CreatedAt))
	default:
		im.report.Rows["skipped"]++
		return nil
	}
	im.report.Rows[head.T]++
	if im.batch.Len() >= batchSize {
		return im.flushBatch()
	}
	return nil
}

// project resolves a slug, creating the project when it is unknown.
func (im *importer) project(slug string) (int64, error) {
	if slug == "" {
		return 0, errors.New("row without project")
	}
	if id, ok := im.projects[slug]; ok {
		return id, nil
	}
	p, err := im.q.GetProject(im.ctx, slug)
	if errors.Is(err, pgx.ErrNoRows) {
		p, err = im.q.CreateProject(im.ctx, sqlc.CreateProjectParams{Slug: slug, Name: slug, PublicKey: auth.NewProjectKey()})
	}
	if err != nil {
		return 0, fmt.Errorf("project %q: %w", slug, err)
	}
	im.projects[slug] = p.ID
	return p.ID, nil
}

func (im *importer) flush() error {
	if err := im.flushEvents(); err != nil {
		return err
	}
	if err := im.flushBatch(); err != nil {
		return err
	}
	// Sessions go through the batch, not store.InsertSessions: their
	// hours are marked here (events are marked by InsertEvents).
	for pid, hours := range im.dirtyS {
		if err := im.q.MarkSessionStatsDirty(im.ctx, sqlc.MarkSessionStatsDirtyParams{ProjectID: pid, Buckets: slices.Collect(maps.Keys(hours))}); err != nil {
			return err
		}
	}
	im.dirtyS = map[int64]map[time.Time]bool{}
	return nil
}

// mark notes the UTC hour of t for pid in m.
func (im *importer) mark(m map[int64]map[time.Time]bool, pid int64, t time.Time) {
	if m[pid] == nil {
		m[pid] = map[time.Time]bool{}
	}
	m[pid][t.UTC().Truncate(time.Hour)] = true
}

func (im *importer) flushEvents() error {
	if len(im.events) == 0 {
		return nil
	}
	rows := im.events
	im.events = nil
	return store.InsertEvents(im.ctx, im.tx, rows)
}

func (im *importer) flushBatch() error {
	if im.batch.Len() == 0 {
		return nil
	}
	b := im.batch
	im.batch = &pgx.Batch{}
	res := im.tx.SendBatch(im.ctx, b)
	for range b.Len() {
		if _, err := res.Exec(); err != nil {
			res.Close()
			return err
		}
	}
	return res.Close()
}

func orJSON(v json.RawMessage, def string) json.RawMessage {
	if len(v) == 0 || string(v) == "null" {
		return json.RawMessage(def)
	}
	return v
}

func tsOrNow(v ts) time.Time {
	if v.IsZero() {
		return time.Now().UTC()
	}
	return v.Time
}

func nonNilStrings(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}

func strOr(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

func nilIfEmptyStr(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

func idStr(id *sentry.ID) *string {
	if id == nil {
		return nil
	}
	s := string(*id)
	return &s
}
