package store

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5"
)

// Project is a project row.
type Project struct {
	ID              int64     `json:"id"`
	Slug            string    `json:"slug"`
	Name            string    `json:"name"`
	Platform        *string   `json:"platform"`
	PublicKey       string    `json:"public_key"`
	SampleKeepFirst int32     `json:"sample_keep_first"`
	SampleRate      float64   `json:"sample_rate"`
	DailyQuota      int32     `json:"daily_quota"`
	CreatedAt       time.Time `json:"created_at"`
}

const projectColumns = "id, slug, name, platform, public_key, sample_keep_first, sample_rate, daily_quota, created_at"

// ProjectKey is a retired-but-still-valid DSN key (RotateProjectKey pushes
// the outgoing key here instead of discarding it).
type ProjectKey struct {
	ID         int64      `json:"id"`
	ProjectID  int64      `json:"project_id"`
	PublicKey  string     `json:"public_key"`
	RetiredAt  time.Time  `json:"retired_at"`
	LastUsedAt *time.Time `json:"last_used_at"`
}

func ListProjects(ctx context.Context, db DB) ([]Project, error) {
	rows, err := db.Query(ctx, "SELECT "+projectColumns+" FROM projects ORDER BY name")
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowToStructByName[Project])
}

func GetProject(ctx context.Context, db DB, slug string) (Project, error) {
	return scanOne[Project](db.Query(ctx, "SELECT "+projectColumns+" FROM projects WHERE slug = $1", slug))
}

func GetProjectByID(ctx context.Context, db DB, id int64) (Project, error) {
	return scanOne[Project](db.Query(ctx, "SELECT "+projectColumns+" FROM projects WHERE id = $1", id))
}

func GetProjectByKey(ctx context.Context, db DB, publicKey string) (Project, error) {
	return scanOne[Project](db.Query(ctx, "SELECT "+projectColumns+" FROM projects WHERE public_key = $1", publicKey))
}

func CreateProject(ctx context.Context, db DB, slug, name string, platform *string, publicKey string) (Project, error) {
	return scanOne[Project](db.Query(ctx,
		"INSERT INTO projects (slug, name, platform, public_key) VALUES ($1, $2, $3, $4) RETURNING "+projectColumns,
		slug, name, platform, publicKey))
}

// ProjectUpdate is UpdateProject's field set — every settings field at
// once, seeded from the current row by the caller and selectively
// overridden (internal/web/settings.go, internal/api/projects.go).
type ProjectUpdate struct {
	ID              int64
	Name            string
	Platform        *string
	SampleKeepFirst int32
	SampleRate      float64
	DailyQuota      int32
}

func UpdateProject(ctx context.Context, db DB, u ProjectUpdate) (Project, error) {
	return scanOne[Project](db.Query(ctx,
		`UPDATE projects SET name = @Name, platform = @Platform, sample_keep_first = @SampleKeepFirst, sample_rate = @SampleRate, daily_quota = @DailyQuota
		WHERE id = @ID RETURNING `+projectColumns,
		pgx.StrictStructArgs(u)))
}

func DeleteProject(ctx context.Context, db DB, id int64) error {
	_, err := db.Exec(ctx, "DELETE FROM projects WHERE id = $1", id)
	return err
}

// RetireProjectKey pushes a project's current key into project_keys
// before Rotate overwrites it — it keeps authenticating
// (GetProjectByRetiredKey) until someone deletes the row explicitly.
func RetireProjectKey(ctx context.Context, db DB, projectID int64) error {
	_, err := db.Exec(ctx, "INSERT INTO project_keys (project_id, public_key) SELECT p.id, p.public_key FROM projects p WHERE p.id = $1", projectID)
	return err
}

// ListProjectKeys: a project's retired-but-still-valid keys, newest
// retirement first.
func ListProjectKeys(ctx context.Context, db DB, projectID int64) ([]ProjectKey, error) {
	rows, err := db.Query(ctx, "SELECT id, project_id, public_key, retired_at, last_used_at FROM project_keys WHERE project_id = $1 ORDER BY retired_at DESC", projectID)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowToStructByName[ProjectKey])
}

func DeleteProjectKey(ctx context.Context, db DB, projectID, id int64) (int64, error) {
	tag, err := db.Exec(ctx, "DELETE FROM project_keys WHERE project_id = $1 AND id = $2", projectID, id)
	if err != nil {
		return 0, err
	}
	return tag.RowsAffected(), nil
}

// GetProjectByRetiredKeyRow: the ingest fallback when a key isn't the
// current one: still valid until its project_keys row is deleted. KeyID
// is what TouchProjectKey needs. Project is embedded (not a named field)
// so RowToStructByName flattens and matches its columns directly —
// callers still read retired.Project (promoted field access is unchanged
// by embedding) or the individual retired.ID etc.
type GetProjectByRetiredKeyRow struct {
	KeyID int64
	Project
}

func GetProjectByRetiredKey(ctx context.Context, db DB, publicKey string) (GetProjectByRetiredKeyRow, error) {
	return scanOne[GetProjectByRetiredKeyRow](db.Query(ctx,
		"SELECT k.id AS key_id, p.id, p.slug, p.name, p.platform, p.public_key, p.sample_keep_first, p.sample_rate, p.daily_quota, p.created_at "+
			"FROM project_keys k JOIN projects p ON p.id = k.project_id WHERE k.public_key = $1", publicKey))
}

// TouchProjectKey records use of a retired key at most once a minute (one
// write per key per minute, not per request) — the fact that answers "is
// it safe to delete this now".
func TouchProjectKey(ctx context.Context, db DB, id int64) error {
	_, err := db.Exec(ctx, "UPDATE project_keys SET last_used_at = now() WHERE id = $1 AND (last_used_at IS NULL OR last_used_at < now() - INTERVAL '1 minute')", id)
	return err
}

// RotateProjectKey issues newKey as a project's new current DSN key,
// retiring the outgoing one into project_keys instead of discarding it —
// it keeps authenticating (GetProjectByRetiredKey) until someone deletes
// the row. The caller generates newKey (auth.NewProjectKey); store cannot
// import internal/auth (it imports store).
func (s *Store) RotateProjectKey(ctx context.Context, projectID int64, newKey string) (Project, error) {
	var p Project
	err := s.Tx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		if err := RetireProjectKey(ctx, tx, projectID); err != nil {
			return err
		}
		var err error
		p, err = scanOne[Project](tx.Query(ctx, "UPDATE projects SET public_key = $2 WHERE id = $1 RETURNING "+projectColumns, projectID, newKey))
		return err
	})
	return p, err
}
