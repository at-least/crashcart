package db_test

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/at-least/crashcart/internal/db"
	"github.com/at-least/crashcart/internal/testdb"
)

func TestInitIdempotent(t *testing.T) {
	p := testdb.New(t).Pool // already migrated once, by pgtestdb's own goose run
	ctx := context.Background()
	if created, err := db.Init(ctx, p); err != nil || created {
		t.Fatalf("Init on an already-migrated database: created=%v err=%v", created, err)
	}
	var n int
	if err := p.QueryRow(ctx, "SELECT count(*) FROM goose_db_version WHERE version_id = 1 AND is_applied").Scan(&n); err != nil || n != 1 {
		t.Fatalf("baseline migration row: %d %v", n, err)
	}
}

func TestInitRefusesWhenDatabaseAheadOfBinary(t *testing.T) {
	p := testdb.New(t).Pool
	ctx := context.Background()
	if _, err := p.Exec(ctx, "INSERT INTO goose_db_version (version_id, is_applied) VALUES (99, true)"); err != nil {
		t.Fatal(err)
	}
	_, err := db.Init(ctx, p)
	if !errors.Is(err, db.ErrDatabaseAhead) {
		t.Fatalf("Init against a database ahead of this binary: %v", err)
	}
	if !strings.Contains(err.Error(), "99") {
		t.Fatalf("message should name the database's version: %v", err)
	}
}

func TestInitFreshDatabase(t *testing.T) {
	pool := emptyDatabase(t)
	ctx := context.Background()
	created, err := db.Init(ctx, pool)
	if err != nil || !created {
		t.Fatalf("Init on an empty database: created=%v err=%v", created, err)
	}
	var hasProjects, hasLegacy bool
	if err := pool.QueryRow(ctx, "SELECT to_regclass('projects') IS NOT NULL").Scan(&hasProjects); err != nil || !hasProjects {
		t.Fatalf("projects table after Init: exists=%v err=%v", hasProjects, err)
	}
	if err := pool.QueryRow(ctx, "SELECT to_regclass('crashcart_schema') IS NOT NULL").Scan(&hasLegacy); err != nil || hasLegacy {
		t.Fatalf("crashcart_schema should not exist on a fresh database: exists=%v err=%v", hasLegacy, err)
	}
	var v int64
	if err := pool.QueryRow(ctx, "SELECT max(version_id) FROM goose_db_version WHERE is_applied").Scan(&v); err != nil || v != latestMigration {
		t.Fatalf("goose_db_version after Init: %d %v (want %d)", v, err, latestMigration)
	}
}

// latestMigration is the highest file in internal/db/migrations; bump it
// with every new migration so the fresh and legacy tests keep proving Init
// reaches the end.
const latestMigration = 3

func TestInitBootstrapsLegacyDatabase(t *testing.T) {
	pool := legacyDatabase(t, 15)
	ctx := context.Background()
	created, err := db.Init(ctx, pool)
	if err != nil || created {
		t.Fatalf("bootstrap: created=%v err=%v", created, err)
	}
	var exists bool
	if err := pool.QueryRow(ctx, "SELECT to_regclass('crashcart_schema') IS NOT NULL").Scan(&exists); err != nil || exists {
		t.Fatalf("crashcart_schema should be dropped after bootstrap: exists=%v err=%v", exists, err)
	}
	// The baseline is marked applied without running; everything after it
	// (00002 …) is applied as pending on the same start.
	var v int64
	if err := pool.QueryRow(ctx, "SELECT max(version_id) FROM goose_db_version WHERE is_applied").Scan(&v); err != nil || v != latestMigration {
		t.Fatalf("goose_db_version after bootstrap: %d %v (want %d)", v, err, latestMigration)
	}
	var nullable bool
	if err := pool.QueryRow(ctx, "SELECT NOT attnotnull FROM pg_attribute WHERE attrelid = 'symbol_files'::regclass AND attname = 'data'").Scan(&nullable); err != nil || !nullable {
		t.Fatalf("00002 not applied on the bootstrapped database: data nullable=%v %v", nullable, err)
	}
	// Calling Init again is a no-op — the bootstrap doesn't re-run.
	if created, err := db.Init(ctx, pool); err != nil || created {
		t.Fatalf("second Init after bootstrap: created=%v err=%v", created, err)
	}
}

