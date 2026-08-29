-- CrashCart schema: the tables every deployment has. The migrator then
-- applies exactly one of 0002_timescale.sql (hypertables, compression,
-- continuous aggregates) or 0002_plain.sql (plain Postgres: rolled-up
-- stats tables behind views with a live current hour) — see internal/db.
--
-- Time-series tables (events, sessions) carry their time in a TIMESTAMPTZ
-- column that is the hypertable dimension; a time window is a range on it.
-- Their unique key includes that column (a hypertable requires it).

-- The one definition of "crash": fatal, or an unhandled exception. Used by
-- the continuous aggregates, the spike check and the event filters.
-- Enumerations: one definition, sqlc generates the Go constants.
CREATE TYPE event_level    AS ENUM ('fatal', 'error', 'warning', 'info', 'debug');
CREATE TYPE session_status AS ENUM ('ok', 'exited', 'crashed', 'errored', 'abnormal');
CREATE TYPE issue_status   AS ENUM ('unresolved', 'triaged', 'resolved', 'ignored', 'regression');
CREATE TYPE symbol_kind    AS ENUM ('proguard', 'sourcemap', 'dsym');
CREATE TYPE job_kind       AS ENUM ('symbolicate', 'resymbolicate', 'alert');
CREATE TYPE alert_type     AS ENUM ('new_issue', 'regression', 'crash_spike');
CREATE TYPE channel_kind   AS ENUM ('webhook', 'telegram');

CREATE FUNCTION crashcart_is_crash(level event_level, handled BOOLEAN) RETURNS BOOLEAN
    LANGUAGE SQL IMMUTABLE AS $$ SELECT level = 'fatal' OR handled = false $$;

-- Time buckets of any width in seconds, epoch/UTC-aligned like Go's
-- t.Truncate(width): the chart queries fold the hourly aggregates with
-- these (4 h / 1 d buckets for longer windows) and gap-fill with
-- crashcart_buckets, so every bucket of a window comes back from SQL.
CREATE FUNCTION crashcart_bucket(t TIMESTAMPTZ, width BIGINT) RETURNS TIMESTAMPTZ
    LANGUAGE SQL IMMUTABLE AS $$ SELECT to_timestamp(floor(extract(epoch FROM t) / width) * width) $$;
CREATE FUNCTION crashcart_buckets(from_at TIMESTAMPTZ, to_at TIMESTAMPTZ, width BIGINT) RETURNS SETOF TIMESTAMPTZ
    LANGUAGE SQL IMMUTABLE AS $$ SELECT b FROM generate_series(from_at, to_at, make_interval(secs => width)) AS b WHERE b < to_at $$;

-- ── projects ───────────────────────────────────────────────────────────

