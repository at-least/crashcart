// Enqueues dedupe against the live job with the same (kind, project,
// args) — jobs_pending: pending, leased or backing off — so a resend or a
// repeated upload queues once. A job waiting out a backoff is pulled
// forward to the new run_after: the enqueue wanted it now.
package store

import (
	"context"
	"encoding/json"
	"time"

	"github.com/jackc/pgx/v5"
)

type Job struct {
	ID          int64           `json:"id"`
	Kind        JobKind         `json:"kind"`
	ProjectID   int64           `json:"project_id"`
	Args        json.RawMessage `json:"args"`
	RunAfter    time.Time       `json:"run_after"`
	Attempts    int32           `json:"attempts"`
	LockedUntil *time.Time      `json:"locked_until"`
	LastError   *string         `json:"last_error"`
	CreatedAt   time.Time       `json:"created_at"`
}

const jobColumns = "id, kind, project_id, args, run_after, attempts, locked_until, last_error, created_at"

func scanJob(row pgx.Row) (Job, error) {
	var j Job
	err := row.Scan(&j.ID, &j.Kind, &j.ProjectID, &j.Args, &j.RunAfter, &j.Attempts, &j.LockedUntil, &j.LastError, &j.CreatedAt)
	return j, err
}

// EnqueueJobParams is one job to enqueue — an accumulator value for
// callers (like ingest) that batch several jobs into one EnqueueJobs call.
type EnqueueJobParams struct {
	Kind      JobKind
	ProjectID int64
	Args      json.RawMessage
	RunAfter  time.Time
}

func EnqueueJob(ctx context.Context, db DB, kind JobKind, projectID int64, args json.RawMessage, runAfter time.Time) error {
	_, err := db.Exec(ctx, `INSERT INTO jobs (kind, project_id, args, run_after) VALUES ($1, $2, $3, $4)
		ON CONFLICT (kind, project_id, args) WHERE attempts < 8 DO UPDATE SET run_after = LEAST(jobs.run_after, EXCLUDED.run_after)`,
		kind, projectID, args, runAfter)
	return err
}

// EnqueueJobs is a multi-row insert: one statement, one NOTIFY.
func EnqueueJobs(ctx context.Context, db DB, kinds []string, projectIDs []int64, args []json.RawMessage, runAfters []time.Time) error {
	_, err := db.Exec(ctx, `INSERT INTO jobs (kind, project_id, args, run_after)
		SELECT unnest($1::text[])::job_kind, unnest($2::bigint[]), unnest($3::jsonb[]), unnest($4::timestamptz[])
		ON CONFLICT (kind, project_id, args) WHERE attempts < 8 DO UPDATE SET run_after = LEAST(jobs.run_after, EXCLUDED.run_after)`,
		kinds, projectIDs, args, runAfters)
	return err
}

// ClaimJobs leases a batch (locked until lockedUntil) and counts the
// attempt. The caller runs the handlers outside any transaction, then
// deletes or reschedules each job; a lease that expires (worker died)
// makes the job claimable again.
func ClaimJobs(ctx context.Context, db DB, lockedUntil time.Time, max int32) ([]Job, error) {
	rows, err := db.Query(ctx, `UPDATE jobs SET locked_until = $1::timestamptz, attempts = attempts + 1
		WHERE id IN (
		    SELECT id FROM jobs
		    WHERE run_after <= now() AND (locked_until IS NULL OR locked_until < now()) AND attempts < 8
		    ORDER BY run_after, id LIMIT $2::int FOR UPDATE SKIP LOCKED)
		RETURNING `+jobColumns, lockedUntil, max)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	items := []Job{}
	for rows.Next() {
		j, err := scanJob(rows)
		if err != nil {
			return nil, err
		}
		items = append(items, j)
	}
	return items, rows.Err()
}

func DeleteJob(ctx context.Context, db DB, id int64) error {
	_, err := db.Exec(ctx, "DELETE FROM jobs WHERE id = $1", id)
	return err
}

