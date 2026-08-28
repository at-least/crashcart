-- CrashCart schema. Requires the timescaledb extension (the migrator
-- creates it; the role needs CREATE on the database, or the extension must
-- be pre-installed by a superuser).
--
-- Time-series tables use the microsecond primary key from internal/pk as
-- the TimescaleDB time dimension: id = unix_ms × 1000 + random(0..999).

CREATE EXTENSION IF NOT EXISTS timescaledb;

-- "now" in id units, so retention / compression / refresh policies can
-- reason about the integer dimension.
CREATE OR REPLACE FUNCTION crashcart_now() RETURNS BIGINT
    LANGUAGE SQL STABLE AS $$ SELECT (extract(epoch FROM now()) * 1000000)::bigint $$;

-- The one definition of "crash": fatal, or an unhandled exception. Used by
-- the continuous aggregates, the spike check and the event filters.
CREATE FUNCTION crashcart_is_crash(level TEXT, handled BOOLEAN) RETURNS BOOLEAN
    LANGUAGE SQL IMMUTABLE AS $$ SELECT level = 'fatal' OR handled = false $$;

-- ── projects ───────────────────────────────────────────────────────────

CREATE TABLE projects (
    id                BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    slug              TEXT NOT NULL UNIQUE,
    name              TEXT NOT NULL,
    platform          TEXT,                                   -- ios | android | flutter | web | … (hint only)
    public_key        TEXT NOT NULL UNIQUE,                   -- DSN key: https://<public_key>@host/<id>
    sample_keep_first INTEGER NOT NULL DEFAULT 100,           -- events stored per issue before sampling kicks in
    sample_rate       DOUBLE PRECISION NOT NULL DEFAULT 1.0,  -- kept fraction after that (fatal always kept)
    created_at        TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- ── events (hypertable) ────────────────────────────────────────────────

CREATE TABLE events (
    id             BIGINT PRIMARY KEY,                -- occurred_at µs (pk package)
    project_id     BIGINT NOT NULL,
    event_id       TEXT NOT NULL,                    -- Sentry event_id (or a derived one)
    level          TEXT NOT NULL,                    -- fatal | error | warning | info | debug
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
    symbols        JSONB                             -- symbolicated frames (written once by the worker)
);
SELECT create_hypertable('events', 'id', chunk_time_interval => 86400000000);
SELECT set_integer_now_func('events', 'crashcart_now');
CREATE INDEX events_project_fingerprint ON events (project_id, fingerprint, id DESC);
CREATE INDEX events_project_user ON events (project_id, user_id, id DESC) WHERE user_id IS NOT NULL;
CREATE INDEX events_project_id ON events (project_id, id DESC);
CREATE INDEX events_project_crash ON events (project_id, id DESC) WHERE crashcart_is_crash(level, handled);
ALTER TABLE events SET (
    timescaledb.compress,
    timescaledb.compress_segmentby = 'project_id, fingerprint',
    timescaledb.compress_orderby = 'id DESC'
);

-- ── sessions (hypertable, release health) ──────────────────────────────

CREATE TABLE sessions (
    id          BIGINT PRIMARY KEY,                  -- started_at µs
    project_id  BIGINT NOT NULL,
    release     TEXT NOT NULL,
    environment TEXT,
    status      TEXT NOT NULL,                       -- ok | exited | crashed | errored | abnormal
    count       INTEGER NOT NULL DEFAULT 1           -- aggregate session items carry counts
);
SELECT create_hypertable('sessions', 'id', chunk_time_interval => 86400000000);
SELECT set_integer_now_func('sessions', 'crashcart_now');
CREATE INDEX sessions_project_release ON sessions (project_id, release, id DESC);
ALTER TABLE sessions SET (
    timescaledb.compress,
    timescaledb.compress_segmentby = 'project_id, release',
    timescaledb.compress_orderby = 'id DESC'
);

-- ── issues (stateful; the only non-hypertable ingest upserts) ──────────

CREATE TABLE issues (
    project_id       BIGINT NOT NULL,
    fingerprint      TEXT NOT NULL,
    title            TEXT NOT NULL,
    level            TEXT NOT NULL,
    error_type       TEXT,
    screen           TEXT,
    platform         TEXT,
    status           TEXT NOT NULL DEFAULT 'unresolved'
                     CHECK (status IN ('unresolved', 'triaged', 'resolved', 'ignored', 'regression')),
    event_count      BIGINT NOT NULL DEFAULT 0,       -- exact: counts sampled-out events too
    stored_count     BIGINT NOT NULL DEFAULT 0,       -- events actually stored
    first_seen       BIGINT NOT NULL,                 -- event ids (µs)
    last_seen        BIGINT NOT NULL,
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
    project_id  BIGINT NOT NULL,
    kind        TEXT NOT NULL CHECK (kind IN ('proguard', 'sourcemap', 'dsym')),
    release     TEXT NOT NULL,                       -- '' when matched by debug_id only
    debug_id    TEXT,                                -- dSYM UUID / proguard mapping uuid
    filename    TEXT NOT NULL,
    size        BIGINT NOT NULL,
    data        BYTEA NOT NULL,
    uploaded_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (project_id, kind, release, filename)
);
CREATE INDEX symbol_files_debug_id ON symbol_files (project_id, debug_id) WHERE debug_id IS NOT NULL;

-- ── jobs (Postgres-backed queue: SKIP LOCKED) ──────────────────────────

CREATE TABLE jobs (
    id         BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    kind       TEXT NOT NULL,                        -- symbolicate | alert
    project_id BIGINT NOT NULL,
    args       JSONB NOT NULL DEFAULT '{}'::jsonb,
    run_after  TIMESTAMPTZ NOT NULL DEFAULT now(),
    attempts   INTEGER NOT NULL DEFAULT 0,
    last_error TEXT,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX jobs_run_after ON jobs (run_after, id);

-- ── alerts ─────────────────────────────────────────────────────────────

CREATE TABLE alert_rules (
    project_id       BIGINT NOT NULL,
    type             TEXT NOT NULL CHECK (type IN ('new_issue', 'regression', 'crash_spike')),
    enabled          BOOLEAN NOT NULL DEFAULT true,
    cooldown_minutes INTEGER NOT NULL DEFAULT 60,
    last_triggered   TIMESTAMPTZ,
    PRIMARY KEY (project_id, type)
);

CREATE TABLE alert_channels (
    id         BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    project_id BIGINT NOT NULL,
    kind       TEXT NOT NULL CHECK (kind IN ('webhook', 'telegram')),
    config     JSONB NOT NULL,                       -- webhook: {"url"}; telegram: {"chat_id"}
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX alert_channels_project ON alert_channels (project_id);

-- ── rate limits (fixed 60 s windows) ───────────────────────────────────

CREATE TABLE rate_limits (
    rl_key       TEXT NOT NULL,                      -- sha256 of the credential
    window_start BIGINT NOT NULL,                    -- unix seconds truncated to the minute
    count        INTEGER NOT NULL DEFAULT 1,
    PRIMARY KEY (rl_key, window_start)
);

-- ── continuous aggregates ──────────────────────────────────────────────
-- All buckets are in id units (µs). Real-time aggregation is on so the
-- newest bucket includes rows not yet materialized.

CREATE MATERIALIZED VIEW event_stats_hourly
WITH (timescaledb.continuous, timescaledb.materialized_only = false) AS
SELECT time_bucket(3600000000::bigint, id) AS bucket,
       project_id,
       COALESCE(release, '')  AS release,
       COALESCE(platform, '') AS platform,
       level,
       count(*)                                                        AS events,
       count(*) FILTER (WHERE crashcart_is_crash(level, handled))        AS crashes,
       count(*) FILTER (WHERE level = 'error' AND handled IS NOT false) AS errors
FROM events
GROUP BY 1, 2, 3, 4, 5
WITH NO DATA;

CREATE MATERIALIZED VIEW issue_stats_hourly
WITH (timescaledb.continuous, timescaledb.materialized_only = false) AS
SELECT time_bucket(3600000000::bigint, id) AS bucket,
       project_id,
       fingerprint,
       count(*) AS events
FROM events
WHERE fingerprint IS NOT NULL
GROUP BY 1, 2, 3
WITH NO DATA;

CREATE MATERIALIZED VIEW release_health_daily
WITH (timescaledb.continuous, timescaledb.materialized_only = false) AS
SELECT time_bucket(86400000000::bigint, id) AS bucket,
       project_id,
       release,
       sum(count)                                                    AS total,
       sum(count) FILTER (WHERE status = 'crashed')                  AS crashed,
       sum(count) FILTER (WHERE status IN ('errored', 'abnormal'))   AS errored
FROM sessions
GROUP BY 1, 2, 3
WITH NO DATA;

-- Refresh: every 5 minutes, re-materialize the last 3 hours / 3 days.
SELECT add_continuous_aggregate_policy('event_stats_hourly',
    start_offset => 10800000000::bigint, end_offset => 60000000::bigint,
    schedule_interval => INTERVAL '5 minutes');
SELECT add_continuous_aggregate_policy('issue_stats_hourly',
    start_offset => 10800000000::bigint, end_offset => 60000000::bigint,
    schedule_interval => INTERVAL '5 minutes');
SELECT add_continuous_aggregate_policy('release_health_daily',
    start_offset => 259200000000::bigint, end_offset => 60000000::bigint,
    schedule_interval => INTERVAL '5 minutes');

-- Retention and compression policies are reconciled at startup from
-- RETENTION_DAYS (see internal/retention), not fixed here.
