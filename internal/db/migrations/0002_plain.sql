-- Plain-Postgres variant (no TimescaleDB — Neon, Supabase, RDS, …): the
-- stats live in *_rolled tables filled by the hourly rollup
-- (internal/retention), and the names the queries use are views that add
-- the current hour computed live from events / sessions. Retention deletes
-- rows in batches; there is no compression.
--
-- Same bucket semantics as the serverless implementation's rollup: buckets
-- are id units (µs), complete hours only in the tables, the current hour
-- is never rolled.

-- Start of the current hour in id units: everything at or after it is live.
CREATE OR REPLACE FUNCTION crashcart_hour_start() RETURNS BIGINT
    LANGUAGE SQL STABLE AS $$ SELECT (extract(epoch FROM date_trunc('hour', now())) * 1000000)::bigint $$;

CREATE TABLE event_stats_hourly_rolled (
    bucket     BIGINT NOT NULL,
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
    bucket      BIGINT NOT NULL,
    project_id  BIGINT NOT NULL,
    fingerprint TEXT   NOT NULL,
    events      BIGINT NOT NULL,
    PRIMARY KEY (project_id, fingerprint, bucket)
);
CREATE TABLE release_health_daily_rolled (
    bucket     BIGINT NOT NULL,
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
SELECT (id / 3600000000) * 3600000000, project_id, COALESCE(release, ''), COALESCE(platform, ''), level,
       count(*)::bigint,
       count(*) FILTER (WHERE crashcart_is_crash(level, handled))::bigint,
       count(*) FILTER (WHERE level = 'error' AND handled IS NOT false)::bigint
FROM events WHERE id >= crashcart_hour_start()
GROUP BY 1, 2, 3, 4, 5;

CREATE VIEW issue_stats_hourly AS
SELECT bucket, project_id, fingerprint, events FROM issue_stats_hourly_rolled
UNION ALL
SELECT (id / 3600000000) * 3600000000, project_id, fingerprint, count(*)::bigint
FROM events WHERE id >= crashcart_hour_start() AND fingerprint IS NOT NULL
GROUP BY 1, 2, 3;

CREATE VIEW release_health_daily AS
SELECT bucket, project_id, release, total, crashed, errored FROM release_health_daily_rolled
UNION ALL
SELECT (id / 86400000000) * 86400000000, project_id, release,
       sum(count)::bigint,
       COALESCE(sum(count) FILTER (WHERE status = 'crashed'), 0)::bigint,
       COALESCE(sum(count) FILTER (WHERE status IN ('errored', 'abnormal')), 0)::bigint
FROM sessions WHERE id >= crashcart_hour_start()
GROUP BY 1, 2, 3;