func RetryJob(ctx context.Context, db DB, id int64, lastError *string, runAfter time.Time) error {
	_, err := db.Exec(ctx, "UPDATE jobs SET last_error = $2, run_after = $3, locked_until = NULL WHERE id = $1", id, lastError, runAfter)
	return err
}

// ReleaseJob is shutdown mid-job: give the lease back without counting an
// attempt (only while the lease is still ours: an expired one may have
// been claimed). A job on its last attempt is outside the jobs_pending
// index, so a newer duplicate may have been enqueued meanwhile; then this
// row is dropped instead of released (the newer one runs) — un-counting
// the attempt would collide with it.
func ReleaseJob(ctx context.Context, db DB, id int64) error {
	_, err := db.Exec(ctx, `WITH released AS (
		    UPDATE jobs SET locked_until = NULL, attempts = GREATEST(0, attempts - 1)
		    WHERE jobs.id = $1 AND jobs.locked_until IS NOT NULL AND jobs.locked_until >= now()
		      AND NOT EXISTS (SELECT 1 FROM jobs j WHERE j.kind = jobs.kind AND j.project_id = jobs.project_id
		                      AND j.args = jobs.args AND j.id <> jobs.id AND j.attempts < 8)
		    RETURNING jobs.id
		)
		DELETE FROM jobs WHERE jobs.id = $1 AND jobs.locked_until IS NOT NULL AND jobs.locked_until >= now()
		  AND NOT EXISTS (SELECT 1 FROM released)`, id)
	return err
}

func CountJobs(ctx context.Context, db DB) (int64, error) {
	var n int64
	err := db.QueryRow(ctx, "SELECT count(*) FROM jobs").Scan(&n)
	return n, err
}

// ExpireJobs: a job that failed 8 times is dead: never claimed again,
// kept with its last_error for a week so the failure can be seen, then
// dropped. (Only dead jobs: a live one older than that is still due.)
func ExpireJobs(ctx context.Context, db DB) (int64, error) {
	tag, err := db.Exec(ctx, "DELETE FROM jobs WHERE attempts >= 8 AND created_at < now() - INTERVAL '7 days'")
	if err != nil {
		return 0, err
	}
	return tag.RowsAffected(), nil
}

func DeadJobs(ctx context.Context, db DB, projectID int64) ([]Job, error) {
	rows, err := db.Query(ctx, "SELECT "+jobColumns+" FROM jobs WHERE project_id = $1 AND attempts >= 8 ORDER BY id DESC LIMIT 100", projectID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	items := []Job{}
	for rows.Next() {
		j, err := scanJob(rows)
		if err != nil {
			return nil, err
		}
		items = append(items, j)
	}
	return items, rows.Err()
}

// EnqueueSymbolicateRelease fans a release out into one symbolicate job
// per unsymbolicated event (bounded, newest first, the whole retention
// window) in a single statement: one NOTIFY, and each event then retries
// on its own. 'at' is rendered exactly as ingest renders it (microseconds,
// Z), so the jobs_pending index folds a fan-out onto a pending
// ingest-queued job.
func EnqueueSymbolicateRelease(ctx context.Context, db DB, projectID int64, release *string, limit int32) (int64, error) {
	tag, err := db.Exec(ctx, `INSERT INTO jobs (kind, project_id, args)
		SELECT 'symbolicate', e.project_id, jsonb_build_object('event', replace(e.event_id::text, '-', ''), 'at', to_char(e.occurred_at AT TIME ZONE 'UTC', 'YYYY-MM-DD"T"HH24:MI:SS.US"Z"'))
		FROM events e
		WHERE e.project_id = $1 AND e.release = $2 AND e.symbolicated = false AND e.fingerprint IS NOT NULL
		ORDER BY e.occurred_at DESC LIMIT $3
		ON CONFLICT (kind, project_id, args) WHERE attempts < 8 DO UPDATE SET run_after = LEAST(jobs.run_after, EXCLUDED.run_after)`,
		projectID, release, limit)
	if err != nil {
		return 0, err
	}
	return tag.RowsAffected(), nil
}
