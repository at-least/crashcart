package store

// Event payloads in the blob store: the spool ingest writes, the packs
// the flusher builds from it, and where each packed event's bytes are
// (packs.go).

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/at-least/crashcart/internal/sentry"
)

// SpoolGroupsRow: what the flusher chooses from — each (project, week)
// with spooled payloads, its total size and its oldest row. The week is
// Monday 00:00 UTC like retention's partitions (date_trunc('week') is
// ISO, Monday-based).
type SpoolGroupsRow struct {
	ProjectID int64
	Week      time.Time
	Bytes     int64
	Oldest    time.Time
}

func SpoolGroups(ctx context.Context, db DB) ([]SpoolGroupsRow, error) {
	rows, err := db.Query(ctx, `SELECT project_id,
		       date_trunc('week', occurred_at AT TIME ZONE 'UTC')::date AS week,
		       sum(length(data))::bigint AS bytes,
		       min(created_at)::timestamptz AS oldest
		FROM payload_spool
		GROUP BY project_id, date_trunc('week', occurred_at AT TIME ZONE 'UTC')
		ORDER BY oldest`)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowToStructByName[SpoolGroupsRow])
}

type SpoolRowsRow struct {
	ProjectID  int64
	EventID    sentry.ID
	OccurredAt time.Time
	Data       []byte
}

// SpoolRows: one group's rows in export order (payload_spool_order),
// oldest week boundary [from, to): the flusher cuts at PackBytes in Go.
func SpoolRows(ctx context.Context, db DB, projectID int64, from, to time.Time, limit int32) ([]SpoolRowsRow, error) {
	rows, err := db.Query(ctx, `SELECT project_id, event_id, occurred_at, data
		FROM payload_spool
		WHERE project_id = $1 AND occurred_at >= $3 AND occurred_at < $4
		ORDER BY occurred_at, event_id
		LIMIT $2`, projectID, limit, from, to)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowToStructByName[SpoolRowsRow])
}

func InsertPack(ctx context.Context, db DB, projectID int64, week time.Time) (int64, error) {
	var id int64
	err := db.QueryRow(ctx, "INSERT INTO packs (project_id, week) VALUES ($1, $2) RETURNING id", projectID, week).Scan(&id)
	return id, err
}

func SetPackBytes(ctx context.Context, db DB, id, bytes int64) error {
	_, err := db.Exec(ctx, "UPDATE packs SET bytes = $2 WHERE id = $1", id, bytes)
	return err
}

func DeletePack(ctx context.Context, db DB, id int64) error {
	_, err := db.Exec(ctx, "DELETE FROM packs WHERE id = $1", id)
	return err
}

// InsertEventPackParams is one event's place in a pack.
type InsertEventPackParams struct {
	ProjectID  int64
	EventID    sentry.ID
	OccurredAt time.Time
	PackID     int64
	PackOffset int32
	PackLen    int32
}

// InsertEventPacks binds by name: PackOffset/PackLen are adjacent int32
// fields, and a positional swap between them would silently corrupt every
// payload's byte range in the pack instead of failing.
func InsertEventPacks(ctx context.Context, db DB, places []InsertEventPackParams) error {
	const q = `INSERT INTO event_packs (project_id, event_id, occurred_at, pack_id, pack_offset, pack_len)
		VALUES (@ProjectID, @EventID, @OccurredAt, @PackID, @PackOffset, @PackLen)
		ON CONFLICT (project_id, event_id, occurred_at) DO UPDATE SET pack_id = EXCLUDED.pack_id, pack_offset = EXCLUDED.pack_offset, pack_len = EXCLUDED.pack_len`
	b := &pgx.Batch{}
	for _, p := range places {
		b.Queue(q, pgx.StrictStructArgs(p))
	}
	res := db.SendBatch(ctx, b)
	for range b.Len() {
		if _, err := res.Exec(); err != nil {
			res.Close()
			return err
		}
	}
	return res.Close()
}

