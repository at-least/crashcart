-- Optional secondary indexes on events. NOT applied by default: every read
-- is a bounded PK range scan, and each index costs one more entry per
-- event write. Apply only the ones you need if a filter over a long window
-- gets slow at your volume:
--
--   psql "$DATABASE_URL" -f sql/optional_indexes.sql        (all of them)
--   psql "$DATABASE_URL" -c "CREATE INDEX CONCURRENTLY ..."  (one line)
--
-- CONCURRENTLY keeps ingest running while the index builds.

CREATE INDEX CONCURRENTLY IF NOT EXISTS events_device_id_idx   ON events (device_id, id DESC)   WHERE device_id IS NOT NULL;
CREATE INDEX CONCURRENTLY IF NOT EXISTS events_user_id_idx     ON events (user_id, id DESC)     WHERE user_id IS NOT NULL;
CREATE INDEX CONCURRENTLY IF NOT EXISTS events_release_idx     ON events (release, id DESC)     WHERE release IS NOT NULL;
CREATE INDEX CONCURRENTLY IF NOT EXISTS events_error_type_idx  ON events (error_type, id DESC)  WHERE error_type IS NOT NULL;
CREATE INDEX CONCURRENTLY IF NOT EXISTS events_fingerprint_idx ON events (fingerprint, id DESC) WHERE fingerprint IS NOT NULL;
CREATE INDEX CONCURRENTLY IF NOT EXISTS events_tags_idx        ON events USING GIN (tags jsonb_path_ops);
