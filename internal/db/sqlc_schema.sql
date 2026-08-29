CREATE FUNCTION crashcart_is_crash(level TEXT, handled BOOLEAN) RETURNS BOOLEAN
    LANGUAGE SQL IMMUTABLE AS $$ SELECT level = 'fatal' OR handled = false $$;

-- sqlc-only mirror of internal/db/migrations (no TimescaleDB DDL).
-- Continuous aggregates appear here as plain tables so queries type-check.
-- Keep in sync with the migrations.

CREATE TABLE projects (
    id                BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    slug              TEXT NOT NULL UNIQUE,
    name              TEXT NOT NULL,
    platform          TEXT,
    public_key        TEXT NOT NULL UNIQUE,
    sample_keep_first INTEGER NOT NULL DEFAULT 100,
    sample_rate       DOUBLE PRECISION NOT NULL DEFAULT 1.0,
    daily_quota       INTEGER NOT NULL DEFAULT 100000,
    created_at        TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE events (
    id             BIGINT PRIMARY KEY,
    project_id     BIGINT NOT NULL,
    event_id       TEXT NOT NULL,
    level          TEXT NOT NULL,
    message        TEXT NOT NULL,
    platform       TEXT,
    environment    TEXT,
    release        TEXT,
    device_id      TEXT,
    device_model   TEXT,
    os_version     TEXT,
    screen         TEXT,
    error_type     TEXT,
    error_location TEXT,
    handled        BOOLEAN,
    sdk_name       TEXT,
    user_id        TEXT,
    fingerprint    TEXT,
    symbolicated   BOOLEAN NOT NULL DEFAULT false,
    tags           JSONB NOT NULL DEFAULT '{}'::jsonb,
    breadcrumbs    JSONB NOT NULL DEFAULT '[]'::jsonb,
    payload        JSONB NOT NULL,
    symbols        JSONB
);

CREATE TABLE sessions (
    id          BIGINT PRIMARY KEY,
    project_id  BIGINT NOT NULL,
    release     TEXT NOT NULL,
    environment TEXT,
    status      TEXT NOT NULL,
    count       INTEGER NOT NULL DEFAULT 1
);

CREATE TABLE issues (
    project_id       BIGINT NOT NULL,
    fingerprint      TEXT NOT NULL,
    title            TEXT NOT NULL,
    level            TEXT NOT NULL,
    error_type       TEXT,
    screen           TEXT,
    platform         TEXT,
    status           TEXT NOT NULL DEFAULT 'unresolved',
    event_count      BIGINT NOT NULL DEFAULT 0,
    stored_count     BIGINT NOT NULL DEFAULT 0,
    first_seen       BIGINT NOT NULL,
    last_seen        BIGINT NOT NULL,
    first_release    TEXT,
    last_release     TEXT,
    resolved_release TEXT,
    created_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (project_id, fingerprint)
);

CREATE TABLE symbol_files (
    id          BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    project_id  BIGINT NOT NULL,
    kind        TEXT NOT NULL,
    release     TEXT NOT NULL,
    debug_id    TEXT,
    filename    TEXT NOT NULL,
    size        BIGINT NOT NULL,
    data        BYTEA NOT NULL,
    uploaded_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (project_id, kind, release, filename)
);

CREATE TABLE upload_chunks (
    sha1       TEXT PRIMARY KEY,
    data       BYTEA NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE jobs (
    id         BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    kind       TEXT NOT NULL,
    project_id BIGINT NOT NULL,
    args       JSONB NOT NULL DEFAULT '{}'::jsonb,
    run_after  TIMESTAMPTZ NOT NULL DEFAULT now(),
    attempts   INTEGER NOT NULL DEFAULT 0,
    last_error TEXT,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE alert_rules (
    project_id       BIGINT NOT NULL,
    type             TEXT NOT NULL,
    enabled          BOOLEAN NOT NULL DEFAULT true,
    cooldown_minutes INTEGER NOT NULL DEFAULT 60,
    last_triggered   TIMESTAMPTZ,
    PRIMARY KEY (project_id, type)
);

CREATE TABLE alert_channels (
    id         BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    project_id BIGINT NOT NULL,
    kind       TEXT NOT NULL,
    config     JSONB NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE rate_limits (
    rl_key       TEXT NOT NULL,
    window_start BIGINT NOT NULL,
    count        INTEGER NOT NULL DEFAULT 1,
    PRIMARY KEY (rl_key, window_start)
);

-- continuous aggregates (as tables for sqlc)
CREATE TABLE event_stats_hourly (
    bucket     BIGINT NOT NULL,
    project_id BIGINT NOT NULL,
    release    TEXT NOT NULL,
    platform   TEXT NOT NULL,
    level      TEXT NOT NULL,
    events     BIGINT NOT NULL,
    crashes    BIGINT NOT NULL,
    errors     BIGINT NOT NULL
);

CREATE TABLE issue_stats_hourly (
    bucket      BIGINT NOT NULL,
    project_id  BIGINT NOT NULL,
    fingerprint TEXT NOT NULL,
    events      BIGINT NOT NULL
);

CREATE TABLE release_health_daily (
    bucket     BIGINT NOT NULL,
    project_id BIGINT NOT NULL,
    release    TEXT NOT NULL,
    total      BIGINT NOT NULL,
    crashed    BIGINT NOT NULL,
    errored    BIGINT NOT NULL
);
