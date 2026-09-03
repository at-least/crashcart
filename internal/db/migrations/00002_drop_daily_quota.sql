-- The daily quota is gone: sampling already bounds storage, and the
-- per-process, in-memory RateLimit (internal/auth) is the one remaining
-- ingest guard against a runaway project — no exact, cross-replica,
-- Postgres-backed cap on top of it. See ARCHITECTURE.md.
DROP TABLE project_usage;
ALTER TABLE projects DROP COLUMN daily_quota;
