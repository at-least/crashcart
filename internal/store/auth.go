// Users, viewer sessions and API keys (internal/auth).
package store

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5"
)

type User struct {
	ID           int64     `json:"id"`
	Email        string    `json:"email"`
	Name         string    `json:"name"`
	PasswordHash string    `json:"password_hash"`
	CreatedAt    time.Time `json:"created_at"`
}

const userColumns = "id, email, name, password_hash, created_at"

func CountUsers(ctx context.Context, db DB) (int64, error) {
	var n int64
	err := db.QueryRow(ctx, "SELECT count(*) FROM users").Scan(&n)
	return n, err
}

func CreateUser(ctx context.Context, db DB, email, name, passwordHash string) (User, error) {
	return scanOne[User](db.Query(ctx, "INSERT INTO users (email, name, password_hash) VALUES ($1, $2, $3) RETURNING "+userColumns,
		email, name, passwordHash))
}

func GetUserByEmail(ctx context.Context, db DB, email string) (User, error) {
	return scanOne[User](db.Query(ctx, "SELECT "+userColumns+" FROM users WHERE email = $1", email))
}

func ListUsers(ctx context.Context, db DB) ([]User, error) {
	rows, err := db.Query(ctx, "SELECT "+userColumns+" FROM users ORDER BY email")
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowToStructByName[User])
}

func SetUserPassword(ctx context.Context, db DB, id int64, passwordHash string) (int64, error) {
	tag, err := db.Exec(ctx, "UPDATE users SET password_hash = $2 WHERE id = $1", id, passwordHash)
	if err != nil {
		return 0, err
	}
	return tag.RowsAffected(), nil
}

func DeleteUser(ctx context.Context, db DB, id int64) (int64, error) {
	tag, err := db.Exec(ctx, "DELETE FROM users WHERE id = $1", id)
	if err != nil {
		return 0, err
	}
	return tag.RowsAffected(), nil
}

func CreateUserSession(ctx context.Context, db DB, tokenHash []byte, userID int64, expiresAt time.Time) error {
	_, err := db.Exec(ctx, "INSERT INTO user_sessions (token_hash, user_id, expires_at) VALUES ($1, $2, $3)", tokenHash, userID, expiresAt)
	return err
}

// GetUserSession is the user behind a live session token.
func GetUserSession(ctx context.Context, db DB, tokenHash []byte) (User, error) {
	return scanOne[User](db.Query(ctx, "SELECT u.id, u.email, u.name, u.password_hash, u.created_at FROM user_sessions s JOIN users u ON u.id = s.user_id "+
		"WHERE s.token_hash = $1 AND s.expires_at > now()", tokenHash))
}

func DeleteUserSession(ctx context.Context, db DB, tokenHash []byte) error {
	_, err := db.Exec(ctx, "DELETE FROM user_sessions WHERE token_hash = $1", tokenHash)
	return err
}

func ExpireUserSessions(ctx context.Context, db DB) (int64, error) {
	tag, err := db.Exec(ctx, "DELETE FROM user_sessions WHERE expires_at < now()")
	if err != nil {
		return 0, err
	}
	return tag.RowsAffected(), nil
}

// APIKey never carries the hash (the secret is shown once when created;
// these queries never select key_hash back out).
type APIKey struct {
	ID         int64      `json:"id"`
	Name       string     `json:"name"`
	Prefix     string     `json:"prefix"`
	CreatedBy  *int64     `json:"created_by"`
	CreatedAt  time.Time  `json:"created_at"`
	LastUsedAt *time.Time `json:"last_used_at"`
	RevokedAt  *time.Time `json:"revoked_at"`
}

const apiKeyColumns = "id, name, prefix, created_by, created_at, last_used_at, revoked_at"

func CreateAPIKey(ctx context.Context, db DB, name string, keyHash []byte, prefix string, createdBy *int64) (APIKey, error) {
	return scanOne[APIKey](db.Query(ctx, "INSERT INTO api_keys (name, key_hash, prefix, created_by) VALUES ($1, $2, $3, $4) RETURNING "+apiKeyColumns,
		name, keyHash, prefix, createdBy))
}

func ListAPIKeys(ctx context.Context, db DB) ([]APIKey, error) {
	rows, err := db.Query(ctx, "SELECT "+apiKeyColumns+" FROM api_keys ORDER BY id")
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowToStructByName[APIKey])
}

func GetAPIKeyByHash(ctx context.Context, db DB, keyHash []byte) (APIKey, error) {
	return scanOne[APIKey](db.Query(ctx, "SELECT "+apiKeyColumns+" FROM api_keys WHERE key_hash = $1 AND revoked_at IS NULL", keyHash))
}

// TouchAPIKey records use at most once a minute (one write per key per
// minute, not per request).
func TouchAPIKey(ctx context.Context, db DB, id int64) error {
	_, err := db.Exec(ctx, "UPDATE api_keys SET last_used_at = now() WHERE id = $1 AND (last_used_at IS NULL OR last_used_at < now() - INTERVAL '1 minute')", id)
	return err
}

func RevokeAPIKey(ctx context.Context, db DB, id int64) (int64, error) {
	tag, err := db.Exec(ctx, "UPDATE api_keys SET revoked_at = now() WHERE id = $1 AND revoked_at IS NULL", id)
	if err != nil {
		return 0, err
	}
	return tag.RowsAffected(), nil
}