// SpoolKey identifies one payload_spool row.
type SpoolKey struct {
	ProjectID  int64
	EventID    sentry.ID
	OccurredAt time.Time
}

func DeleteSpoolRows(ctx context.Context, db DB, keys []SpoolKey) error {
	const q = "DELETE FROM payload_spool WHERE project_id = $1 AND event_id = $2 AND occurred_at = $3"
	b := &pgx.Batch{}
	for _, k := range keys {
		b.Queue(q, k.ProjectID, k.EventID, k.OccurredAt)
	}
	res := db.SendBatch(ctx, b)
	for range b.Len() {
		if _, err := res.Exec(); err != nil {
			res.Close()
			return err
		}
	}
	return res.Close()
}

// PayloadLocationRow: one statement, one snapshot — the spool row if the
// event is not packed yet, else its place in a pack; neither when it has
// no payload.
type PayloadLocationRow struct {
	Spooled    []byte
	PackID     *int64
	PackOffset *int32
	PackLen    *int32
	Week       *time.Time
}

func PayloadLocation(ctx context.Context, db DB, projectID int64, eventID sentry.ID, occurredAt time.Time) (PayloadLocationRow, error) {
	// PackOffset/PackLen are adjacent *int32 fields — scanned by name so a
	// column-list edit can't silently swap them (same risk InsertEventPacks
	// has on the write side).
	return scanOne[PayloadLocationRow](db.Query(ctx, `SELECT s.data AS spooled, p.pack_id, p.pack_offset, p.pack_len, k.week
		FROM (SELECT $1::bigint AS project_id, $2::uuid AS event_id, $3::timestamptz AS occurred_at) e
		LEFT JOIN payload_spool s ON s.project_id = e.project_id AND s.event_id = e.event_id AND s.occurred_at = e.occurred_at
		LEFT JOIN event_packs p ON p.project_id = e.project_id AND p.event_id = e.event_id AND p.occurred_at = e.occurred_at
		LEFT JOIN packs k ON k.id = p.pack_id`, projectID, eventID, occurredAt))
}

// ExpiredPacksRow: packs of weeks past retention — the same rule as the
// partition drop (a week is expired once its end is at or before the
// cutoff).
type ExpiredPacksRow struct {
	ID        int64
	ProjectID int64
	Week      time.Time
}

func ExpiredPacks(ctx context.Context, db DB, cutoff time.Time) ([]ExpiredPacksRow, error) {
	rows, err := db.Query(ctx, "SELECT id, project_id, week FROM packs WHERE week + 7 <= $1::date", cutoff)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowToStructByName[ExpiredPacksRow])
}

// ExpireSpool deletes spool rows of expired weeks (the partition rule,
// not a plain cutoff: a weekly partition keeps rows up to a week past the
// cutoff, and a row still in the spool — a bucket outage — is their only
// payload).
func ExpireSpool(ctx context.Context, db DB, cutoff time.Time) (int64, error) {
	tag, err := db.Exec(ctx, "DELETE FROM payload_spool WHERE date_trunc('week', occurred_at AT TIME ZONE 'UTC')::date + 7 <= $1::date", cutoff)
	if err != nil {
		return 0, err
	}
	return tag.RowsAffected(), nil
}

// ProjectPacksRow: a project's packs, read before the project (and, by
// cascade, the rows) is deleted, so the objects can be deleted after.
type ProjectPacksRow struct {
	ID   int64
	Week time.Time
}

func ProjectPacks(ctx context.Context, db DB, projectID int64) ([]ProjectPacksRow, error) {
	rows, err := db.Query(ctx, "SELECT id, week FROM packs WHERE project_id = $1", projectID)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowToStructByName[ProjectPacksRow])
}

func SpoolCount(ctx context.Context, db DB) (int64, error) {
	var n int64
	err := db.QueryRow(ctx, "SELECT count(*)::bigint FROM payload_spool").Scan(&n)
	return n, err
}
