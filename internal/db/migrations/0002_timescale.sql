-- TimescaleDB variant (applied when the extension is available and
-- TIMESCALE is not "off"): hypertables, compression, continuous aggregates
-- with real-time aggregation. Idempotent, so a database migrated before the
-- split can re-run it.
CREATE EXTENSION IF NOT EXISTS timescaledb;

SELECT create_hypertable('events', 'id', chunk_time_interval => 86400000000, if_not_exists => true);
SELECT set_integer_now_func('events', 'crashcart_now', replace_if_exists => true);
ALTER TABLE events SET (
    timescaledb.compress,
    timescaledb.compress_segmentby = 'project_id, fingerprint',
    timescaledb.compress_orderby = 'id DESC'
);

SELECT create_hypertable('sessions', 'id', chunk_time_interval => 86400000000, if_not_exists => true);
SELECT set_integer_now_func('sessions', 'crashcart_now', replace_if_exists => true);
ALTER TABLE sessions SET (
    timescaledb.compress,
    timescaledb.compress_segmentby = 'project_id, release',
    timescaledb.compress_orderby = 'id DESC'
);

-- ── continuous aggregates ──────────────────────────────────────────────
-- All buckets are in id units (µs). Real-time aggregation is on so the
-- newest bucket includes rows not yet materialized.

CREATE MATERIALIZED VIEW IF NOT EXISTS event_stats_hourly
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

CREATE MATERIALIZED VIEW IF NOT EXISTS issue_stats_hourly
WITH (timescaledb.continuous, timescaledb.materialized_only = false) AS
SELECT time_bucket(3600000000::bigint, id) AS bucket,
       project_id,
       fingerprint,
       count(*) AS events
FROM events
WHERE fingerprint IS NOT NULL
GROUP BY 1, 2, 3
WITH NO DATA;

CREATE MATERIALIZED VIEW IF NOT EXISTS release_health_daily
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
    schedule_interval => INTERVAL '5 minutes', if_not_exists => true);
SELECT add_continuous_aggregate_policy('issue_stats_hourly',
    start_offset => 10800000000::bigint, end_offset => 60000000::bigint,
    schedule_interval => INTERVAL '5 minutes', if_not_exists => true);
SELECT add_continuous_aggregate_policy('release_health_daily',
    start_offset => 259200000000::bigint, end_offset => 60000000::bigint,
    schedule_interval => INTERVAL '5 minutes', if_not_exists => true);

-- Retention and compression policies are reconciled at startup from
-- RETENTION_DAYS (see internal/retention), not fixed here.
