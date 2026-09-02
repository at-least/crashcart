package store

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/at-least/crashcart/internal/sentry"
)

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
