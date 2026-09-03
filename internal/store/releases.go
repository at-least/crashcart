package store

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5"
)

type Release struct {
	ProjectID int64     `json:"project_id"`
	Release   string    `json:"release"`
	Platforms []string  `json:"platforms"`
	FirstSeen time.Time `json:"first_seen"`
}

// UpsertReleases records the releases an envelope mentions. The conflict
// update only fires when it would change something (a new platform, an
// earlier first_seen), so the usual envelope leaves the row untouched.
func UpsertReleases(ctx context.Context, db DB, projectID int64, releases, platforms []string, firstSeens []time.Time) error {
	_, err := db.Exec(ctx, `INSERT INTO releases (project_id, release, platforms, first_seen)
		SELECT $1::bigint, u.r, CASE WHEN u.p = '' THEN '{}'::text[] ELSE ARRAY[u.p] END, u.t
		FROM (SELECT unnest($2::text[]) AS r, unnest($3::text[]) AS p,
		             unnest($4::timestamptz[]) AS t) AS u
		ON CONFLICT (project_id, release) DO UPDATE SET
		    platforms  = (SELECT array_agg(DISTINCT x ORDER BY x) FROM unnest(releases.platforms || EXCLUDED.platforms) AS x),
		    first_seen = LEAST(releases.first_seen, EXCLUDED.first_seen)
		WHERE NOT releases.platforms @> EXCLUDED.platforms OR EXCLUDED.first_seen < releases.first_seen`,
		projectID, releases, platforms, firstSeens)
	return err
}

// ListReleases: newest first (by first appearance).
func ListReleases(ctx context.Context, db DB, projectID int64, limit int32) ([]Release, error) {
	rows, err := db.Query(ctx, "SELECT project_id, release, platforms, first_seen FROM releases WHERE project_id = $1 ORDER BY first_seen DESC, release DESC LIMIT $2",
		projectID, limit)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowToStructByName[Release])
}
