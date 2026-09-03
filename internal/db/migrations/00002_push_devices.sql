-- ── push notifications (mobile companion apps) ──────────────────────────
-- A device belongs to the API key that registered it (not a project — one
-- phone follows several projects), so losing the key removes the device
-- and, via the second cascade, its subscriptions: no separate "revoke my
-- phone" step beyond deleting the key. token is UNIQUE so a reinstall or
-- token refresh upserts the existing row instead of double-sending.
-- platform is plain TEXT, not an enum, matching projects.platform — the
-- API layer validates it (ios | android), see internal/api/devices.go.
CREATE TABLE push_devices (
    id         BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    api_key_id BIGINT NOT NULL REFERENCES api_keys ON DELETE CASCADE,
    token      TEXT NOT NULL UNIQUE,
    platform   TEXT NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX push_devices_api_key ON push_devices (api_key_id);

-- v1 subscribes a device to a project's alerts as a whole (every enabled
-- alert type), the same granularity as alert_rules — no per-type filter.
CREATE TABLE push_subscriptions (
    device_id  BIGINT NOT NULL REFERENCES push_devices ON DELETE CASCADE,
    project_id BIGINT NOT NULL REFERENCES projects ON DELETE CASCADE,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (device_id, project_id)
);
CREATE INDEX push_subscriptions_project ON push_subscriptions (project_id);