func TestInitRefusesUnknownLegacyVersion(t *testing.T) {
	pool := legacyDatabase(t, 14)
	_, err := db.Init(context.Background(), pool)
	if !errors.Is(err, db.ErrLegacySchemaVersion) {
		t.Fatalf("Init on legacy schema version 14: %v", err)
	}
	if !strings.Contains(err.Error(), "14") {
		t.Fatalf("message should name the database's legacy version: %v", err)
	}
}

// A new project accepts everything by default: sampling bounds the
// database, a daily quota is an explicit cost cap.
func TestProjectDefaults(t *testing.T) {
	p := testdb.New(t).Pool
	ctx := context.Background()
	var quota int32
	var rate float64
	if err := p.QueryRow(ctx, "INSERT INTO projects (slug, name, public_key) VALUES ('d', 'D', 'k') RETURNING daily_quota, sample_rate").Scan(&quota, &rate); err != nil {
		t.Fatal(err)
	}
	if quota != 0 || rate != 1 {
		t.Fatalf("defaults: daily_quota=%d sample_rate=%v (want 0 = unlimited, 1)", quota, rate)
	}
}

func TestConnect(t *testing.T) {
	url := os.Getenv("TEST_DATABASE_URL")
	if url == "" {
		t.Skip("TEST_DATABASE_URL not set")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	pool, err := db.Connect(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	pool.Close()
	if _, err := db.Connect(ctx, "postgres://nobody:nothing@127.0.0.1:1/nowhere?sslmode=disable&connect_timeout=2"); err == nil {
		t.Error("an unreachable server must fail Connect, not the first query")
	}
	if _, err := db.Connect(ctx, "::not a url::"); err == nil {
		t.Error("an unparseable URL must fail")
	}
}

// emptyDatabase creates a fresh, empty database on the TEST_DATABASE_URL
// server (not one of testdb's pgtestdb-cloned databases, which are already
// migrated) and returns a pool connected to it, dropped at cleanup.
func emptyDatabase(t *testing.T) *pgxpool.Pool {
	t.Helper()
	adminURL := os.Getenv("TEST_DATABASE_URL")
	if adminURL == "" {
		t.Skip("TEST_DATABASE_URL not set")
	}
	ctx := context.Background()
	admin, err := pgxpool.New(ctx, adminURL)
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Close()
	name := fmt.Sprintf("legacy_%d_%d", os.Getpid(), time.Now().UnixNano())
	if _, err := admin.Exec(ctx, "CREATE DATABASE "+name); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		a, err := pgxpool.New(context.Background(), adminURL)
		if err != nil {
			return
		}
		defer a.Close()
		a.Exec(context.Background(), "DROP DATABASE IF EXISTS "+name+" WITH (FORCE)")
	})

	u, err := url.Parse(adminURL)
	if err != nil {
		t.Fatal(err)
	}
	u.Path = "/" + name
	pool, err := pgxpool.New(ctx, u.String())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	return pool
}

// legacyDatabase creates a fresh database, applies legacySchema to it, and
// sets crashcart_schema.version to the given version — reproducing what a
// pre-migration binary would have left behind, for
// db.Init's legacy-bootstrap path to run against.
func legacyDatabase(t *testing.T, version int) *pgxpool.Pool {
	t.Helper()
	pool := emptyDatabase(t)
	ctx := context.Background()
	if _, err := pool.Exec(ctx, legacySchema); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, "INSERT INTO crashcart_schema (version) VALUES ($1)", version); err != nil {
		t.Fatal(err)
	}
	return pool
}

