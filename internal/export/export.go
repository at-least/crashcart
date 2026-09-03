// Package export streams every table as NDJSON and loads it back.
//
// This code is the format: docs/reference/export-format.md is hand-written
// but checked against Format and Tables (cmd/gendocs), so a change here
// that isn't matched there is caught, not silently drifted.
//
// Format (one JSON object per line):
//
//	{"t":"_meta","format":<Format>,"exported_at":"<RFC3339>","app":"crashcart"}
//	{"t":"projects", ...}   then issues, events, attachments, user_reports,
//	                        monitors, monitor_checkins, sessions, symbol_files,
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
// Import is idempotent: events, attachments and sessions are inserted with
// ON CONFLICT DO NOTHING, everything else is upserted on its natural key
// (issue counts are replaced, not added; a user_reports row is replaced
// wholesale, as ingest itself does on a resend), alert channels are
// inserted only when no identical (project, kind, config) row exists.
package export

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"slices"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/at-least/crashcart/internal/auth"
	"github.com/at-least/crashcart/internal/blob"
	"github.com/at-least/crashcart/internal/sentry"
	"github.com/at-least/crashcart/internal/store"
	"github.com/at-least/crashcart/internal/symbolicate"
)

// Format is the NDJSON format version written in the _meta line.
const Format = 3 // 3: no triaged status; 2: transaction / culprit / unhandled_spike (was screen / error_location / crash_spike)

// Tables lists the exported tables in the order they are written (and the
// order import expects: projects first so later rows can reference them).
var Tables = []string{"users", "api_keys", "projects", "project_keys", "releases", "issues", "events", "attachments", "user_reports", "monitors", "monitor_checkins", "sessions", "symbol_files", "alert_rules", "alert_channels"}

// Options narrows an export.
type Options struct {
	Project string       // slug; "" = all
	Log     *slog.Logger // nil = slog.Default
}

// maxLine is the longest NDJSON line import accepts: a symbol file, the
// largest row, is at most symbolicate.MaxUpload, and base64 (a ~4/3
// expansion) is the biggest single field on its line — the fixed
// remainder covers the row's other JSON fields (project slug, filename,
// kind, release, debug_id, size, uploaded_at), never more than a few KB.
// An event payload is at most a 20 MB envelope, an attachment
// sentry.MaxAttachmentSize — both well under a symbol file's own bound.
const maxLine = symbolicate.MaxUpload*4/3 + (8 << 20)

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
	T                     string   `json:"t"`
	Project               string   `json:"project"`
	Fingerprint           string   `json:"fingerprint"`
	Title                 string   `json:"title"`
	Level                 string   `json:"level"`
	ErrorType             *string  `json:"error_type,omitempty"`
	Transaction           *string  `json:"transaction,omitempty"`
	Screen                *string  `json:"screen,omitempty"` // format 1 name of transaction (read only)
	Platform              *string  `json:"platform,omitempty"`
	Status                string   `json:"status"`
	StatusBy              *string  `json:"status_by,omitempty"`
	EventCount            int64    `json:"event_count"`
	StoredCount           int64    `json:"stored_count"`
	FirstSeen             ts       `json:"first_seen"`
	LastSeen              ts       `json:"last_seen"`
	FirstRelease          *string  `json:"first_release,omitempty"`
	LastRelease           *string  `json:"last_release,omitempty"`
	Releases              []string `json:"releases,omitempty"`
	ResolvedReleases      []string `json:"resolved_releases,omitempty"`
	IgnoreUntil           *ts      `json:"ignore_until,omitempty"`
	IgnoreUntilCount      *int64   `json:"ignore_until_count,omitempty"`
	IgnoreUntilEscalating bool     `json:"ignore_until_escalating,omitempty"`
	IgnoreBaseline        *int64   `json:"ignore_baseline,omitempty"`
	CreatedAt             ts       `json:"created_at"`
	UpdatedAt             ts       `json:"updated_at"`
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

type attachmentRow struct {
	T              string `json:"t"`
	Project        string `json:"project"`
	OccurredAt     ts     `json:"occurred_at"` // the event's
	EventID        string `json:"event_id"`
	N              int32  `json:"n"`
	Filename       string `json:"filename"`
	ContentType    string `json:"content_type"`
	AttachmentType string `json:"attachment_type"`
	Size           int64  `json:"size"`
	Data           []byte `json:"data"` // base64
}

