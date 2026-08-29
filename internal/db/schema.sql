-- CrashCart schema, created whole on the first start against an empty
-- database (internal/db.Init); there is no migration history. Plain
-- Postgres 14+: no extensions. The time-series tables (events, sessions)
-- are partitioned by week on their TIMESTAMPTZ time column (a time window
-- is a range on it; their primary key includes it, as partitioning
-- requires); internal/retention creates the partitions ahead of time and
-- drops the expired ones. The statistics are rollup tables kept current by
-- a dirty-key job (the stats section at the end of this file). Event
-- payloads, symbol files and upload chunks are not here at all: they live
-- in the object store (internal/blob), addressed by the row's key.

-- The one definition of "crash": fatal, or an unhandled exception. Used by
-- the rollups, the spike check and the event filters.
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
-- t.Truncate(width): the chart queries fold the hourly rollups with
-- these (4 h / 1 d buckets for longer windows) and gap-fill with
-- crashcart_buckets, so every bucket of a window comes back from SQL.
CREATE FUNCTION crashcart_bucket(t TIMESTAMPTZ, width BIGINT) RETURNS TIMESTAMPTZ
    LANGUAGE SQL IMMUTABLE AS $$ SELECT to_timestamp(floor(extract(epoch FROM t) / width) * width) $$;
CREATE FUNCTION crashcart_buckets(from_at TIMESTAMPTZ, to_at TIMESTAMPTZ, width BIGINT) RETURNS SETOF TIMESTAMPTZ
    LANGUAGE SQL IMMUTABLE AS $$ SELECT b FROM generate_series(from_at, to_at, make_interval(secs => width)) AS b WHERE b < to_at $$;

