-- Event payloads in the blob store (BLOB_STORE=s3). Nothing changes on
-- events itself: with a store configured, ingest writes payload = NULL and
-- the gzipped bytes into payload_spool in the same transaction (durable in
-- Postgres before any object exists); a background flusher packs the
-- spool per (project, week) into ~8 MB objects — events/<project>/<week>/<id>
-- — and records each event's place in event_packs. Reads go column →
-- spool → ranged GET (internal/store/packs.go). A week's packs are deleted
-- when its partition is dropped (internal/retention).
--
-- +goose Up
CREATE TABLE payload_spool (
    project_id  BIGINT NOT NULL REFERENCES projects ON DELETE CASCADE,
    event_id    UUID NOT NULL,
    occurred_at TIMESTAMPTZ NOT NULL,
    data        BYTEA NOT NULL,                      -- the raw event, gzipped (as events.payload is)
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (project_id, event_id, occurred_at)
);
ALTER TABLE payload_spool ALTER COLUMN data SET STORAGE EXTERNAL;
-- The flusher's read order, which is also export's stream order, so a
-- pack's events are contiguous in an export.
CREATE INDEX payload_spool_order ON payload_spool (project_id, occurred_at, event_id);

CREATE TABLE packs (
    id         BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    project_id BIGINT NOT NULL REFERENCES projects ON DELETE CASCADE,
    week       DATE NOT NULL,                        -- retention's week (Monday, UTC) of its events
    bytes      BIGINT NOT NULL DEFAULT 0,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX packs_week ON packs (week);

CREATE TABLE event_packs (
    project_id  BIGINT NOT NULL REFERENCES projects ON DELETE CASCADE,
    event_id    UUID NOT NULL,
    occurred_at TIMESTAMPTZ NOT NULL,
    pack_id     BIGINT NOT NULL REFERENCES packs ON DELETE CASCADE,
    pack_offset INTEGER NOT NULL,
    pack_len    INTEGER NOT NULL,
    PRIMARY KEY (project_id, event_id, occurred_at)
);
CREATE INDEX event_packs_pack ON event_packs (pack_id);

-- +goose Down
-- No down: this project authors forward-only migrations. Restore an
-- earlier schema from a `crashcart export` taken before upgrading.
