package store

import (
	"context"

	"github.com/jackc/pgx/v5"

	"github.com/at-least/crashcart/internal/db/sqlc"
)

// RotateProjectKey issues newKey as a project's new current DSN key,
// retiring the outgoing one into project_keys instead of discarding it —
// it keeps authenticating (GetProjectByRetiredKey) until someone deletes
// the row. The caller generates newKey (auth.NewProjectKey); store cannot
// import internal/auth (it imports store).
func (s *Store) RotateProjectKey(ctx context.Context, projectID int64, newKey string) (sqlc.Project, error) {
	var p sqlc.Project
	err := s.Tx(ctx, func(ctx context.Context, tx pgx.Tx, q *sqlc.Queries) error {
		if err := q.RetireProjectKey(ctx, projectID); err != nil {
			return err
		}
		var err error
		p, err = q.RotateProjectKey(ctx, sqlc.RotateProjectKeyParams{ID: projectID, PublicKey: newKey})
		return err
	})
	return p, err
}
