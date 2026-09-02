-- Enumerations: one definition, sqlc generates the Go constants.
CREATE TYPE event_level    AS ENUM ('fatal', 'error', 'warning', 'info', 'debug');
CREATE TYPE session_status AS ENUM ('ok', 'exited', 'crashed', 'errored', 'abnormal');
CREATE TYPE issue_status   AS ENUM ('unresolved', 'resolved', 'ignored', 'regression');
CREATE TYPE symbol_kind    AS ENUM ('proguard', 'sourcemap', 'dsym');
CREATE TYPE job_kind       AS ENUM ('symbolicate', 'resymbolicate', 'alert');
CREATE TYPE alert_type     AS ENUM ('new_issue', 'regression', 'unhandled_spike', 'escalating');
CREATE TYPE channel_kind   AS ENUM ('webhook', 'telegram');

-- Time buckets of any width in seconds, Unix-epoch-aligned (equal to Go's t.Truncate(width) for widths
-- that divide a day; a 7 d width would be Thursday-aligned): the chart queries fold the hourly aggregates with
-- these (4 h / 1 d buckets for longer windows) and gap-fill with
-- crashcart_buckets, so every bucket of a window comes back from SQL.
CREATE FUNCTION crashcart_bucket(t TIMESTAMPTZ, width BIGINT) RETURNS TIMESTAMPTZ
    LANGUAGE SQL IMMUTABLE AS $$ SELECT to_timestamp(floor(extract(epoch FROM t) / width) * width) $$;
CREATE FUNCTION crashcart_buckets(from_at TIMESTAMPTZ, to_at TIMESTAMPTZ, width BIGINT) RETURNS SETOF TIMESTAMPTZ
    LANGUAGE SQL IMMUTABLE AS $$ SELECT b FROM generate_series(from_at, to_at, make_interval(secs => width)) AS b WHERE b < to_at $$;

-- sqlc-only mirror of internal/db/schema.sql (no partitioning DDL). The
-- stats views appear here as plain tables so queries type-check. Keep in
-- sync with schema.sql.

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
    transaction         TEXT,
    error_type     TEXT,
    culprit TEXT,
    handled        BOOLEAN,
    sdk_name       TEXT,
    user_id        TEXT,
    fingerprint    UUID,
    symbolicated   BOOLEAN NOT NULL DEFAULT false,
    tags           JSONB NOT NULL DEFAULT '{}'::jsonb,
    symbols        JSONB,
    payload        BYTEA,
    PRIMARY KEY (project_id, event_id, occurred_at)
);

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
);

CREATE TABLE user_reports (
    project_id  BIGINT NOT NULL REFERENCES projects ON DELETE CASCADE,
    event_id    UUID NOT NULL,
    received_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    name        TEXT,
    email       TEXT,
    comments    TEXT NOT NULL,
    PRIMARY KEY (project_id, event_id)
);

CREATE TABLE sessions (
    started_at  TIMESTAMPTZ NOT NULL,
    project_id  BIGINT NOT NULL REFERENCES projects ON DELETE CASCADE,
    sid         TEXT NOT NULL,
    release     TEXT NOT NULL,
    environment TEXT,
    status      session_status NOT NULL,
    count       INTEGER NOT NULL DEFAULT 1,
    PRIMARY KEY (project_id, sid, started_at)
);

CREATE TABLE releases (
    project_id BIGINT NOT NULL REFERENCES projects ON DELETE CASCADE,
    release    TEXT NOT NULL,
    platforms  TEXT[] NOT NULL DEFAULT '{}',
    first_seen TIMESTAMPTZ NOT NULL,
    PRIMARY KEY (project_id, release)
);

CREATE TABLE issues (
    project_id       BIGINT NOT NULL REFERENCES projects ON DELETE CASCADE,
    fingerprint      UUID NOT NULL,
    title            TEXT NOT NULL,
    level            event_level NOT NULL,
    error_type       TEXT,
    transaction           TEXT,
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


-- stats: dirty keys, rollup tables, and the views (as tables for sqlc)
CREATE TABLE event_stats_dirty (
    project_id BIGINT NOT NULL,
    bucket     TIMESTAMPTZ NOT NULL,
    gen        BIGINT NOT NULL DEFAULT 1,
    PRIMARY KEY (project_id, bucket)
);
CREATE TABLE session_stats_dirty (
    project_id BIGINT NOT NULL,
    bucket     TIMESTAMPTZ NOT NULL,
    gen        BIGINT NOT NULL DEFAULT 1,
    PRIMARY KEY (project_id, bucket)
);
CREATE TABLE event_stats_hourly_rolled (
    bucket     TIMESTAMPTZ NOT NULL,
    project_id BIGINT NOT NULL,
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
    project_id  BIGINT NOT NULL,
    fingerprint UUID NOT NULL,
    events      BIGINT NOT NULL,
    PRIMARY KEY (project_id, fingerprint, bucket)
);
CREATE TABLE release_health_hourly_rolled (
    bucket     TIMESTAMPTZ NOT NULL,
    project_id BIGINT NOT NULL,
    release    TEXT NOT NULL,
    total      BIGINT NOT NULL,
    crashed    BIGINT NOT NULL,
    errored    BIGINT NOT NULL,
    PRIMARY KEY (project_id, bucket, release)
);

CREATE TABLE event_stats_hourly (
    bucket     TIMESTAMPTZ NOT NULL,
    project_id BIGINT NOT NULL,
    release    TEXT NOT NULL,
    platform   TEXT NOT NULL,
    level      event_level NOT NULL,
    events     BIGINT NOT NULL,
    unhandled    BIGINT NOT NULL,
    errors     BIGINT NOT NULL
);

CREATE FUNCTION crashcart_event_stats(pid BIGINT, from_at TIMESTAMPTZ, to_at TIMESTAMPTZ)
RETURNS SETOF event_stats_hourly
LANGUAGE SQL STABLE AS $$ SELECT NULL $$;

CREATE TABLE issue_stats_hourly (
    bucket      TIMESTAMPTZ NOT NULL,
    project_id  BIGINT NOT NULL,
    fingerprint UUID NOT NULL,
    events      BIGINT NOT NULL
);

CREATE TABLE release_health_hourly (
    bucket     TIMESTAMPTZ NOT NULL,
    project_id BIGINT NOT NULL,
    release    TEXT NOT NULL,
    total      BIGINT NOT NULL,
    crashed    BIGINT NOT NULL,
    errored    BIGINT NOT NULL
);

CREATE TABLE crashcart_schema (
    version    INTEGER NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