-- ── users, sessions, API keys ──────────────────────────────────────────
-- Access to the viewer is a user account (bcrypt password) with a session
-- cookie whose token is stored hashed; access to /api/* is an API key,
-- also stored hashed (the secret is shown once when created). The first
-- user is created on the /setup page (only while there are none) or with
-- `crashcart user add`.

CREATE TABLE users (
    id            BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    email         TEXT NOT NULL UNIQUE,                 -- lowercased
    name          TEXT NOT NULL DEFAULT '',
    password_hash TEXT NOT NULL,                        -- bcrypt
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE user_sessions (
    token_hash BYTEA PRIMARY KEY,                       -- sha256 of the cookie token
    user_id    BIGINT NOT NULL REFERENCES users ON DELETE CASCADE,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    expires_at TIMESTAMPTZ NOT NULL
);
CREATE INDEX user_sessions_expires ON user_sessions (expires_at);

CREATE TABLE api_keys (
    id           BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    name         TEXT NOT NULL,
    key_hash     BYTEA NOT NULL UNIQUE,                 -- sha256 of the secret
    prefix       TEXT NOT NULL,                         -- the secret's first characters, for display
    created_by   BIGINT REFERENCES users ON DELETE SET NULL,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    last_used_at TIMESTAMPTZ,
    revoked_at   TIMESTAMPTZ
);

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

-- Partitioned by week of occurred_at (internal/retention creates the
-- partitions ahead and drops expired ones; retention is a DROP TABLE). The
-- DEFAULT partition catches what no weekly partition covers — a device with
-- a wrong clock — so an insert never fails for want of a partition; the
-- partition job moves such rows into a real partition when it creates one.
-- The payload (the event as the SDK sent it) goes through payload_spool
-- into a pack of many payloads in the object store; payload_ref says
-- which and where.
CREATE TABLE events (
    occurred_at    TIMESTAMPTZ NOT NULL,              -- event timestamp (partition key)
    project_id     BIGINT NOT NULL REFERENCES projects ON DELETE CASCADE,
    event_id       UUID NOT NULL,                    -- Sentry event_id (or a derived one); the dedupe key
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
    fingerprint    UUID,                             -- issues.fingerprint (null when nothing to group)
    symbolicated   BOOLEAN NOT NULL DEFAULT false,
    tags           JSONB NOT NULL DEFAULT '{}'::jsonb,
    symbols        JSONB,                            -- symbolicated frames (written once)
    payload_ref    TEXT,                             -- blob.Ref of the raw event in its pack (NULL: still in payload_spool, or never stored)
    PRIMARY KEY (project_id, event_id, occurred_at)  -- a resent envelope lands on the same key
) PARTITION BY RANGE (occurred_at);
CREATE TABLE events_default PARTITION OF events DEFAULT;
CREATE INDEX events_project_time ON events (project_id, occurred_at DESC, event_id DESC);
CREATE INDEX events_project_fingerprint ON events (project_id, fingerprint, occurred_at DESC);
CREATE INDEX events_project_user ON events (project_id, user_id, occurred_at DESC) WHERE user_id IS NOT NULL;
CREATE INDEX events_project_crash ON events (project_id, occurred_at DESC) WHERE crashcart_is_crash(level, handled);
CREATE INDEX events_tags ON events USING GIN (tags jsonb_path_ops); -- tag filters are `tags @> {k: v}`


-- ── payload spool ──────────────────────────────────────────────────────
-- The raw payload of a stored event, written in the ingest transaction
-- (so it is exactly as durable as the event row) and drained a batch at a
-- time into a pack object in the object store (retention.PackPayloads),
-- which then sets events.payload_ref and deletes the row. The table only
-- ever holds the last few seconds of payloads — longer if the object
-- store is down, which is the point.

CREATE TABLE payload_spool (
    project_id  BIGINT NOT NULL REFERENCES projects ON DELETE CASCADE,
    event_id    UUID NOT NULL,
    occurred_at TIMESTAMPTZ NOT NULL,
    data        BYTEA NOT NULL,                      -- the payload, gzipped (as it will sit in the pack)
    size        INTEGER NOT NULL,                    -- octet_length(data); the pack trigger sums it without touching data
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (project_id, event_id, occurred_at)
);
CREATE INDEX payload_spool_created ON payload_spool (created_at);

-- ── sessions (release health) ──────────────────────────────────────────

CREATE TABLE sessions (
    started_at  TIMESTAMPTZ NOT NULL,                -- partition key
    project_id  BIGINT NOT NULL REFERENCES projects ON DELETE CASCADE,
    sid         TEXT NOT NULL,                       -- SDK session id; aggregate rows get a random one
    release     TEXT NOT NULL,
    environment TEXT,
    status      session_status NOT NULL,                       -- ok | exited | crashed | errored | abnormal
    count       INTEGER NOT NULL DEFAULT 1,          -- aggregate session items carry counts
    PRIMARY KEY (project_id, sid, started_at)        -- updates of one session hit the same row
) PARTITION BY RANGE (started_at);
CREATE TABLE sessions_default PARTITION OF sessions DEFAULT;
CREATE INDEX sessions_project_release ON sessions (project_id, release, started_at DESC);

-- ── releases ───────────────────────────────────────────────────────────
-- Every release a project has ever reported (events or sessions), with the
-- platforms seen on it. Upserted at ingest; the update is a no-op unless a
-- new platform or an earlier first_seen shows up, so it is not a hot row.
-- Issues and stats keep the release as text (no FK): a release is a fact
-- about events, this table is the index of them.

CREATE TABLE releases (
    project_id BIGINT NOT NULL REFERENCES projects ON DELETE CASCADE,
    release    TEXT NOT NULL,
    platforms  TEXT[] NOT NULL DEFAULT '{}',
    first_seen TIMESTAMPTZ NOT NULL,
    PRIMARY KEY (project_id, release)
);
CREATE INDEX releases_project_first_seen ON releases (project_id, first_seen DESC);

-- ── issues (stateful; the only non-hypertable ingest upserts) ──────────

CREATE TABLE issues (
    project_id       BIGINT NOT NULL REFERENCES projects ON DELETE CASCADE,
    fingerprint      UUID NOT NULL,
    title            TEXT NOT NULL,
    level            event_level NOT NULL,
    error_type       TEXT,
    screen           TEXT,
    platform         TEXT,
    status           issue_status NOT NULL DEFAULT 'unresolved',
    status_by        TEXT,                            -- who set the status last: a user's email or an API key's name
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
-- The bytes are in the object store at blob.SymbolKey(project_id, id);
-- sentry-cli's upload chunks go straight there too (blob.ChunkKey) and
-- expire by lifecycle rule.

CREATE TABLE symbol_files (
    id          BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    project_id  BIGINT NOT NULL REFERENCES projects ON DELETE CASCADE,
    kind        symbol_kind NOT NULL,
    release     TEXT,                                -- NULL when matched by debug_id only
    debug_id    TEXT,                                -- dSYM UUID / proguard mapping uuid
    filename    TEXT NOT NULL,
    size        BIGINT NOT NULL,
    uploaded_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE NULLS NOT DISTINCT (project_id, kind, release, filename)
);
CREATE INDEX symbol_files_debug_id ON symbol_files (project_id, debug_id) WHERE debug_id IS NOT NULL;

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

-- ── statistics: rollup tables, dirty keys, views ───────────────────────
-- The charts read hourly rollups (one row per project, hour and dimension)
-- instead of the raw tables. They are kept by a dirty-key job rather than
-- at ingest: a write to events / sessions marks its (project, hour) dirty
-- in the same transaction (a hot row per project and hour, no aggregate
-- row is touched); the views below read the rollup for clean hours and
-- compute dirty hours live from the raw table, so they are exact at every
-- moment — including for events that arrive days after they occurred, the
-- normal case for a crash sent on the next app launch. internal/retention
-- recomputes dirty hours every minute (all of them, from the raw rows: an
-- update — a session's status, an event's fingerprint after
-- symbolication — is handled like an insert) and clears the keys whose
-- gen did not move meanwhile. Buckets are UTC hour starts; the rollups
-- keep 400 days, longer than the raw rows.

CREATE TABLE event_stats_dirty (
    project_id BIGINT NOT NULL REFERENCES projects ON DELETE CASCADE,
    bucket     TIMESTAMPTZ NOT NULL,                 -- hour of occurred_at
    gen        BIGINT NOT NULL DEFAULT 1,            -- bumped on every mark; the job clears a key only at the gen it read
    PRIMARY KEY (project_id, bucket)
);
CREATE TABLE session_stats_dirty (
    project_id BIGINT NOT NULL REFERENCES projects ON DELETE CASCADE,
    bucket     TIMESTAMPTZ NOT NULL,                 -- hour of started_at
    gen        BIGINT NOT NULL DEFAULT 1,
    PRIMARY KEY (project_id, bucket)
);

CREATE TABLE event_stats_hourly_rolled (
    bucket     TIMESTAMPTZ NOT NULL,
    project_id BIGINT NOT NULL REFERENCES projects ON DELETE CASCADE,
    release    TEXT NOT NULL,                        -- '' when the event had none
    platform   TEXT NOT NULL,
    level      event_level NOT NULL,
    events     BIGINT NOT NULL,
    crashes    BIGINT NOT NULL,
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
CREATE TABLE release_health_hourly_rolled (
    bucket     TIMESTAMPTZ NOT NULL,
    project_id BIGINT NOT NULL REFERENCES projects ON DELETE CASCADE,
    release    TEXT NOT NULL,
    total      BIGINT NOT NULL,
    crashed    BIGINT NOT NULL,
    errored    BIGINT NOT NULL,
    PRIMARY KEY (project_id, bucket, release)
);

-- The live half of each view: the dirty hours, aggregated from the raw
-- rows. (A dirty key joins its hour of events through the
-- events_project_time index.)
CREATE VIEW event_stats_hourly AS
SELECT r.bucket, r.project_id, r.release, r.platform, r.level, r.events, r.crashes, r.errors
FROM event_stats_hourly_rolled r
WHERE NOT EXISTS (SELECT 1 FROM event_stats_dirty d WHERE d.project_id = r.project_id AND d.bucket = r.bucket)
UNION ALL
SELECT d.bucket, d.project_id, COALESCE(e.release, ''), COALESCE(e.platform, ''), e.level,
       count(*)::bigint,
       count(*) FILTER (WHERE crashcart_is_crash(e.level, e.handled))::bigint,
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

-- crashcart_event_stats is the chart queries' one source: the hourly rows
-- of a project in a window. Inlined into the caller. (width is the bucket
-- width the caller folds to; kept in the signature so the queries read
-- the same whatever the source's granularity.)
CREATE FUNCTION crashcart_event_stats(pid BIGINT, from_at TIMESTAMPTZ, to_at TIMESTAMPTZ, width BIGINT)
RETURNS SETOF event_stats_hourly
LANGUAGE SQL STABLE AS $$
    SELECT bucket, project_id, release, platform, level, events, crashes, errors FROM event_stats_hourly
    WHERE project_id = pid AND bucket >= from_at AND bucket < to_at
$$;
