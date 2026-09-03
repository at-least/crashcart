package store

import (
	"context"
	"time"
)

func PutUploadChunk(ctx context.Context, db DB, sha1 string, data []byte) error {
	_, err := db.Exec(ctx, "INSERT INTO upload_chunks (sha1, data) VALUES ($1, $2) ON CONFLICT (sha1) DO NOTHING", sha1, data)
	return err
}

func UploadChunksPresent(ctx context.Context, db DB, sha1s []string) ([]string, error) {
	rows, err := db.Query(ctx, "SELECT sha1 FROM upload_chunks WHERE sha1 = ANY($1::text[])", sha1s)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	items := []string{}
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			return nil, err
		}
		items = append(items, s)
	}
	return items, rows.Err()
}

type UploadChunk struct {
	Sha1 string
	Data []byte
}

// GetUploadChunks: the chunks of one file, in the order the assemble
// request lists them.
func GetUploadChunks(ctx context.Context, db DB, sha1s []string) ([]UploadChunk, error) {
	rows, err := db.Query(ctx, "SELECT c.sha1, c.data FROM unnest($1::text[]) WITH ORDINALITY AS w(sha1, n) "+
		"JOIN upload_chunks c ON c.sha1 = w.sha1 ORDER BY w.n", sha1s)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	items := []UploadChunk{}
	for rows.Next() {
		var c UploadChunk
		if err := rows.Scan(&c.Sha1, &c.Data); err != nil {
			return nil, err
		}
		items = append(items, c)
	}
	return items, rows.Err()
}

func DeleteUploadChunks(ctx context.Context, db DB, sha1s []string) error {
	_, err := db.Exec(ctx, "DELETE FROM upload_chunks WHERE sha1 = ANY($1::text[])", sha1s)
	return err
}

func ExpireUploadChunks(ctx context.Context, db DB, before time.Time) (int64, error) {
	tag, err := db.Exec(ctx, "DELETE FROM upload_chunks WHERE created_at < $1", before)
	if err != nil {
		return 0, err
	}
	return tag.RowsAffected(), nil
}
