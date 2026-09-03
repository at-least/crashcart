package store

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/at-least/crashcart/internal/sentry"
)

// Attachment is the full row (with bytes) — GetAttachment's shape.
type Attachment struct {
	OccurredAt     time.Time `json:"occurred_at"`
	ProjectID      int64     `json:"project_id"`
	EventID        sentry.ID `json:"event_id"`
	N              int32     `json:"n"`
	Filename       string    `json:"filename"`
	ContentType    string    `json:"content_type"`
	AttachmentType string    `json:"attachment_type"`
	Size           int64     `json:"size"`
	Data           []byte    `json:"data"`
}

// AttachmentMeta is an event's attachments without their bytes (the event
// page, the API).
type AttachmentMeta struct {
	OccurredAt     time.Time `json:"occurred_at"`
	ProjectID      int64     `json:"project_id"`
	EventID        sentry.ID `json:"event_id"`
	N              int32     `json:"n"`
	Filename       string    `json:"filename"`
	ContentType    string    `json:"content_type"`
	AttachmentType string    `json:"attachment_type"`
	Size           int64     `json:"size"`
}

// ListAttachments: an event's attachments without their bytes. The time
// keeps the lookup to one partition.
func ListAttachments(ctx context.Context, db DB, projectID int64, eventID sentry.ID, occurredAt time.Time) ([]AttachmentMeta, error) {
	rows, err := db.Query(ctx, "SELECT occurred_at, project_id, event_id, n, filename, content_type, attachment_type, size "+
		"FROM attachments WHERE project_id = $1 AND event_id = $2 AND occurred_at = $3 ORDER BY n", projectID, eventID, occurredAt)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	items := []AttachmentMeta{}
	for rows.Next() {
		var a AttachmentMeta
		if err := rows.Scan(&a.OccurredAt, &a.ProjectID, &a.EventID, &a.N, &a.Filename, &a.ContentType, &a.AttachmentType, &a.Size); err != nil {
			return nil, err
		}
		items = append(items, a)
	}
	return items, rows.Err()
}

func GetAttachment(ctx context.Context, db DB, projectID int64, eventID sentry.ID, occurredAt time.Time, n int32) (Attachment, error) {
	var a Attachment
	err := db.QueryRow(ctx, "SELECT occurred_at, project_id, event_id, n, filename, content_type, attachment_type, size, data "+
		"FROM attachments WHERE project_id = $1 AND event_id = $2 AND occurred_at = $3 AND n = $4", projectID, eventID, occurredAt, n).
		Scan(&a.OccurredAt, &a.ProjectID, &a.EventID, &a.N, &a.Filename, &a.ContentType, &a.AttachmentType, &a.Size, &a.Data)
	return a, err
}

// AttachmentInsert is one row for InsertAttachments: an envelope
// attachment item kept with its stored event.
type AttachmentInsert struct {
	OccurredAt     time.Time // the event's
	ProjectID      int64
	EventID        sentry.ID
	N              int32 // position among the event's attachments
	Filename       string
	ContentType    string
	AttachmentType string
	Data           []byte
}

const insertAttachmentSQL = `INSERT INTO attachments (occurred_at, project_id, event_id, n, filename, content_type, attachment_type, size, data)
	VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9)
	ON CONFLICT (project_id, event_id, occurred_at, n) DO NOTHING`

// InsertAttachments writes a batch in one round trip. Duplicate keys (a
// resent envelope) are skipped. Attachments carry no statistics, so
// nothing is marked dirty.
func InsertAttachments(ctx context.Context, tx pgx.Tx, rows []AttachmentInsert) error {
	if len(rows) == 0 {
		return nil
	}
	b := &pgx.Batch{}
	for _, r := range rows {
		b.Queue(insertAttachmentSQL, r.OccurredAt, r.ProjectID, r.EventID, r.N, r.Filename, r.ContentType, r.AttachmentType, int64(len(r.Data)), r.Data)
	}
	res := tx.SendBatch(ctx, b)
	defer res.Close()
	for range rows {
		if _, err := res.Exec(); err != nil {
			return err
		}
	}
	return res.Close()
}
