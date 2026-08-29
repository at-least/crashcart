-- Plain-Postgres variant (no TimescaleDB — Neon, Supabase, RDS, …): the
-- stats live in *_rolled tables filled by the hourly rollup
-- (internal/retention), and the names the queries use are views that add
-- the current hour computed live from events / sessions. Retention deletes
-- rows in batches; there is no compression.
--
-- Buckets are UTC-aligned TIMESTAMPTZ starts; complete hours only in the
-- tables. stats_rollup.watermark is where the rolled data ends; the views
-- compute everything at or after it live, so nothing is missing between an
-- hour rolling over and the next rollup run.

CREATE TABLE stats_rollup (
    id        BOOLEAN PRIMARY KEY DEFAULT true CHECK (id),  -- one row
    watermark TIMESTAMPTZ NOT NULL
);

-- Start of the live range: the rollup watermark, or everything on a fresh database.
CREATE FUNCTION crashcart_rolled_until() RETURNS TIMESTAMPTZ
    LANGUAGE SQL STABLE AS $$ SELECT COALESCE((SELECT watermark FROM stats_rollup), '-infinity'::timestamptz) $$;

CREATE TABLE event_stats_hourly_rolled (
    bucket     TIMESTAMPTZ NOT NULL,
    project_id BIGINT NOT NULL,
    release    TEXT   NOT NULL DEFAULT '',
    platform   TEXT   NOT NULL DEFAULT '',
    level      TEXT   NOT NULL,
    events     BIGINT NOT NULL,
    crashes    BIGINT NOT NULL,
    errors     BIGINT NOT NULL,
    PRIMARY KEY (project_id, bucket, release, platform, level)
);
CREATE TABLE issue_stats_hourly_rolled (
    bucket      TIMESTAMPTZ NOT NULL,
    project_id  BIGINT NOT NULL,
    fingerprint TEXT   NOT NULL,
    events      BIGINT NOT NULL,
    PRIMARY KEY (project_id, fingerprint, bucket)
);
CREATE TABLE release_health_daily_rolled (
    bucket     TIMESTAMPTZ NOT NULL,
    project_id BIGINT NOT NULL,
    release    TEXT   NOT NULL,
    total      BIGINT NOT NULL,
    crashed    BIGINT NOT NULL,
    errored    BIGINT NOT NULL,
    PRIMARY KEY (project_id, release, bucket)
);

CREATE VIEW event_stats_hourly AS
SELECT bucket, project_id, release, platform, level, events, crashes, errors FROM event_stats_hourly_rolled
UNION ALL
SELECT date_trunc('hour', occurred_at, 'UTC'), project_id, COALESCE(release, ''), COALESCE(platform, ''), level,
       count(*)::bigint,
       count(*) FILTER (WHERE crashcart_is_crash(level, handled))::bigint,
       count(*) FILTER (WHERE level = 'error' AND handled IS NOT false)::bigint
FROM events WHERE occurred_at >= crashcart_rolled_until()
GROUP BY 1, 2, 3, 4, 5;

CREATE VIEW issue_stats_hourly AS
SELECT bucket, project_id, fingerprint, events FROM issue_stats_hourly_rolled
UNION ALL
SELECT date_trunc('hour', occurred_at, 'UTC'), project_id, fingerprint, count(*)::bigint
FROM events WHERE occurred_at >= crashcart_rolled_until() AND fingerprint IS NOT NULL
GROUP BY 1, 2, 3;

CREATE VIEW release_health_daily AS
SELECT bucket, project_id, release, total, crashed, errored FROM release_health_daily_rolled
UNION ALL
SELECT date_trunc('day', started_at, 'UTC'), project_id, release,
       sum(count)::bigint,
       COALESCE(sum(count) FILTER (WHERE status = 'crashed'), 0)::bigint,
       COALESCE(sum(count) FILTER (WHERE status IN ('errored', 'abnormal')), 0)::bigint
FROM sessions WHERE started_at >= crashcart_rolled_until()
GROUP BY 1, 2, 3;
