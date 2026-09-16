-- Users and authentication.
--
-- Added in phase 13. Phases 0-12 ran with no authentication at all, which was
-- a deliberate scope decision rather than an oversight -- but the boundary was
-- designed from the start (api-design.md §7.3) precisely so that adding it
-- later would not mean reshaping anything.

CREATE TABLE users (
    id            UUID PRIMARY KEY,

    -- Case-insensitive uniqueness without a CITEXT extension: the column
    -- stores what the user typed, and the index enforces uniqueness over the
    -- lowered form. Storing only the lowered form would lose the display
    -- casing; comparing without the index would allow "Ada" and "ada" to
    -- coexist and then race on login.
    username      TEXT        NOT NULL,
    display_name  TEXT        NOT NULL,

    -- The full PHC string: algorithm, parameters, salt and hash together.
    -- Storing the parameters alongside the hash is what makes the cost
    -- factor upgradable -- an old hash keeps verifying with its own
    -- parameters while new ones use the current cost.
    password_hash TEXT        NOT NULL,

    role          TEXT        NOT NULL DEFAULT 'viewer',

    -- Disabling beats deleting: a removed row would orphan whatever it is
    -- referenced by, and "this account existed and was revoked" is itself
    -- information worth keeping.
    is_active     BOOLEAN     NOT NULL DEFAULT TRUE,

    last_login_at TIMESTAMPTZ,
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at    TIMESTAMPTZ NOT NULL DEFAULT now(),

    CONSTRAINT users_role_valid  CHECK (role IN ('admin', 'viewer')),
    CONSTRAINT users_username_len CHECK (char_length(username) BETWEEN 3 AND 64),
    -- A hash, never a password. The prefix check is a cheap guard against a
    -- plaintext password reaching this column through some future code path.
    CONSTRAINT users_hash_is_phc CHECK (password_hash LIKE '$argon2id$%')
);

-- The only lookup that matters. Login is the one hot path, and it is by
-- username; is_active is checked on the row that comes back, not filtered in
-- SQL, so a partial index on it would be an index nothing uses. The index
-- catalogue test rejects exactly that, and did.
CREATE UNIQUE INDEX uq_users_username_lower ON users (lower(username));

COMMENT ON TABLE users IS
    'Dashboard accounts. Passwords are Argon2id PHC strings, never plaintext.';
