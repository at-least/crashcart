-- CrashCart — Postgres schema.
--
-- Designed for Postgres from the start (no SQLite/D1 heritage):
--   * real TIMESTAMPTZ columns (no "timestamp encoded in the primary key")
--   * JSONB for tags / breadcrumbs / payload, with a GIN index for tag filters
--   * payloads of any size live inline (TOAST), no object-store side channel
--   * issues are keyed by a SHA-256 fingerprint; events carry it so issue
--     drill-downs are exact joins instead of error_type approximations

-- ── events: one row per Sentry event ────────────────────────
CREATE TABLE events (
    id             BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    occurred_at    TIMESTAMPTZ NOT NULL,            -- event.timestamp (UTC)
    received_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    event_id       TEXT,                            -- Sentry event_id when present
    level          TEXT NOT NULL,                   -- fatal | error | warning | info | debug
    message        TEXT NOT NULL,
    platform       TEXT,
    environment    TEXT,
    release        TEXT,                            -- contexts.app.app_version or event.release
    device_id      TEXT,                            -- tags.device_id
    device_model   TEXT,
    os_version     TEXT,
    screen         TEXT,                            -- event.transaction
    error_type     TEXT,                            -- exception.values[0].type
    error_location TEXT,                            -- "CartFragment.java:142" (deepest in-app frame)
    handled        BOOLEAN,                         -- false = crash, true = caught, null = no exception
    sdk_name       TEXT,
    user_id        TEXT,
    fingerprint    TEXT,                            -- issues.fingerprint (null when no exception)
    tags           JSONB NOT NULL DEFAULT '{}'::jsonb,   -- {"key": "value"}
    breadcrumbs    JSONB NOT NULL DEFAULT '[]'::jsonb,   -- last 20 crumbs, normalized
    payload        JSONB NOT NULL                   -- the untouched Sentry event
);

CREATE INDEX events_occurred_at_idx  ON events (occurred_at DESC, id DESC);
CREATE INDEX events_device_id_idx    ON events (device_id, occurred_at DESC) WHERE device_id IS NOT NULL;
CREATE INDEX events_user_id_idx      ON events (user_id, occurred_at DESC) WHERE user_id IS NOT NULL;
CREATE INDEX events_error_type_idx   ON events (error_type, occurred_at DESC) WHERE error_type IS NOT NULL;
CREATE INDEX events_release_idx      ON events (release, occurred_at DESC) WHERE release IS NOT NULL;
CREATE INDEX events_fingerprint_idx  ON events (fingerprint, occurred_at DESC) WHERE fingerprint IS NOT NULL;
CREATE INDEX events_tags_idx         ON events USING GIN (tags jsonb_path_ops);

-- ── user_devices: user → device mapping ─────────────────────
CREATE TABLE user_devices (
    user_id    TEXT NOT NULL,
    device_id  TEXT NOT NULL,
    last_seen  TIMESTAMPTZ NOT NULL,                -- refreshed lazily (≤ once/day per pair)
    PRIMARY KEY (user_id, device_id)
);
CREATE INDEX user_devices_last_seen_idx ON user_devices (last_seen);

-- ── hourly_stats: write-time aggregate for error + fatal events ──
CREATE TABLE hourly_stats (
    hour         TIMESTAMPTZ NOT NULL,              -- date_trunc('hour', occurred_at)
    level        TEXT NOT NULL,                     -- 'error' | 'fatal'
    crash_count  BIGINT NOT NULL DEFAULT 0,         -- fatal OR unhandled
    fatal_count  BIGINT NOT NULL DEFAULT 0,
    error_count  BIGINT NOT NULL DEFAULT 0,         -- handled errors
    PRIMARY KEY (hour, level)
);

-- ── issues: fingerprint grouping + lifecycle ─────────────────
CREATE TABLE issues (
    fingerprint    TEXT PRIMARY KEY,
    title          TEXT NOT NULL,
    level          TEXT NOT NULL,
    error_type     TEXT,
    screen         TEXT,
    platform       TEXT,
    status         TEXT NOT NULL DEFAULT 'unresolved'
                   CHECK (status IN ('unresolved', 'triaged', 'resolved', 'ignored', 'regression')),
    event_count    BIGINT NOT NULL DEFAULT 0,
    first_seen     TIMESTAMPTZ NOT NULL,
    last_seen      TIMESTAMPTZ NOT NULL,
    first_release  TEXT,
    last_release   TEXT,
    created_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at     TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX issues_last_seen_idx  ON issues (last_seen DESC);
CREATE INDEX issues_first_seen_idx ON issues (first_seen DESC);
CREATE INDEX issues_error_type_idx ON issues (error_type);

-- ── releases: per-version counters ──────────────────────────
CREATE TABLE releases (
    version       TEXT PRIMARY KEY,
    platform      TEXT,
    first_seen    TIMESTAMPTZ NOT NULL,
    last_seen     TIMESTAMPTZ NOT NULL,
    crash_count   BIGINT NOT NULL DEFAULT 0,
    error_count   BIGINT NOT NULL DEFAULT 0,
    total_events  BIGINT NOT NULL DEFAULT 0
);
CREATE INDEX releases_last_seen_idx ON releases (last_seen DESC);

-- ── release_health: crash-free sessions per release per day ──
CREATE TABLE release_health (
    release           TEXT NOT NULL,
    day               DATE NOT NULL,
    total_sessions    BIGINT NOT NULL DEFAULT 0,
    crashed_sessions  BIGINT NOT NULL DEFAULT 0,
    errored_sessions  BIGINT NOT NULL DEFAULT 0,
    PRIMARY KEY (release, day)
);

-- ── alert_types: the three built-in detectors ────────────────
CREATE TABLE alert_types (
    type            TEXT PRIMARY KEY CHECK (type IN ('crash_spike', 'new_error', 'regression')),
    enabled         BOOLEAN NOT NULL DEFAULT true,
    last_triggered  TIMESTAMPTZ,
    cooldown_until  TIMESTAMPTZ
);
INSERT INTO alert_types (type, enabled) VALUES
    ('crash_spike', true),
    ('new_error', true),
    ('regression', false);

-- ── symbol_files: ProGuard mappings / source maps / dSYMs ────
CREATE TABLE symbol_files (
    id           BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    platform     TEXT NOT NULL,
    release      TEXT NOT NULL,
    filename     TEXT NOT NULL,
    size         BIGINT NOT NULL,
    data         BYTEA NOT NULL,
    uploaded_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (platform, release, filename)
);
