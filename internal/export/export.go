// Package export streams every table as NDJSON and loads it back.
//
// The format is specified in docs/export-format.md — that file is the
// contract shared with other CrashCart implementations; change it first.
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
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/crashcartapp/crashcart/internal/db/sqlc"
	"github.com/crashcartapp/crashcart/internal/store"
)

// Format is the NDJSON format version written in the _meta line.
const Format = 1

// Tables lists the exported tables in the order they are written (and the
// order import expects: projects first so later rows can reference them).
var Tables = []string{"projects", "issues", "events", "sessions", "symbol_files", "alert_rules", "alert_channels"}

// Options narrows an export.
type Options struct {
	Project string // slug; "" = all
}

// maxLine is the longest NDJSON line import accepts (payloads are ≤ 20 MB
// envelopes, so a single event row fits comfortably).
const maxLine = 16 << 20

// batchSize is how many events / sessions / upserts go in one round trip.
const batchSize = 500

// ── row shapes ─────────────────────────────────────────────────────────

type metaRow struct {
	T          string    `json:"t"`
	Format     int       `json:"format"`
	ExportedAt time.Time `json:"exported_at"`
	App        string    `json:"app"`
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

type issueRow struct {
	T               string  `json:"t"`
	Project         string  `json:"project"`
	Fingerprint     string  `json:"fingerprint"`
	Title           string  `json:"title"`
	Level           string  `json:"level"`
	ErrorType       *string `json:"error_type,omitempty"`
	Screen          *string `json:"screen,omitempty"`
	Platform        *string `json:"platform,omitempty"`
	Status          string  `json:"status"`
	EventCount      int64   `json:"event_count"`
	StoredCount     int64   `json:"stored_count"`
	FirstSeen       ts      `json:"first_seen"`
	LastSeen        ts      `json:"last_seen"`
	FirstRelease    *string `json:"first_release,omitempty"`
	LastRelease     *string `json:"last_release,omitempty"`
	ResolvedRelease *string `json:"resolved_release,omitempty"`
	CreatedAt       ts      `json:"created_at"`
	UpdatedAt       ts      `json:"updated_at"`
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
	Screen        *string         `json:"screen,omitempty"`
	ErrorType     *string         `json:"error_type,omitempty"`
	ErrorLocation *string         `json:"error_location,omitempty"`
	Handled       *bool           `json:"handled,omitempty"`
	SDKName       *string         `json:"sdk_name,omitempty"`
	UserID        *string         `json:"user_id,omitempty"`
	Fingerprint   *string         `json:"fingerprint,omitempty"`
	Symbolicated  bool            `json:"symbolicated"`
	Tags          json.RawMessage `json:"tags"`
	Breadcrumbs   json.RawMessage `json:"breadcrumbs"`
	Payload       json.RawMessage `json:"payload"`
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
	selectIssues = `SELECT project_id, fingerprint, title, level, error_type, screen, platform, status, event_count, stored_count,
	first_seen, last_seen, first_release, last_release, resolved_release, created_at, updated_at
	FROM issues WHERE project_id = $1 ORDER BY fingerprint`
	selectEvents = `SELECT occurred_at, project_id, event_id, level, message, platform, environment, release, device_id, device_model,
	os_version, screen, error_type, error_location, handled, sdk_name, user_id, fingerprint, symbolicated, tags, breadcrumbs,
	payload, symbols FROM events WHERE project_id = $1 ORDER BY occurred_at, event_id`
	selectSessions    = `SELECT started_at, project_id, sid, release, environment, status, count FROM sessions WHERE project_id = $1 ORDER BY started_at, sid`
	selectSymbolFiles = `SELECT id, project_id, kind, release, debug_id, filename, size, data, uploaded_at
	FROM symbol_files WHERE project_id = $1 ORDER BY kind, release, filename`
	selectAlertRules    = `SELECT project_id, type, enabled, cooldown_minutes, last_triggered FROM alert_rules WHERE project_id = $1 ORDER BY type`
	selectAlertChannels = `SELECT id, project_id, kind, config, created_at FROM alert_channels WHERE project_id = $1 ORDER BY id`
)

// Export writes NDJSON to w. Tables are streamed row by row per project
// (projects in slug order), never loaded into memory.
func Export(ctx context.Context, st *store.Store, w io.Writer, opt Options) error {
	bw := bufio.NewWriterSize(w, 256<<10)
	enc := json.NewEncoder(bw)
	enc.SetEscapeHTML(false)

	projects, err := exportProjects(ctx, st, opt)
	if err != nil {
		return err
	}
	if err := enc.Encode(metaRow{T: "_meta", Format: Format, ExportedAt: time.Now().UTC(), App: "crashcart"}); err != nil {
		return err
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
		if err := stream(ctx, st, selectIssues, p.ID, func(r sqlc.Issue) error {
			return enc.Encode(issueRow{
				T: "issues", Project: p.Slug, Fingerprint: r.Fingerprint, Title: r.Title, Level: r.Level,
				ErrorType: r.ErrorType, Screen: r.Screen, Platform: r.Platform, Status: r.Status,
				EventCount: r.EventCount, StoredCount: r.StoredCount, FirstSeen: at(r.FirstSeen), LastSeen: at(r.LastSeen),
				FirstRelease: r.FirstRelease, LastRelease: r.LastRelease, ResolvedRelease: r.ResolvedRelease,
				CreatedAt: at(r.CreatedAt), UpdatedAt: at(r.UpdatedAt),
			})
		}); err != nil {
			return fmt.Errorf("export issues: %w", err)
		}
	}
	for _, p := range projects {
		if err := stream(ctx, st, selectEvents, p.ID, func(r sqlc.Event) error {
			return enc.Encode(eventRow{
				T: "events", Project: p.Slug, OccurredAt: at(r.OccurredAt), EventID: r.EventID, Level: r.Level, Message: r.Message,
				Platform: r.Platform, Environment: r.Environment, Release: r.Release, DeviceID: r.DeviceID,
				DeviceModel: r.DeviceModel, OSVersion: r.OsVersion, Screen: r.Screen, ErrorType: r.ErrorType,
				ErrorLocation: r.ErrorLocation, Handled: r.Handled, SDKName: r.SdkName, UserID: r.UserID,
				Fingerprint: r.Fingerprint, Symbolicated: r.Symbolicated, Tags: r.Tags, Breadcrumbs: r.Breadcrumbs,
				Payload: r.Payload, Symbols: r.Symbols,
			})
		}); err != nil {
			return fmt.Errorf("export events: %w", err)
		}
	}
	for _, p := range projects {
		if err := stream(ctx, st, selectSessions, p.ID, func(r sqlc.Session) error {
			return enc.Encode(sessionRow{T: "sessions", Project: p.Slug, StartedAt: at(r.StartedAt), SID: r.Sid, Release: r.Release, Environment: r.Environment, Status: r.Status, Count: r.Count})
		}); err != nil {
			return fmt.Errorf("export sessions: %w", err)
		}
	}
	for _, p := range projects {
		if err := stream(ctx, st, selectSymbolFiles, p.ID, func(r sqlc.SymbolFile) error {
			return enc.Encode(symbolFileRow{
				T: "symbol_files", Project: p.Slug, Kind: r.Kind, Release: r.Release, DebugID: r.DebugID,
				Filename: r.Filename, Size: r.Size, Data: r.Data, UploadedAt: at(r.UploadedAt),
			})
		}); err != nil {
			return fmt.Errorf("export symbol_files: %w", err)
		}
	}
	for _, p := range projects {
		if err := stream(ctx, st, selectAlertRules, p.ID, func(r sqlc.AlertRule) error {
			var lt *ts
			if r.LastTriggered != nil {
				v := at(*r.LastTriggered)
				lt = &v
			}
			return enc.Encode(alertRuleRow{T: "alert_rules", Project: p.Slug, Type: r.Type, Enabled: r.Enabled, CooldownMinutes: r.CooldownMinutes, LastTriggered: lt})
		}); err != nil {
			return fmt.Errorf("export alert_rules: %w", err)
		}
	}
	for _, p := range projects {
		if err := stream(ctx, st, selectAlertChannels, p.ID, func(r sqlc.AlertChannel) error {
			return enc.Encode(alertChannelRow{T: "alert_channels", Project: p.Slug, Kind: r.Kind, Config: r.Config, CreatedAt: at(r.CreatedAt)})
		}); err != nil {
			return fmt.Errorf("export alert_channels: %w", err)
		}
	}
	return bw.Flush()
}

func exportProjects(ctx context.Context, st *store.Store, opt Options) ([]sqlc.Project, error) {
	if opt.Project != "" {
		p, err := st.GetProject(ctx, opt.Project)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, fmt.Errorf("project %q not found", opt.Project)
		}
		return []sqlc.Project{p}, err
	}
	r, err := st.Pool.Query(ctx, "SELECT id, slug, name, platform, public_key, sample_keep_first, sample_rate, daily_quota, created_at FROM projects ORDER BY slug")
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(r, pgx.RowToStructByPos[sqlc.Project])
}