type userReportRow struct {
	T          string  `json:"t"`
	Project    string  `json:"project"`
	EventID    string  `json:"event_id"`
	Name       *string `json:"name,omitempty"`
	Email      *string `json:"email,omitempty"`
	Comments   string  `json:"comments"`
	ReceivedAt ts      `json:"received_at"`
}

// projectKeyRow: a DSN key Rotate has retired but nobody has deleted yet.
type projectKeyRow struct {
	T          string `json:"t"`
	Project    string `json:"project"`
	PublicKey  string `json:"public_key"`
	RetiredAt  ts     `json:"retired_at"`
	LastUsedAt *ts    `json:"last_used_at,omitempty"`
}

type monitorRow struct {
	T                    string  `json:"t"`
	Project              string  `json:"project"`
	Slug                 string  `json:"slug"`
	ScheduleType         string  `json:"schedule_type"`
	ScheduleValue        string  `json:"schedule_value"`
	ScheduleUnit         *string `json:"schedule_unit,omitempty"`
	Timezone             string  `json:"timezone"`
	CheckinMarginMin     int32   `json:"checkin_margin_min"`
	MaxRuntimeMin        int32   `json:"max_runtime_min"`
	FailureThreshold     int32   `json:"failure_threshold"`
	RecoveryThreshold    int32   `json:"recovery_threshold"`
	LastStatus           *string `json:"last_status,omitempty"`
	ConsecutiveFailures  int32   `json:"consecutive_failures"`
	ConsecutiveSuccesses int32   `json:"consecutive_successes"`
	Alerting             bool    `json:"alerting"`
	NextExpectedAt       *ts     `json:"next_expected_at,omitempty"`
	LastCheckinAt        *ts     `json:"last_checkin_at,omitempty"`
	CreatedAt            ts      `json:"created_at"`
}

