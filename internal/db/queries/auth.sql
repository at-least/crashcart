-- Users, viewer sessions and API keys (internal/auth).

-- name: CountUsers :one
SELECT count(*) FROM users;

-- name: CreateUser :one
INSERT INTO users (email, name, password_hash) VALUES ($1, $2, $3) RETURNING *;

-- name: GetUserByEmail :one
SELECT * FROM users WHERE email = $1;

-- name: GetUserByID :one
SELECT * FROM users WHERE id = $1;

-- name: ListUsers :many
SELECT * FROM users ORDER BY email;

-- name: SetUserPassword :execrows
UPDATE users SET password_hash = $2 WHERE id = $1;

-- name: DeleteUser :execrows
DELETE FROM users WHERE id = $1;

-- name: CreateUserSession :exec
INSERT INTO user_sessions (token_hash, user_id, expires_at) VALUES ($1, $2, $3);

-- name: GetUserSession :one
-- The user behind a live session token.
SELECT u.* FROM user_sessions s JOIN users u ON u.id = s.user_id
WHERE s.token_hash = $1 AND s.expires_at > now();

-- name: DeleteUserSession :exec
DELETE FROM user_sessions WHERE token_hash = $1;

-- name: ExpireUserSessions :execrows
DELETE FROM user_sessions WHERE expires_at < now();

-- name: CreateAPIKey :one
INSERT INTO api_keys (name, key_hash, prefix, created_by) VALUES ($1, $2, $3, $4)
RETURNING id, name, prefix, created_by, created_at, last_used_at, revoked_at;

-- name: ListAPIKeys :many
SELECT id, name, prefix, created_by, created_at, last_used_at, revoked_at FROM api_keys ORDER BY id;

-- name: GetAPIKeyByHash :one
SELECT id, name, prefix, created_by, created_at, last_used_at, revoked_at FROM api_keys
WHERE key_hash = $1 AND revoked_at IS NULL;

-- name: TouchAPIKey :exec
-- Records use at most once a minute (one write per key per minute, not per request).
UPDATE api_keys SET last_used_at = now()
WHERE id = $1 AND (last_used_at IS NULL OR last_used_at < now() - INTERVAL '1 minute');

-- name: RevokeAPIKey :execrows
UPDATE api_keys SET revoked_at = now() WHERE id = $1 AND revoked_at IS NULL;
