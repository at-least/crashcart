-- name: EnqueueJob :exec
INSERT INTO jobs (kind, project_id, args, run_after) VALUES ($1, $2, $3, $4);

-- name: ClaimJobs :many
-- Leases a batch (locked until $2) and counts the attempt. The caller runs
-- the handlers outside any transaction, then deletes or reschedules each
-- job; a lease that expires (worker died) makes the job claimable again.
UPDATE jobs SET locked_until = sqlc.arg(locked_until)::timestamptz, attempts = attempts + 1
WHERE id IN (
    SELECT id FROM jobs
    WHERE run_after <= now() AND (locked_until IS NULL OR locked_until < now())
    ORDER BY run_after, id LIMIT sqlc.arg(max)::int FOR UPDATE SKIP LOCKED)
RETURNING *;

-- name: DeleteJob :exec
DELETE FROM jobs WHERE id = $1;

-- name: RetryJob :exec
UPDATE jobs SET last_error = $2, run_after = $3, locked_until = NULL WHERE id = $1;

-- name: ReleaseJob :exec
-- Shutdown mid-job: give the lease back without counting an attempt.
UPDATE jobs SET locked_until = NULL, attempts = attempts - 1 WHERE id = $1;

-- name: CountJobs :one
SELECT count(*) FROM jobs;

-- name: ExpireJobs :execrows
DELETE FROM jobs WHERE attempts >= 8 OR created_at < now() - INTERVAL '7 days';

-- name: EnqueueSymbolicateRelease :execrows
-- Fans a release out into one symbolicate job per unsymbolicated event
-- (bounded, newest first) in a single statement: one NOTIFY, and each
-- event then retries on its own.
INSERT INTO jobs (kind, project_id, args)
SELECT 'symbolicate', e.project_id, jsonb_build_object('event', e.event_id)
FROM events e
WHERE e.project_id = $1 AND e.release = $2 AND e.symbolicated = false AND e.fingerprint IS NOT NULL
  AND e.occurred_at >= $3
ORDER BY e.occurred_at DESC LIMIT $4;