CREATE TABLE projects (
    id                BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    slug              TEXT NOT NULL UNIQUE,
    name              TEXT NOT NULL,
    platform          TEXT,                                   -- ios | android | flutter | web | … (hint only)
    public_key        TEXT NOT NULL UNIQUE,                   -- DSN key: https://<public_key>@host/<id>
    sample_keep_first INTEGER NOT NULL DEFAULT 100,           -- events stored per issue before sampling kicks in
    sample_rate       DOUBLE PRECISION NOT NULL DEFAULT 1.0,  -- kept fraction after that (fatal always kept)
    daily_quota       INTEGER NOT NULL DEFAULT 100000,        -- events accepted per UTC day; 0 = unlimited
    created_at        TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- ── events ─────────────────────────────────────────────────────────────

CREATE TABLE events (
    occurred_at    TIMESTAMPTZ NOT NULL,              -- event timestamp (time dimension)
    project_id     BIGINT NOT NULL REFERENCES projects ON DELETE CASCADE,
    event_id       TEXT NOT NULL,                    -- Sentry event_id (or a derived one); the dedupe key
    level          event_level NOT NULL,                    -- fatal | error | warning | info | debug
    message        TEXT NOT NULL,
    platform       TEXT,
    environment    TEXT,
    release        TEXT,
    device_id      TEXT,
    device_model   TEXT,
    os_version     TEXT,
    screen         TEXT,                             -- event.transaction
    error_type     TEXT,                             -- exception.values[0].type
    error_location TEXT,                             -- deepest in-app frame "File.ext:line"
    handled        BOOLEAN,                          -- false = crash, true = caught, null = no exception
    sdk_name       TEXT,
    user_id        TEXT,
    fingerprint    TEXT,                             -- issues.fingerprint (null when nothing to group)
    symbolicated   BOOLEAN NOT NULL DEFAULT false,
    tags           JSONB NOT NULL DEFAULT '{}'::jsonb,
    breadcrumbs    JSONB NOT NULL DEFAULT '[]'::jsonb,
    payload        JSONB NOT NULL,                   -- the untouched Sentry event; never rewritten
    symbols        JSONB,                            -- symbolicated frames (written once)
    PRIMARY KEY (project_id, event_id, occurred_at)  -- a resent envelope lands on the same key
);
CREATE INDEX events_project_time ON events (project_id, occurred_at DESC, event_id DESC);
CREATE INDEX events_project_fingerprint ON events (project_id, fingerprint, occurred_at DESC);
CREATE INDEX events_project_user ON events (project_id, user_id, occurred_at DESC) WHERE user_id IS NOT NULL;
CREATE INDEX events_project_crash ON events (project_id, occurred_at DESC) WHERE crashcart_is_crash(level, handled);
CREATE INDEX events_tags ON events USING GIN (tags jsonb_path_ops); -- tag filters are `tags @> {k: v}`

-- ── sessions (release health) ──────────────────────────────────────────

CREATE TABLE sessions (
    started_at  TIMESTAMPTZ NOT NULL,                -- time dimension
    project_id  BIGINT NOT NULL REFERENCES projects ON DELETE CASCADE,
    sid         TEXT NOT NULL,                       -- SDK session id; aggregate rows get a random one
    release     TEXT NOT NULL,
    environment TEXT,
    status      session_status NOT NULL,                       -- ok | exited | crashed | errored | abnormal
    count       INTEGER NOT NULL DEFAULT 1,          -- aggregate session items carry counts
    PRIMARY KEY (project_id, sid, started_at)        -- updates of one session hit the same row
);
CREATE INDEX sessions_project_release ON sessions (project_id, release, started_at DESC);

-- ── issues (stateful; the only non-hypertable ingest upserts) ──────────

CREATE TABLE issues (
    project_id       BIGINT NOT NULL REFERENCES projects ON DELETE CASCADE,
    fingerprint      TEXT NOT NULL,
    title            TEXT NOT NULL,
    level            event_level NOT NULL,
    error_type       TEXT,
    screen           TEXT,
    platform         TEXT,
    status           issue_status NOT NULL DEFAULT 'unresolved',
    event_count      BIGINT NOT NULL DEFAULT 0,       -- exact: counts sampled-out events too
    stored_count     BIGINT NOT NULL DEFAULT 0,       -- events actually stored
    first_seen       TIMESTAMPTZ NOT NULL,
    last_seen        TIMESTAMPTZ NOT NULL,
    first_release    TEXT,
    last_release     TEXT,
    resolved_release TEXT,                            -- last_release at resolve time (regression detection)
    created_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (project_id, fingerprint)
);
CREATE INDEX issues_project_last_seen ON issues (project_id, last_seen DESC);
CREATE INDEX issues_project_status ON issues (project_id, status, last_seen DESC);

-- ── symbol files ───────────────────────────────────────────────────────

CREATE TABLE symbol_files (
    id          BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    project_id  BIGINT NOT NULL REFERENCES projects ON DELETE CASCADE,
    kind        symbol_kind NOT NULL,
    release     TEXT,                                -- NULL when matched by debug_id only
    debug_id    TEXT,                                -- dSYM UUID / proguard mapping uuid
    filename    TEXT NOT NULL,
    size        BIGINT NOT NULL,
    data        BYTEA NOT NULL,
    uploaded_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE NULLS NOT DISTINCT (project_id, kind, release, filename)
);
CREATE INDEX symbol_files_debug_id ON symbol_files (project_id, debug_id) WHERE debug_id IS NOT NULL;

-- ── upload chunks (sentry-cli chunked upload; assembled into symbol_files) ──

CREATE TABLE upload_chunks (
    sha1       TEXT PRIMARY KEY,
    data       BYTEA NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- ── project usage (daily quota) ────────────────────────────────────────
-- One row per project and UTC day, bumped in the ingest transaction; the
-- envelope that would push it past projects.daily_quota is rolled back.

CREATE TABLE project_usage (
    project_id BIGINT NOT NULL REFERENCES projects ON DELETE CASCADE,
    day        TIMESTAMPTZ NOT NULL,                 -- UTC midnight
    events     BIGINT NOT NULL,                      -- events received (stored or sampled out)
    PRIMARY KEY (project_id, day)
);

-- ── jobs (Postgres-backed queue: SKIP LOCKED) ──────────────────────────

CREATE TABLE jobs (
    id         BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    kind       job_kind NOT NULL,                        -- symbolicate | resymbolicate | alert
    project_id BIGINT NOT NULL REFERENCES projects ON DELETE CASCADE,
    args       JSONB NOT NULL DEFAULT '{}'::jsonb,
    run_after  TIMESTAMPTZ NOT NULL DEFAULT now(),
    attempts   INTEGER NOT NULL DEFAULT 0,       -- counted when claimed
    locked_until TIMESTAMPTZ,                     -- a worker's lease; NULL = free
    last_error TEXT,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX jobs_run_after ON jobs (run_after, id);
-- One pending job per (kind, project, args): enqueues use ON CONFLICT DO NOTHING.
CREATE UNIQUE INDEX jobs_pending ON jobs (kind, project_id, args) WHERE locked_until IS NULL AND attempts < 8;

-- ── alerts ─────────────────────────────────────────────────────────────

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
    config     JSONB NOT NULL,                       -- webhook: {"url"}; telegram: {"chat_id"}
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX alert_channels_project ON alert_channels (project_id);

-- ── notifications (LISTEN/NOTIFY; internal/store.Listener) ─────────────
-- Fired on commit: a job worker wakes as soon as a job is queued, and the
-- viewer's SSE stream re-counts when a project gains an issue or a
-- regression. Polling stays as the fallback on both sides.

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