type monitorCheckinRow struct {
	T           string   `json:"t"`
	Project     string   `json:"project"`
	StartedAt   ts       `json:"started_at"`
	MonitorSlug string   `json:"monitor_slug"`
	CheckInID   string   `json:"check_in_id"`
	Status      string   `json:"status"`
	DurationS   *float32 `json:"duration_s,omitempty"`
	Release     *string  `json:"release,omitempty"`
	Environment *string  `json:"environment,omitempty"`
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

func tsPtr(t *time.Time) *ts {
	if t == nil {
		return nil
	}
	v := at(*t)
	return &v
}

// ── export ─────────────────────────────────────────────────────────────

const (
	selectIssues = `SELECT project_id, fingerprint, title, level, error_type, transaction, platform, status, status_by, event_count, stored_count,
	first_seen, last_seen, first_release, last_release, releases, resolved_releases, ignore_until, ignore_until_count, ignore_until_escalating, ignore_baseline, created_at, updated_at
	FROM issues WHERE project_id = $1 ORDER BY fingerprint`
	// Events in pack order — the rows whose payload is in the column or the
	// spool first, by time, then the packed ones pack by pack, in offset
	// order — so the export reads each pack once (PackReader), whatever
	// order the events arrived in (a late crash lands in a later pack than
	// its neighbours in time). Import does not care about row order.
	selectEvents = `SELECT e.occurred_at, e.project_id, e.event_id, e.level, e.message, e.platform, e.environment, e.release, e.device_id, e.device_model,
	e.os_version, e.transaction, e.error_type, e.culprit, e.handled, e.sdk_name, e.user_id, e.fingerprint, e.symbolicated, e.tags,
	e.symbols, e.payload FROM events e
	LEFT JOIN event_packs k ON k.project_id = e.project_id AND k.event_id = e.event_id AND k.occurred_at = e.occurred_at
	WHERE e.project_id = $1 ORDER BY k.pack_id NULLS FIRST, k.pack_offset, e.occurred_at, e.event_id`
	selectAttachments = `SELECT occurred_at, project_id, event_id, n, filename, content_type, attachment_type, size, data FROM attachments WHERE project_id = $1 ORDER BY occurred_at, event_id, n`
	selectUserReports = `SELECT project_id, event_id, received_at, name, email, comments FROM user_reports WHERE project_id = $1 ORDER BY event_id`
	selectProjectKeys = `SELECT id, project_id, public_key, retired_at, last_used_at FROM project_keys WHERE project_id = $1 ORDER BY retired_at`
	selectMonitors    = `SELECT project_id, slug, schedule_type, schedule_value, schedule_unit, timezone, checkin_margin_min, max_runtime_min,
	failure_threshold, recovery_threshold, last_status, consecutive_failures, consecutive_successes, alerting, next_expected_at, last_checkin_at, created_at
	FROM monitors WHERE project_id = $1 ORDER BY slug`
	selectMonitorCheckins = `SELECT started_at, project_id, monitor_slug, check_in_id, status, duration_s, release, environment
	FROM monitor_checkins WHERE project_id = $1 ORDER BY monitor_slug, started_at, check_in_id`
	selectSessions    = `SELECT started_at, project_id, sid, release, environment, status, count FROM sessions WHERE project_id = $1 ORDER BY started_at, sid`
	selectReleases    = `SELECT project_id, release, platforms, first_seen FROM releases WHERE project_id = $1 ORDER BY release`
	selectSymbolFiles = `SELECT id, project_id, kind, release, debug_id, filename, size, data, blob_key, uploaded_at
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
		if err := stream(ctx, tx, selectProjectKeys, func(r store.ProjectKey) error {
			return enc.Encode(projectKeyRow{T: "project_keys", Project: p.Slug, PublicKey: r.PublicKey, RetiredAt: at(r.RetiredAt), LastUsedAt: tsPtr(r.LastUsedAt)})
		}, p.ID); err != nil {
			return fmt.Errorf("export project_keys: %w", err)
		}
	}
	for _, p := range projects {
		if err := stream(ctx, tx, selectReleases, func(r store.Release) error {
			return enc.Encode(releaseRow{T: "releases", Project: p.Slug, Release: r.Release, Platforms: r.Platforms, FirstSeen: at(r.FirstSeen)})
		}, p.ID); err != nil {
			return fmt.Errorf("export releases: %w", err)
		}
	}
	for _, p := range projects {
		if err := stream(ctx, tx, selectIssues, func(r store.Issue) error {
			return enc.Encode(issueRow{
				T: "issues", Project: p.Slug, Fingerprint: string(r.Fingerprint), Title: r.Title, Level: string(r.Level),
				ErrorType: r.ErrorType, Transaction: r.Transaction, Platform: r.Platform, Status: string(r.Status), StatusBy: r.StatusBy,
				EventCount: r.EventCount, StoredCount: r.StoredCount, FirstSeen: at(r.FirstSeen), LastSeen: at(r.LastSeen),
				FirstRelease: r.FirstRelease, LastRelease: r.LastRelease, Releases: r.Releases, ResolvedReleases: r.ResolvedReleases,
				IgnoreUntil: tsPtr(r.IgnoreUntil), IgnoreUntilCount: r.IgnoreUntilCount, IgnoreUntilEscalating: r.IgnoreUntilEscalating, IgnoreBaseline: r.IgnoreBaseline,
				CreatedAt: at(r.CreatedAt), UpdatedAt: at(r.UpdatedAt),
			})
		}, p.ID); err != nil {
			return fmt.Errorf("export issues: %w", err)
		}
	}
	// Payloads in packs are read through a small cache of whole packs: the
	// stream is in pack order, so this is about one GET per pack, not per
	// event. The location is read on the pool, not the snapshot connection
	// (it is busy streaming the rows) — PayloadLocation is one statement,
	// so it needs no snapshot of its own: a row packed since the snapshot
	// reads from its pack, one expired since reads as having no payload.
	packs := st.NewPackReader()
	for _, p := range projects {
		if err := stream(ctx, tx, selectEvents, func(r store.Event) error {
			payload, err := packs.Payload(ctx, nil, r)
			if err != nil {
				return fmt.Errorf("payload %s: %w", r.EventID, err)
			}
			return enc.Encode(eventRow{
				T: "events", Project: p.Slug, OccurredAt: at(r.OccurredAt), EventID: string(r.EventID), Level: string(r.Level), Message: r.Message,
				Platform: r.Platform, Environment: r.Environment, Release: r.Release, DeviceID: r.DeviceID,
				DeviceModel: r.DeviceModel, OSVersion: r.OSVersion, Transaction: r.Transaction, ErrorType: r.ErrorType,
				Culprit: r.Culprit, Handled: r.Handled, SDKName: r.SDKName, UserID: r.UserID,
				Fingerprint: idStr(r.Fingerprint), Symbolicated: r.Symbolicated, Tags: r.Tags,
				Payload: json.RawMessage(payload), Symbols: r.Symbols,
			})
		}, p.ID); err != nil {
			return fmt.Errorf("export events: %w", err)
		}
	}
	for _, p := range projects {
		if err := stream(ctx, tx, selectAttachments, func(r store.Attachment) error {
			return enc.Encode(attachmentRow{
				T: "attachments", Project: p.Slug, OccurredAt: at(r.OccurredAt), EventID: string(r.EventID), N: r.N,
				Filename: r.Filename, ContentType: r.ContentType, AttachmentType: r.AttachmentType, Size: r.Size, Data: r.Data,
			})
		}, p.ID); err != nil {
			return fmt.Errorf("export attachments: %w", err)
		}
	}
	for _, p := range projects {
		if err := stream(ctx, tx, selectUserReports, func(r store.UserReport) error {
			return enc.Encode(userReportRow{
				T: "user_reports", Project: p.Slug, EventID: string(r.EventID), Name: r.Name, Email: r.Email,
				Comments: r.Comments, ReceivedAt: at(r.ReceivedAt),
			})
		}, p.ID); err != nil {
			return fmt.Errorf("export user_reports: %w", err)
		}
	}
	for _, p := range projects {
		if err := stream(ctx, tx, selectMonitors, func(r store.Monitor) error {
			var lastStatus *string
			if r.LastStatus != nil {
				s := string(*r.LastStatus)
				lastStatus = &s
			}
			return enc.Encode(monitorRow{
				T: "monitors", Project: p.Slug, Slug: r.Slug, ScheduleType: r.ScheduleType, ScheduleValue: r.ScheduleValue,
				ScheduleUnit: r.ScheduleUnit, Timezone: r.Timezone, CheckinMarginMin: r.CheckinMarginMin, MaxRuntimeMin: r.MaxRuntimeMin,
				FailureThreshold: r.FailureThreshold, RecoveryThreshold: r.RecoveryThreshold, LastStatus: lastStatus,
				ConsecutiveFailures: r.ConsecutiveFailures, ConsecutiveSuccesses: r.ConsecutiveSuccesses, Alerting: r.Alerting,
				NextExpectedAt: tsPtr(r.NextExpectedAt), LastCheckinAt: tsPtr(r.LastCheckinAt), CreatedAt: at(r.CreatedAt),
			})
		}, p.ID); err != nil {
			return fmt.Errorf("export monitors: %w", err)
		}
	}
	for _, p := range projects {
		if err := stream(ctx, tx, selectMonitorCheckins, func(r store.MonitorCheckin) error {
			var durationS *float32
			if r.DurationS != nil {
				d := *r.DurationS
				durationS = &d
			}
			return enc.Encode(monitorCheckinRow{
				T: "monitor_checkins", Project: p.Slug, StartedAt: at(r.StartedAt), MonitorSlug: r.MonitorSlug,
				CheckInID: string(r.CheckInID), Status: string(r.Status), DurationS: durationS, Release: r.Release, Environment: r.Environment,
			})
		}, p.ID); err != nil {
			return fmt.Errorf("export monitor_checkins: %w", err)
		}
	}
	for _, p := range projects {
		if err := stream(ctx, tx, selectSessions, func(r store.Session) error {
			return enc.Encode(sessionRow{T: "sessions", Project: p.Slug, StartedAt: at(r.StartedAt), SID: r.Sid, Release: r.Release, Environment: r.Environment, Status: string(r.Status), Count: r.Count})
		}, p.ID); err != nil {
			return fmt.Errorf("export sessions: %w", err)
		}
	}
	for _, p := range projects {
		if err := stream(ctx, tx, selectSymbolFiles, func(r store.SymbolFile) error {
			data, ok, err := exportSymbolBytes(ctx, st, opt, r)
			if err != nil {
				return err
			}
			if !ok {
				return nil
			}
			return enc.Encode(symbolFileRow{
				T: "symbol_files", Project: p.Slug, Kind: string(r.Kind), Release: strOr(r.Release), DebugID: r.DebugID,
				Filename: r.Filename, Size: r.Size, Data: data, UploadedAt: at(r.UploadedAt),
			})
		}, p.ID); err != nil {
			return fmt.Errorf("export symbol_files: %w", err)
		}
	}
	for _, p := range projects {
		if err := stream(ctx, tx, selectAlertRules, func(r store.AlertRule) error {
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
		if err := stream(ctx, tx, selectAlertChannels, func(r store.AlertChannel) error {
			return enc.Encode(alertChannelRow{T: "alert_channels", Project: p.Slug, Kind: string(r.Kind), Config: r.Config, CreatedAt: at(r.CreatedAt)})
		}, p.ID); err != nil {
			return fmt.Errorf("export alert_channels: %w", err)
		}
	}
	return bw.Flush()
}

func exportProjects(ctx context.Context, tx pgx.Tx, opt Options) ([]store.Project, error) {
	if opt.Project != "" {
		p, err := store.GetProject(ctx, tx, opt.Project)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, fmt.Errorf("project %q not found", opt.Project)
		}
		return []store.Project{p}, err
	}
	r, err := tx.Query(ctx, "SELECT id, slug, name, platform, public_key, sample_keep_first, sample_rate, daily_quota, created_at FROM projects ORDER BY slug")
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(r, pgx.RowToStructByPos[store.Project])
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

// exportSymbolBytes is a symbol file's bytes for the file: the data column,
// or the object blob_key names — the export inlines them either way, so
// the file stands on its own and imports into any backend. The snapshot
// transaction does not cover the blob store: an object a re-upload
// replaced meanwhile is re-read through the row's current key once, and a
// file still missing is skipped with a warning rather than failing an
// export a long way in.
func exportSymbolBytes(ctx context.Context, st *store.Store, opt Options, r store.SymbolFile) (data []byte, ok bool, err error) {
	if r.BlobKey == nil {
		return r.Data, true, nil
	}
	if st.Blobs == nil {
		return nil, false, fmt.Errorf("symbol file %d is in the blob store, but BLOB_STORE is not configured", r.ID)
	}
	data, err = st.Blobs.Get(ctx, *r.BlobKey)
	if errors.Is(err, blob.ErrNotFound) {
		if row, rerr := store.SymbolFileData(ctx, st.Pool, r.ID); rerr == nil && row.BlobKey != nil {
			data, err = st.Blobs.Get(ctx, *row.BlobKey)
		} else if rerr == nil {
			data, err = row.Data, nil
		}
	}
	if errors.Is(err, blob.ErrNotFound) {
		log := opt.Log
		if log == nil {
			log = slog.Default()
		}
		log.Warn("export: symbol file skipped, its object is gone", "id", r.ID, "filename", r.Filename)
		return nil, false, nil
	}
	return data, err == nil, err
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
	stored_count, first_seen, last_seen, first_release, last_release, releases, resolved_releases,
	ignore_until, ignore_until_count, ignore_until_escalating, ignore_baseline, created_at, updated_at)
	VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,$19,$20,$21,$22,$23)
	ON CONFLICT (project_id, fingerprint) DO UPDATE SET title = EXCLUDED.title, level = EXCLUDED.level, status_by = EXCLUDED.status_by,
	    error_type = EXCLUDED.error_type, transaction = EXCLUDED.transaction, platform = EXCLUDED.platform, status = EXCLUDED.status,
	    event_count = GREATEST(issues.event_count, EXCLUDED.event_count), stored_count = GREATEST(issues.stored_count, EXCLUDED.stored_count), first_seen = EXCLUDED.first_seen,
	    last_seen = EXCLUDED.last_seen, first_release = EXCLUDED.first_release, last_release = EXCLUDED.last_release,
	    releases = EXCLUDED.releases, resolved_releases = EXCLUDED.resolved_releases,
	    ignore_until = EXCLUDED.ignore_until, ignore_until_count = EXCLUDED.ignore_until_count,
	    ignore_until_escalating = EXCLUDED.ignore_until_escalating, ignore_baseline = EXCLUDED.ignore_baseline,
	    created_at = EXCLUDED.created_at, updated_at = EXCLUDED.updated_at`
	insertAttachment = `INSERT INTO attachments (occurred_at, project_id, event_id, n, filename, content_type, attachment_type, size, data)
	VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9) ON CONFLICT (project_id, event_id, occurred_at, n) DO NOTHING`
	insertProjectKey = `INSERT INTO project_keys (project_id, public_key, retired_at, last_used_at) VALUES ($1,$2,$3,$4)
	ON CONFLICT (public_key) DO NOTHING`
	upsertUserReport = `INSERT INTO user_reports (project_id, event_id, received_at, name, email, comments)
	VALUES ($1,$2,$3,$4,$5,$6)
	ON CONFLICT (project_id, event_id) DO UPDATE SET
	    received_at = EXCLUDED.received_at, name = EXCLUDED.name, email = EXCLUDED.email, comments = EXCLUDED.comments`
	upsertMonitor = `INSERT INTO monitors (project_id, slug, schedule_type, schedule_value, schedule_unit, timezone,
	checkin_margin_min, max_runtime_min, failure_threshold, recovery_threshold, last_status, consecutive_failures,
	consecutive_successes, alerting, next_expected_at, last_checkin_at, created_at)
	VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17)
	ON CONFLICT (project_id, slug) DO UPDATE SET
	    schedule_type = EXCLUDED.schedule_type, schedule_value = EXCLUDED.schedule_value, schedule_unit = EXCLUDED.schedule_unit,
	    timezone = EXCLUDED.timezone, checkin_margin_min = EXCLUDED.checkin_margin_min, max_runtime_min = EXCLUDED.max_runtime_min,
	    failure_threshold = EXCLUDED.failure_threshold, recovery_threshold = EXCLUDED.recovery_threshold,
	    last_status = EXCLUDED.last_status, consecutive_failures = EXCLUDED.consecutive_failures,
	    consecutive_successes = EXCLUDED.consecutive_successes, alerting = EXCLUDED.alerting,
	    next_expected_at = EXCLUDED.next_expected_at, last_checkin_at = EXCLUDED.last_checkin_at, created_at = EXCLUDED.created_at`
	insertMonitorCheckin = `INSERT INTO monitor_checkins (started_at, project_id, monitor_slug, check_in_id, status, duration_s, release, environment)
	VALUES ($1,$2,$3,$4,$5,$6,$7,$8) ON CONFLICT (project_id, monitor_slug, check_in_id, started_at) DO NOTHING`
	upsertRelease = `INSERT INTO releases (project_id, release, platforms, first_seen) VALUES ($1,$2,$3,$4)
	ON CONFLICT (project_id, release) DO UPDATE SET
	    platforms = (SELECT array_agg(DISTINCT x ORDER BY x) FROM unnest(releases.platforms || EXCLUDED.platforms) AS x),
	    first_seen = LEAST(releases.first_seen, EXCLUDED.first_seen)`
	insertSession = `INSERT INTO sessions (started_at, project_id, sid, release, environment, status, count) VALUES ($1,$2,$3,$4,$5,$6,$7)
	ON CONFLICT (project_id, sid, started_at) DO NOTHING`
	upsertSymbolFile = `INSERT INTO symbol_files (project_id, kind, release, debug_id, filename, size, data, uploaded_at, blob_key)
	VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9)
	ON CONFLICT (project_id, kind, release, filename) DO UPDATE SET debug_id = EXCLUDED.debug_id, size = EXCLUDED.size,
	    data = EXCLUDED.data, blob_key = EXCLUDED.blob_key, uploaded_at = EXCLUDED.uploaded_at`
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
	projects map[string]int64 // slug → id
	events   []store.EventInsert
	batch    *pgx.Batch                   // sessions / issues / alert rows
	dirtyS   map[int64]map[time.Time]bool // project → hours of sessions written (stats rollup)
	report   Report

	// Symbol files go to the blob store when one is configured: the
	// object is written before its row is queued, so a chunk that fails
	// deletes what it wrote (pending), and a committed chunk deletes the
	// objects the rows it replaced pointed at (replaced).
	blobs    blob.Store
	pending  []string
	replaced []*string
}

// endChunk settles the chunk's objects after its transaction.
func (im *importer) endChunk(committed bool) {
	ctx := context.WithoutCancel(im.ctx)
	if committed {
		for _, k := range im.replaced {
			if k != nil && im.blobs != nil {
				im.blobs.Delete(ctx, *k)
			}
		}
	} else {
		for _, k := range im.pending {
			im.blobs.Delete(ctx, k)
		}
	}
	im.pending, im.replaced = nil, nil
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
	im := &importer{ctx: ctx, st: st, projects: map[string]int64{}, batch: &pgx.Batch{}, blobs: st.Blobs,
		dirtyS: map[int64]map[time.Time]bool{}, report: Report{Rows: map[string]int64{}}}
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 1<<20), maxLine)
	line, committed := 0, 0
	for {
		// One transaction per chunk of lines.
		more := true
		err := st.Tx(ctx, func(ctx context.Context, tx pgx.Tx) error {
			im.tx = tx
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
		im.endChunk(err == nil)
		if err == nil && im.blobs != nil {
			// Pack what this chunk spooled before reading the next: the spool
			// then holds at most CommitEvery lines, not the whole file.
			var drainErr error
			if _, lerr := st.RunAsLeader(ctx, store.LeaderPack, func() { _, drainErr = st.Drain(ctx) }); lerr != nil {
				err = lerr
			} else if drainErr != nil {
				err = fmt.Errorf("pack imported payloads: %w", drainErr)
			}
		}
		if err != nil {
			if committed > 0 {
				err = fmt.Errorf("%w (lines 1-%d were committed; import is idempotent, re-run the file)", err, committed)
			}
			return im.report, err
		}
		committed = line
		im.report.Committed = committed
		if !more {
			// With a blob store the payloads are in the spool: pack them now,
			// so the import is complete when the command returns (under the
			// same lock as the running flusher, which otherwise does it).
			var drainErr error
			if _, err := st.RunAsLeader(ctx, store.LeaderPack, func() { _, drainErr = st.Drain(ctx) }); err != nil {
				return im.report, err
			}
			if drainErr != nil {
				return im.report, fmt.Errorf("pack imported payloads: %w", drainErr)
			}
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
		var ignoreUntil *time.Time
		if r.IgnoreUntil != nil {
			ignoreUntil = &r.IgnoreUntil.Time
		}
		im.batch.Queue(upsertIssue, pid, fp, r.Title, r.Level, r.ErrorType, r.Transaction, r.Platform, r.Status, r.StatusBy,
			r.EventCount, r.StoredCount, tsOrNow(r.FirstSeen), tsOrNow(r.LastSeen), r.FirstRelease, r.LastRelease,
			nonNilStrings(r.Releases), r.ResolvedReleases, ignoreUntil, r.IgnoreUntilCount, r.IgnoreUntilEscalating, r.IgnoreBaseline,
			tsOrNow(r.CreatedAt), tsOrNow(r.UpdatedAt))
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
	case "attachments":
		var r attachmentRow
		if err := json.Unmarshal(b, &r); err != nil {
			return err
		}
		pid, err := im.project(r.Project)
		if err != nil {
			return err
		}
		if r.OccurredAt.IsZero() || r.EventID == "" {
			return errors.New("attachments row needs occurred_at and event_id")
		}
		eid, ok := sentry.ParseID(r.EventID)
		if !ok {
			return fmt.Errorf("attachments row: event_id %q is not a 32-hex id", r.EventID)
		}
		if r.Size == 0 {
			r.Size = int64(len(r.Data))
		}
		if r.Data == nil {
			r.Data = []byte{}
		}
		im.batch.Queue(insertAttachment, r.OccurredAt.Time, pid, eid, r.N, orStr(r.Filename, "attachment"), orStr(r.ContentType, "application/octet-stream"),
			orStr(r.AttachmentType, "event.attachment"), r.Size, r.Data)
	case "user_reports":
		var r userReportRow
		if err := json.Unmarshal(b, &r); err != nil {
			return err
		}
		pid, err := im.project(r.Project)
		if err != nil {
			return err
		}
		if r.EventID == "" {
			return errors.New("user_reports row needs event_id")
		}
		eid, ok := sentry.ParseID(r.EventID)
		if !ok {
			return fmt.Errorf("user_reports row: event_id %q is not a 32-hex id", r.EventID)
		}
		im.batch.Queue(upsertUserReport, pid, eid, tsOrNow(r.ReceivedAt), r.Name, r.Email, r.Comments)
	case "project_keys":
		var r projectKeyRow
		if err := json.Unmarshal(b, &r); err != nil {
			return err
		}
		pid, err := im.project(r.Project)
		if err != nil {
			return err
		}
		if r.PublicKey == "" {
			return errors.New("project_keys row needs public_key")
		}
		var lastUsed *time.Time
		if r.LastUsedAt != nil {
			lastUsed = &r.LastUsedAt.Time
		}
		im.batch.Queue(insertProjectKey, pid, r.PublicKey, tsOrNow(r.RetiredAt), lastUsed)
	case "monitors":
		var r monitorRow
		if err := json.Unmarshal(b, &r); err != nil {
			return err
		}
		pid, err := im.project(r.Project)
		if err != nil {
			return err
		}
		if r.Slug == "" {
			return errors.New("monitors row without slug")
		}
		var nextExpected, lastCheckin *time.Time
		if r.NextExpectedAt != nil {
			nextExpected = &r.NextExpectedAt.Time
		}
		if r.LastCheckinAt != nil {
			lastCheckin = &r.LastCheckinAt.Time
		}
		im.batch.Queue(upsertMonitor, pid, r.Slug, r.ScheduleType, r.ScheduleValue, r.ScheduleUnit, r.Timezone,
			r.CheckinMarginMin, r.MaxRuntimeMin, r.FailureThreshold, r.RecoveryThreshold, r.LastStatus,
			r.ConsecutiveFailures, r.ConsecutiveSuccesses, r.Alerting, nextExpected, lastCheckin, tsOrNow(r.CreatedAt))
	case "monitor_checkins":
		var r monitorCheckinRow
		if err := json.Unmarshal(b, &r); err != nil {
			return err
		}
		pid, err := im.project(r.Project)
		if err != nil {
			return err
		}
		if r.StartedAt.IsZero() || r.MonitorSlug == "" || r.CheckInID == "" {
			return errors.New("monitor_checkins row needs started_at, monitor_slug and check_in_id")
		}
		cid, ok := sentry.ParseID(r.CheckInID)
		if !ok {
			return fmt.Errorf("monitor_checkins row: check_in_id %q is not a 32-hex id", r.CheckInID)
		}
		im.batch.Queue(insertMonitorCheckin, r.StartedAt.Time, pid, r.MonitorSlug, cid, r.Status, r.DurationS, r.Release, r.Environment)
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
		// The same write order as an upload (internal/symbolicate/files.go):
		// object first, then the row under the row's lock, which also
		// pins down the key of the row this one replaces.
		release := nilIfEmptyStr(r.Release)
		var data []byte
		var key *string
		if im.blobs != nil {
			k := blob.SymbolKey(pid)
			if err := im.blobs.Put(im.ctx, k, r.Data); err != nil {
				return fmt.Errorf("blob store: %w", err)
			}
			im.pending = append(im.pending, k)
			key = &k
		} else {
			data = r.Data
		}
		if err := store.LockSymbolFile(im.ctx, im.tx, symbolicate.LockKey(pid, r.Kind, release, r.Filename)); err != nil {
			return err
		}
		prev, err := store.SymbolFileBlobKey(im.ctx, im.tx, pid, store.SymbolKind(r.Kind), release, r.Filename)
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		if prev != nil {
			im.replaced = append(im.replaced, prev)
		}
		im.batch.Queue(upsertSymbolFile, pid, r.Kind, release, r.DebugID, r.Filename, r.Size, data, tsOrNow(r.UploadedAt), key)
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
	p, err := store.GetProject(im.ctx, im.tx, slug)
	if errors.Is(err, pgx.ErrNoRows) {
		p, err = store.CreateProject(im.ctx, im.tx, slug, slug, nil, auth.NewProjectKey())
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
		if err := store.MarkSessionStatsDirty(im.ctx, im.tx, pid, slices.Collect(maps.Keys(hours))); err != nil {
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
	return im.st.InsertEvents(im.ctx, im.tx, rows)
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

// orStr is s, or def when empty.
func orStr(s, def string) string {
	if s == "" {
		return def
	}
	return s
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
