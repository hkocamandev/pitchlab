-- name: GetUserByUsername :one
-- Looked up by the lowered form so that login is case-insensitive, matching
-- the unique index. The stored casing is returned for display.
SELECT * FROM users WHERE lower(username) = lower($1);

-- name: GetUser :one
SELECT * FROM users WHERE id = $1;

-- name: CreateUser :one
INSERT INTO users (id, username, display_name, password_hash, role)
VALUES ($1, $2, $3, $4, $5)
RETURNING *;

-- name: UpsertUser :one
-- Used by the seed command so that re-running it rotates the password rather
-- than failing on a conflict.
INSERT INTO users (id, username, display_name, password_hash, role)
VALUES ($1, $2, $3, $4, $5)
ON CONFLICT (lower(username)) DO UPDATE SET
    display_name  = EXCLUDED.display_name,
    password_hash = EXCLUDED.password_hash,
    role          = EXCLUDED.role,
    is_active     = TRUE,
    updated_at    = now()
RETURNING *;

-- name: RecordLogin :exec
-- Separate from the lookup so a successful verification is recorded without
-- holding a transaction open across the password comparison, which is
-- deliberately slow.
UPDATE users SET last_login_at = now(), updated_at = now() WHERE id = $1;

-- name: ListUsers :many
SELECT * FROM users ORDER BY lower(username);
