-- Enqueues dedupe against the live job with the same (kind, project,
-- args) — jobs_pending: pending, leased or backing off — so a resend or a
-- repeated upload queues once. A job waiting out a backoff is pulled
-- forward to the new run_after: the enqueue wanted it now.

-- name: EnqueueJob :exec
INSERT INTO jobs (kind, project_id, args, run_after) VALUES ($1, $2, $3, $4)
ON CONFLICT (kind, project_id, args) WHERE attempts < 8 DO UPDATE SET run_after = LEAST(jobs.run_after, EXCLUDED.run_after);

-- name: EnqueueJobs :exec
-- Multi-row insert: one statement, one NOTIFY.
INSERT INTO jobs (kind, project_id, args, run_after)
SELECT unnest(sqlc.arg(kinds)::text[])::job_kind, unnest(sqlc.arg(project_ids)::bigint[]), unnest(sqlc.arg(args)::jsonb[]), unnest(sqlc.arg(run_afters)::timestamptz[])
ON CONFLICT (kind, project_id, args) WHERE attempts < 8 DO UPDATE SET run_after = LEAST(jobs.run_after, EXCLUDED.run_after);

-- name: ClaimJobs :many
-- Leases a batch (locked until $2) and counts the attempt. The caller runs
-- the handlers outside any transaction, then deletes or reschedules each
-- job; a lease that expires (worker died) makes the job claimable again.
UPDATE jobs SET locked_until = sqlc.arg(locked_until)::timestamptz, attempts = attempts + 1
WHERE id IN (
    SELECT id FROM jobs
    WHERE run_after <= now() AND (locked_until IS NULL OR locked_until < now()) AND attempts < 8
    ORDER BY run_after, id LIMIT sqlc.arg(max)::int FOR UPDATE SKIP LOCKED)
RETURNING *;

-- name: DeleteJob :exec
DELETE FROM jobs WHERE id = $1;

-- name: RetryJob :exec
UPDATE jobs SET last_error = $2, run_after = $3, locked_until = NULL WHERE id = $1;

-- name: ReleaseJob :exec
-- Shutdown mid-job: give the lease back without counting an attempt (only
-- while the lease is still ours: an expired one may have been claimed).
UPDATE jobs SET locked_until = NULL, attempts = GREATEST(0, attempts - 1)
WHERE id = $1 AND locked_until IS NOT NULL AND locked_until >= now();

-- name: CountJobs :one
SELECT count(*) FROM jobs;

-- name: MetricsGauges :one
-- The database-backed gauges of GET /metrics, in one round trip.
SELECT (SELECT count(*) FROM jobs WHERE locked_until IS NULL AND attempts < 8)::bigint AS jobs_pending,
       (SELECT count(*) FROM jobs WHERE attempts >= 8)::bigint AS jobs_dead,
       (SELECT count(*) FROM event_stats_dirty)::bigint + (SELECT count(*) FROM session_stats_dirty)::bigint AS dirty_hours,
       (SELECT count(*) FROM issues)::bigint AS issues;

-- name: ExpireJobs :execrows
-- A job that failed 8 times is dead: never claimed again, kept with its
-- last_error for a week so the failure can be seen, then dropped. (Only
-- dead jobs: a live one older than that is still due.)
DELETE FROM jobs WHERE attempts >= 8 AND created_at < now() - INTERVAL '7 days';

-- name: DeadJobs :many
SELECT * FROM jobs WHERE project_id = $1 AND attempts >= 8 ORDER BY id DESC LIMIT 100;

-- name: EnqueueSymbolicateRelease :execrows
-- Fans a release out into one symbolicate job per unsymbolicated event
-- (bounded, newest first, the whole retention window) in a single
-- statement: one NOTIFY, and each event then retries on its own.
INSERT INTO jobs (kind, project_id, args)
SELECT 'symbolicate', e.project_id, jsonb_build_object('event', replace(e.event_id::text, '-', ''), 'at', e.occurred_at)
FROM events e
WHERE e.project_id = $1 AND e.release = $2 AND e.symbolicated = false AND e.fingerprint IS NOT NULL
ORDER BY e.occurred_at DESC LIMIT $3
ON CONFLICT (kind, project_id, args) WHERE attempts < 8 DO UPDATE SET run_after = LEAST(jobs.run_after, EXCLUDED.run_after);