// stream runs sql for one project and calls fn per row as it arrives.
func stream[T any](ctx context.Context, st *store.Store, sql string, projectID int64, fn func(T) error) error {
	rows, err := st.Pool.Query(ctx, sql, projectID)
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
	Rows map[string]int64 `json:"rows"`
}

const (
	upsertProject = `INSERT INTO projects (slug, name, platform, public_key, sample_keep_first, sample_rate, daily_quota, created_at)
	VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
	ON CONFLICT (slug) DO UPDATE SET name = EXCLUDED.name, platform = EXCLUDED.platform,
	    sample_keep_first = EXCLUDED.sample_keep_first, sample_rate = EXCLUDED.sample_rate,
	    daily_quota = EXCLUDED.daily_quota, created_at = EXCLUDED.created_at
	RETURNING id`
	upsertIssue = `INSERT INTO issues (project_id, fingerprint, title, level, error_type, screen, platform, status, event_count,
	stored_count, first_seen, last_seen, first_release, last_release, resolved_release, created_at, updated_at)
	VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17)
	ON CONFLICT (project_id, fingerprint) DO UPDATE SET title = EXCLUDED.title, level = EXCLUDED.level,
	    error_type = EXCLUDED.error_type, screen = EXCLUDED.screen, platform = EXCLUDED.platform, status = EXCLUDED.status,
	    event_count = EXCLUDED.event_count, stored_count = EXCLUDED.stored_count, first_seen = EXCLUDED.first_seen,
	    last_seen = EXCLUDED.last_seen, first_release = EXCLUDED.first_release, last_release = EXCLUDED.last_release,
	    resolved_release = EXCLUDED.resolved_release, created_at = EXCLUDED.created_at, updated_at = EXCLUDED.updated_at`
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

// importer carries the state of one Import call.
type importer struct {
	ctx      context.Context
	st       *store.Store
	projects map[string]int64 // slug → id
	events   []store.EventInsert
	batch    *pgx.Batch // sessions / issues / symbol files / alert rows
	report   Report
}

// Import loads NDJSON from r (idempotent). Rows referencing a project slug
// that does not exist create it (name = slug, fresh public key).
func Import(ctx context.Context, st *store.Store, r io.Reader) (Report, error) {
	im := &importer{ctx: ctx, st: st, projects: map[string]int64{}, batch: &pgx.Batch{}, report: Report{Rows: map[string]int64{}}}
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 1<<20), maxLine)
	line := 0
	for sc.Scan() {
		line++
		b := sc.Bytes()
		if len(b) == 0 {
			continue
		}
		if err := im.line(b); err != nil {
			return im.report, fmt.Errorf("line %d: %w", line, err)
		}
	}
	if err := sc.Err(); err != nil {
		return im.report, err
	}
	if err := im.flush(); err != nil {
		return im.report, err
	}
	return im.report, nil
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
	case "projects":
		var r projectRow
		if err := json.Unmarshal(b, &r); err != nil {
			return err
		}
		if r.Slug == "" {
			return errors.New("projects row without slug")
		}
		if r.PublicKey == "" {
			r.PublicKey = newKey()
		}
		if r.SampleRate <= 0 {
			r.SampleRate = 1
		}
		var id int64
		quota := int32(100000)
		if r.DailyQuota != nil {
			quota = *r.DailyQuota
		}
		err := im.st.Pool.QueryRow(im.ctx, upsertProject, r.Slug, r.Name, r.Platform, r.PublicKey, r.SampleKeepFirst, r.SampleRate, quota, tsOrNow(r.CreatedAt)).Scan(&id)
		if err != nil {
			return fmt.Errorf("upsert project %q: %w", r.Slug, err)
		}
		im.projects[r.Slug] = id
	case "issues":
		var r issueRow
		if err := json.Unmarshal(b, &r); err != nil {
			return err
		}
		pid, err := im.project(r.Project)
		if err != nil {
			return err
		}
		if r.Status == "" {
			r.Status = "unresolved"
		}
		im.batch.Queue(upsertIssue, pid, r.Fingerprint, r.Title, r.Level, r.ErrorType, r.Screen, r.Platform, r.Status,
			r.EventCount, r.StoredCount, tsOrNow(r.FirstSeen), tsOrNow(r.LastSeen), r.FirstRelease, r.LastRelease, r.ResolvedRelease,
			tsOrNow(r.CreatedAt), tsOrNow(r.UpdatedAt))
	case "events":
		var r eventRow
		if err := json.Unmarshal(b, &r); err != nil {
			return err
		}
		pid, err := im.project(r.Project)
		if err != nil {
			return err
		}
		if r.OccurredAt.IsZero() || r.EventID == "" || len(r.Payload) == 0 {
			return errors.New("events row needs occurred_at, event_id and payload")
		}
		im.events = append(im.events, store.EventInsert{
			OccurredAt: r.OccurredAt.Time, ProjectID: pid, EventID: r.EventID, Level: r.Level, Message: r.Message, Platform: r.Platform,
			Environment: r.Environment, Release: r.Release, DeviceID: r.DeviceID, DeviceModel: r.DeviceModel,
			OSVersion: r.OSVersion, Screen: r.Screen, ErrorType: r.ErrorType, ErrorLocation: r.ErrorLocation,
			Handled: r.Handled, SDKName: r.SDKName, UserID: r.UserID, Fingerprint: r.Fingerprint,
			Symbolicated: r.Symbolicated, Tags: orJSON(r.Tags, "{}"), Breadcrumbs: orJSON(r.Breadcrumbs, "[]"),
			Payload: r.Payload, Symbols: r.Symbols,
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
		im.batch.Queue(upsertSymbolFile, pid, r.Kind, r.Release, r.DebugID, r.Filename, r.Size, r.Data, tsOrNow(r.UploadedAt))
	case "alert_rules":
		var r alertRuleRow
		if err := json.Unmarshal(b, &r); err != nil {
			return err
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
	p, err := im.st.GetProject(im.ctx, slug)
	if errors.Is(err, pgx.ErrNoRows) {
		p, err = im.st.CreateProject(im.ctx, sqlc.CreateProjectParams{Slug: slug, Name: slug, PublicKey: newKey()})
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
	return im.flushBatch()
}

func (im *importer) flushEvents() error {
	if len(im.events) == 0 {
		return nil
	}
	rows := im.events
	im.events = nil
	return im.st.Tx(im.ctx, func(ctx context.Context, tx pgx.Tx, _ *sqlc.Queries) error {
		return store.InsertEvents(ctx, tx, rows)
	})
}

func (im *importer) flushBatch() error {
	if im.batch.Len() == 0 {
		return nil
	}
	b := im.batch
	im.batch = &pgx.Batch{}
	res := im.st.Pool.SendBatch(im.ctx, b)
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

func newKey() string {
	b := make([]byte, 16)
	rand.Read(b)
	return hex.EncodeToString(b)
}
