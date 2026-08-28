-- CrashCart — Postgres schema.
--
-- Write-cost first: every table has exactly one index — its primary key.
--   * events.id encodes the event time (µs), so the PK doubles as the
--     chronological index; no secondary indexes anywhere
--   * JSONB for tags / breadcrumbs / payload; payloads of any size live
--     inline (TOAST), no object-store side channel
--   * issues are keyed by a SHA-256 fingerprint; events carry it so issue
--     drill-downs are exact (one PK range scan, then issues by key)

-- ── events: one row per Sentry event ────────────────────────
-- The primary key IS the timestamp: id = event unix-ms × 1000 + random(0..999)
-- (see internal/pk). A range on id is a range in time, newest events sort
-- last, and there is deliberately NO secondary index: every read path
-- (dashboard, filters, alerts, retention) is a bounded PK range scan, so a
-- write costs exactly one heap row + one btree entry.
CREATE TABLE events (
    id             BIGINT PRIMARY KEY,               -- occurred_at µs (ms×1000 + rnd)
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

-- ── user_devices: user → device mapping ─────────────────────
CREATE TABLE user_devices (
    user_id    TEXT NOT NULL,
    device_id  TEXT NOT NULL,
    last_seen  TIMESTAMPTZ NOT NULL,                -- refreshed lazily (≤ once/day per pair)
    PRIMARY KEY (user_id, device_id)
);

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
    platform     TEXT NOT NULL,
    release      TEXT NOT NULL,
    filename     TEXT NOT NULL,
    size         BIGINT NOT NULL,
    data         BYTEA NOT NULL,
    uploaded_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (platform, release, filename)
);