// legacySchema is internal/db/schema.sql as it stood through the last
// pre-migration release (schema version 15) — kept verbatim here as a test
// fixture so the legacy-bootstrap tests above can exercise db.Init against
// a database that binary would have created, without internal/db itself
// carrying two schema definitions (the migrations directory is the only
// one) going forward.
const legacySchema = `
CREATE TYPE event_level    AS ENUM ('fatal', 'error', 'warning', 'info', 'debug');
CREATE TYPE session_status AS ENUM ('ok', 'exited', 'crashed', 'errored', 'abnormal');
CREATE TYPE issue_status   AS ENUM ('unresolved', 'resolved', 'ignored', 'regression');
CREATE TYPE symbol_kind    AS ENUM ('proguard', 'sourcemap', 'dsym');
CREATE TYPE job_kind       AS ENUM ('symbolicate', 'resymbolicate', 'alert');
CREATE TYPE alert_type     AS ENUM ('new_issue', 'regression', 'unhandled_spike', 'escalating', 'monitor_failed', 'monitor_recovered');
CREATE TYPE channel_kind   AS ENUM ('webhook', 'slack', 'telegram');
CREATE TYPE checkin_status AS ENUM ('in_progress', 'ok', 'error', 'missed', 'timeout');

CREATE FUNCTION crashcart_bucket(t TIMESTAMPTZ, width BIGINT) RETURNS TIMESTAMPTZ
    LANGUAGE SQL IMMUTABLE AS $$ SELECT to_timestamp(floor(extract(epoch FROM t) / width) * width) $$;
CREATE FUNCTION crashcart_buckets(from_at TIMESTAMPTZ, to_at TIMESTAMPTZ, width BIGINT) RETURNS SETOF TIMESTAMPTZ
    LANGUAGE SQL IMMUTABLE AS $$ SELECT b FROM generate_series(from_at, to_at, make_interval(secs => width)) AS b WHERE b < to_at $$;

CREATE TABLE users (
    id            BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    email         TEXT NOT NULL UNIQUE,
    name          TEXT NOT NULL DEFAULT '',
    password_hash TEXT NOT NULL,
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE user_sessions (
    token_hash BYTEA PRIMARY KEY,
    user_id    BIGINT NOT NULL REFERENCES users ON DELETE CASCADE,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    expires_at TIMESTAMPTZ NOT NULL
);
CREATE INDEX user_sessions_expires ON user_sessions (expires_at);

CREATE TABLE api_keys (
    id           BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    name         TEXT NOT NULL,
    key_hash     BYTEA NOT NULL UNIQUE,
    prefix       TEXT NOT NULL,
    created_by   BIGINT REFERENCES users ON DELETE SET NULL,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    last_used_at TIMESTAMPTZ,
    revoked_at   TIMESTAMPTZ
);

CREATE TABLE projects (
    id                BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    slug              TEXT NOT NULL UNIQUE,
    name              TEXT NOT NULL,
    platform          TEXT,
    public_key        TEXT NOT NULL UNIQUE,
    sample_keep_first INTEGER NOT NULL DEFAULT 100,
    sample_rate       DOUBLE PRECISION NOT NULL DEFAULT 1.0,
    daily_quota       INTEGER NOT NULL DEFAULT 0,
    created_at        TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE project_keys (
    id           BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    project_id   BIGINT NOT NULL REFERENCES projects ON DELETE CASCADE,
    public_key   TEXT NOT NULL UNIQUE,
    retired_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    last_used_at TIMESTAMPTZ
);

CREATE TABLE events (
    occurred_at    TIMESTAMPTZ NOT NULL,
    project_id     BIGINT NOT NULL REFERENCES projects ON DELETE CASCADE,
    event_id       UUID NOT NULL,
    level          event_level NOT NULL,
    message        TEXT NOT NULL,
    platform       TEXT,
    environment    TEXT,
    release        TEXT,
    device_id      TEXT,
    device_model   TEXT,
    os_version     TEXT,
    transaction    TEXT,
    error_type     TEXT,
    culprit        TEXT,
    handled        BOOLEAN,
    sdk_name       TEXT,
    user_id        TEXT,
    fingerprint    UUID,
    symbolicated   BOOLEAN NOT NULL DEFAULT false,
    tags           JSONB NOT NULL DEFAULT '{}'::jsonb,
    symbols        JSONB,
    payload        BYTEA,
    PRIMARY KEY (project_id, event_id, occurred_at)
) PARTITION BY RANGE (occurred_at);
ALTER TABLE events ALTER COLUMN payload SET STORAGE EXTERNAL;
CREATE TABLE events_default PARTITION OF events DEFAULT;
CREATE INDEX events_project_time ON events (project_id, occurred_at DESC, event_id DESC);
CREATE INDEX events_project_fingerprint ON events (project_id, fingerprint, occurred_at DESC) INCLUDE (user_id);
CREATE INDEX events_project_user ON events (project_id, user_id, occurred_at DESC) WHERE user_id IS NOT NULL;
CREATE INDEX events_project_unhandled ON events (project_id, occurred_at DESC) WHERE handled = false;
CREATE INDEX events_tags ON events USING GIN (tags jsonb_path_ops);

CREATE TABLE attachments (
    occurred_at     TIMESTAMPTZ NOT NULL,
    project_id      BIGINT NOT NULL REFERENCES projects ON DELETE CASCADE,
    event_id        UUID NOT NULL,
    n               INTEGER NOT NULL,
    filename        TEXT NOT NULL,
    content_type    TEXT NOT NULL,
    attachment_type TEXT NOT NULL,
    size            BIGINT NOT NULL,
    data            BYTEA NOT NULL,
    PRIMARY KEY (project_id, event_id, occurred_at, n)
) PARTITION BY RANGE (occurred_at);
ALTER TABLE attachments ALTER COLUMN data SET STORAGE EXTERNAL;
CREATE TABLE attachments_default PARTITION OF attachments DEFAULT;

CREATE TABLE user_reports (
    project_id  BIGINT NOT NULL REFERENCES projects ON DELETE CASCADE,
    event_id    UUID NOT NULL,
    received_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    name        TEXT,
    email       TEXT,
    comments    TEXT NOT NULL,
    PRIMARY KEY (project_id, event_id)
);
CREATE INDEX user_reports_project_received ON user_reports (project_id, received_at DESC);

CREATE TABLE client_report_counts (
    project_id BIGINT NOT NULL REFERENCES projects ON DELETE CASCADE,
    bucket     TIMESTAMPTZ NOT NULL,
    reason     TEXT NOT NULL,
    category   TEXT NOT NULL,
    quantity   BIGINT NOT NULL,
    PRIMARY KEY (project_id, bucket, reason, category)
);

CREATE TABLE monitors (
    project_id             BIGINT NOT NULL REFERENCES projects ON DELETE CASCADE,
    slug                   TEXT NOT NULL,
    schedule_type          TEXT NOT NULL,
    schedule_value         TEXT NOT NULL,
    schedule_unit          TEXT,
    timezone               TEXT NOT NULL DEFAULT 'UTC',
    checkin_margin_min     INTEGER NOT NULL DEFAULT 1,
    max_runtime_min        INTEGER NOT NULL DEFAULT 30,
    failure_threshold      INTEGER NOT NULL DEFAULT 1,
    recovery_threshold     INTEGER NOT NULL DEFAULT 1,
    last_status            checkin_status,
    consecutive_failures   INTEGER NOT NULL DEFAULT 0,
    consecutive_successes  INTEGER NOT NULL DEFAULT 0,
    alerting               BOOLEAN NOT NULL DEFAULT false,
    next_expected_at       TIMESTAMPTZ,
    last_checkin_at        TIMESTAMPTZ,
    created_at             TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (project_id, slug)
);

CREATE TABLE monitor_checkins (
    started_at   TIMESTAMPTZ NOT NULL,
    project_id   BIGINT NOT NULL REFERENCES projects ON DELETE CASCADE,
    monitor_slug TEXT NOT NULL,
    check_in_id  UUID NOT NULL,
    status       checkin_status NOT NULL,
    duration_s   REAL,
    release      TEXT,
    environment  TEXT,
    PRIMARY KEY (project_id, monitor_slug, check_in_id, started_at)
) PARTITION BY RANGE (started_at);
CREATE TABLE monitor_checkins_default PARTITION OF monitor_checkins DEFAULT;
CREATE INDEX monitor_checkins_latest_in_progress ON monitor_checkins (project_id, monitor_slug, started_at DESC) WHERE status = 'in_progress';

CREATE TABLE sessions (
    started_at  TIMESTAMPTZ NOT NULL,
    project_id  BIGINT NOT NULL REFERENCES projects ON DELETE CASCADE,
    sid         TEXT NOT NULL,
    release     TEXT NOT NULL,
    environment TEXT,
    status      session_status NOT NULL,
    count       INTEGER NOT NULL DEFAULT 1,
    PRIMARY KEY (project_id, sid, started_at)
) PARTITION BY RANGE (started_at);
CREATE TABLE sessions_default PARTITION OF sessions DEFAULT;
CREATE INDEX sessions_project_release ON sessions (project_id, release, started_at DESC);

CREATE TABLE releases (
    project_id BIGINT NOT NULL REFERENCES projects ON DELETE CASCADE,
    release    TEXT NOT NULL,
    platforms  TEXT[] NOT NULL DEFAULT '{}',
    first_seen TIMESTAMPTZ NOT NULL,
    PRIMARY KEY (project_id, release)
);
CREATE INDEX releases_project_first_seen ON releases (project_id, first_seen DESC);

CREATE TABLE issues (
    project_id       BIGINT NOT NULL REFERENCES projects ON DELETE CASCADE,
    fingerprint      UUID NOT NULL,
    title            TEXT NOT NULL,
    level            event_level NOT NULL,
    error_type       TEXT,
    transaction      TEXT,
    platform         TEXT,
    status           issue_status NOT NULL DEFAULT 'unresolved',
    status_by        TEXT,
    event_count      BIGINT NOT NULL DEFAULT 0,
    stored_count     BIGINT NOT NULL DEFAULT 0,
    first_seen       TIMESTAMPTZ NOT NULL,
    last_seen        TIMESTAMPTZ NOT NULL,
    first_release    TEXT,
    last_release     TEXT,
    releases         TEXT[] NOT NULL DEFAULT '{}',
    resolved_releases TEXT[],
    ignore_until            TIMESTAMPTZ,
    ignore_until_count      BIGINT,
    ignore_until_escalating BOOLEAN NOT NULL DEFAULT false,
    ignore_baseline         BIGINT,
    created_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (project_id, fingerprint)
);
CREATE INDEX issues_project_last_seen ON issues (project_id, last_seen DESC);
CREATE INDEX issues_ignored_conditional ON issues (project_id, fingerprint)
    WHERE status = 'ignored' AND (ignore_until IS NOT NULL OR ignore_until_count IS NOT NULL OR ignore_until_escalating);
CREATE INDEX issues_project_status ON issues (project_id, status, last_seen DESC);
CREATE INDEX issues_project_first_seen ON issues (project_id, first_seen DESC);
CREATE INDEX issues_project_first_release ON issues (project_id, first_release);
CREATE INDEX issues_project_last_release ON issues (project_id, last_release);

CREATE TABLE symbol_files (
    id          BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    project_id  BIGINT NOT NULL REFERENCES projects ON DELETE CASCADE,
    kind        symbol_kind NOT NULL,
    release     TEXT,
    debug_id    TEXT,
    filename    TEXT NOT NULL,
    size        BIGINT NOT NULL,
    data        BYTEA NOT NULL,
    uploaded_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE NULLS NOT DISTINCT (project_id, kind, release, filename)
);
CREATE INDEX symbol_files_debug_id ON symbol_files (project_id, debug_id) WHERE debug_id IS NOT NULL;

CREATE TABLE upload_chunks (
    sha1       TEXT PRIMARY KEY,
    data       BYTEA NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE project_usage (
    project_id BIGINT NOT NULL REFERENCES projects ON DELETE CASCADE,
    day        TIMESTAMPTZ NOT NULL,
    events     BIGINT NOT NULL,
    PRIMARY KEY (project_id, day)
);

CREATE TABLE jobs (
    id         BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    kind       job_kind NOT NULL,
    project_id BIGINT NOT NULL REFERENCES projects ON DELETE CASCADE,
    args       JSONB NOT NULL DEFAULT '{}'::jsonb,
    run_after  TIMESTAMPTZ NOT NULL DEFAULT now(),
    attempts   INTEGER NOT NULL DEFAULT 0,
    locked_until TIMESTAMPTZ,
    last_error TEXT,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX jobs_due ON jobs (run_after, id) WHERE attempts < 8;
CREATE UNIQUE INDEX jobs_pending ON jobs (kind, project_id, args) WHERE attempts < 8;

CREATE TABLE alert_rules (
    project_id       BIGINT NOT NULL REFERENCES projects ON DELETE CASCADE,
    type             alert_type NOT NULL,
    enabled          BOOLEAN NOT NULL DEFAULT true,
    cooldown_minutes INTEGER NOT NULL DEFAULT 60,
    last_triggered   TIMESTAMPTZ,
    PRIMARY KEY (project_id, type)
);

CREATE TABLE alert_channels (
    id         BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    project_id BIGINT NOT NULL REFERENCES projects ON DELETE CASCADE,
    kind       channel_kind NOT NULL,
    config     JSONB NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX alert_channels_project ON alert_channels (project_id);

CREATE FUNCTION crashcart_notify_job() RETURNS TRIGGER
    LANGUAGE plpgsql AS $$ BEGIN PERFORM pg_notify('crashcart_jobs', ''); RETURN NULL; END $$;
CREATE TRIGGER jobs_notify AFTER INSERT ON jobs
    FOR EACH STATEMENT EXECUTE FUNCTION crashcart_notify_job();

CREATE FUNCTION crashcart_notify_issue() RETURNS TRIGGER
    LANGUAGE plpgsql AS $$ BEGIN PERFORM pg_notify('crashcart_issues', NEW.project_id::text); RETURN NULL; END $$;
CREATE TRIGGER issues_notify_insert AFTER INSERT ON issues
    FOR EACH ROW EXECUTE FUNCTION crashcart_notify_issue();
CREATE TRIGGER issues_notify_regression AFTER UPDATE OF status ON issues
    FOR EACH ROW WHEN (NEW.status = 'regression' AND OLD.status IS DISTINCT FROM NEW.status)
    EXECUTE FUNCTION crashcart_notify_issue();

CREATE TABLE event_stats_dirty (
    project_id BIGINT NOT NULL REFERENCES projects ON DELETE CASCADE,
    bucket     TIMESTAMPTZ NOT NULL,
    gen        BIGINT NOT NULL DEFAULT 1,
    PRIMARY KEY (project_id, bucket)
);
CREATE TABLE session_stats_dirty (
    project_id BIGINT NOT NULL REFERENCES projects ON DELETE CASCADE,
    bucket     TIMESTAMPTZ NOT NULL,
    gen        BIGINT NOT NULL DEFAULT 1,
    PRIMARY KEY (project_id, bucket)
);

CREATE TABLE event_stats_hourly_rolled (
    bucket     TIMESTAMPTZ NOT NULL,
    project_id BIGINT NOT NULL REFERENCES projects ON DELETE CASCADE,
    release    TEXT NOT NULL,
    platform   TEXT NOT NULL,
    level      event_level NOT NULL,
    events     BIGINT NOT NULL,
    unhandled    BIGINT NOT NULL,
    errors     BIGINT NOT NULL,
    PRIMARY KEY (project_id, bucket, release, platform, level)
);
CREATE TABLE issue_stats_hourly_rolled (
    bucket      TIMESTAMPTZ NOT NULL,
    project_id  BIGINT NOT NULL REFERENCES projects ON DELETE CASCADE,
    fingerprint UUID NOT NULL,
    events      BIGINT NOT NULL,
    PRIMARY KEY (project_id, fingerprint, bucket)
);
CREATE INDEX issue_stats_hourly_rolled_bucket ON issue_stats_hourly_rolled (project_id, bucket);
CREATE INDEX event_stats_hourly_rolled_expiry ON event_stats_hourly_rolled (bucket);
CREATE INDEX issue_stats_hourly_rolled_expiry ON issue_stats_hourly_rolled (bucket);
CREATE TABLE release_health_hourly_rolled (
    bucket     TIMESTAMPTZ NOT NULL,
    project_id BIGINT NOT NULL REFERENCES projects ON DELETE CASCADE,
    release    TEXT NOT NULL,
    total      BIGINT NOT NULL,
    crashed    BIGINT NOT NULL,
    errored    BIGINT NOT NULL,
    PRIMARY KEY (project_id, bucket, release)
);
CREATE INDEX release_health_hourly_rolled_expiry ON release_health_hourly_rolled (bucket);

CREATE VIEW event_stats_hourly AS
SELECT r.bucket, r.project_id, r.release, r.platform, r.level, r.events, r.unhandled, r.errors
FROM event_stats_hourly_rolled r
WHERE NOT EXISTS (SELECT 1 FROM event_stats_dirty d WHERE d.project_id = r.project_id AND d.bucket = r.bucket)
UNION ALL
SELECT d.bucket, d.project_id, COALESCE(e.release, ''), COALESCE(e.platform, ''), e.level,
       count(*)::bigint,
       count(*) FILTER (WHERE e.handled = false)::bigint,
       count(*) FILTER (WHERE e.level = 'error' AND e.handled IS NOT false)::bigint
FROM event_stats_dirty d
JOIN events e ON e.project_id = d.project_id AND e.occurred_at >= d.bucket AND e.occurred_at < d.bucket + INTERVAL '1 hour'
GROUP BY d.bucket, d.project_id, 3, 4, e.level;

CREATE VIEW issue_stats_hourly AS
SELECT r.bucket, r.project_id, r.fingerprint, r.events
FROM issue_stats_hourly_rolled r
WHERE NOT EXISTS (SELECT 1 FROM event_stats_dirty d WHERE d.project_id = r.project_id AND d.bucket = r.bucket)
UNION ALL
SELECT d.bucket, d.project_id, e.fingerprint, count(*)::bigint
FROM event_stats_dirty d
JOIN events e ON e.project_id = d.project_id AND e.occurred_at >= d.bucket AND e.occurred_at < d.bucket + INTERVAL '1 hour'
WHERE e.fingerprint IS NOT NULL
GROUP BY d.bucket, d.project_id, e.fingerprint;

CREATE VIEW release_health_hourly AS
SELECT r.bucket, r.project_id, r.release, r.total, r.crashed, r.errored
FROM release_health_hourly_rolled r
WHERE NOT EXISTS (SELECT 1 FROM session_stats_dirty d WHERE d.project_id = r.project_id AND d.bucket = r.bucket)
UNION ALL
SELECT d.bucket, d.project_id, s.release,
       sum(s.count)::bigint,
       COALESCE(sum(s.count) FILTER (WHERE s.status = 'crashed'), 0)::bigint,
       COALESCE(sum(s.count) FILTER (WHERE s.status IN ('errored', 'abnormal')), 0)::bigint
FROM session_stats_dirty d
JOIN sessions s ON s.project_id = d.project_id AND s.started_at >= d.bucket AND s.started_at < d.bucket + INTERVAL '1 hour'
GROUP BY d.bucket, d.project_id, s.release;

CREATE FUNCTION crashcart_event_stats(pid BIGINT, from_at TIMESTAMPTZ, to_at TIMESTAMPTZ)
RETURNS SETOF event_stats_hourly
LANGUAGE SQL STABLE AS $$
    SELECT bucket, project_id, release, platform, level, events, unhandled, errors FROM event_stats_hourly
    WHERE project_id = pid AND bucket >= from_at AND bucket < to_at
$$;

CREATE TABLE crashcart_schema (
    version    INTEGER NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
`
