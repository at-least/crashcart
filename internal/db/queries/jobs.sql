-- name: EnqueueJob :exec
INSERT INTO jobs (kind, project_id, args, run_after) VALUES ($1, $2, $3, $4);

-- name: ClaimJobs :many
-- Locks a batch for this transaction; the caller deletes or reschedules them.
SELECT * FROM jobs WHERE run_after <= now() ORDER BY run_after, id
LIMIT $1 FOR UPDATE SKIP LOCKED;

-- name: DeleteJob :exec
DELETE FROM jobs WHERE id = $1;

-- name: RetryJob :exec
UPDATE jobs SET attempts = attempts + 1, last_error = $2, run_after = $3 WHERE id = $1;

-- name: CountJobs :one
SELECT count(*) FROM jobs;

-- name: ExpireJobs :execrows
DELETE FROM jobs WHERE attempts >= 8 OR created_at < now() - INTERVAL '7 days';

-- name: UnsymbolicatedEvents :many
-- Events of a release that still lack symbols (bounded, newest first).
SELECT id FROM events
WHERE project_id = $1 AND release = $2 AND symbolicated = false AND fingerprint IS NOT NULL
  AND id >= $3 ORDER BY id DESC LIMIT $4;
