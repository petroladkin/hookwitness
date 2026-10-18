-- People who may use the admin once login is on (the first user turns it
-- on). name is stored lower-case; password is a secret.HashToken record.
CREATE TABLE users (
    id                  INTEGER PRIMARY KEY,
    name                TEXT    NOT NULL UNIQUE,
    role                TEXT    NOT NULL CHECK (role IN ('user', 'admin')),
    password            TEXT    NOT NULL,
    created_at          INTEGER NOT NULL, -- unix milliseconds, UTC
    password_changed_at INTEGER NOT NULL
) STRICT;

-- Browser logins. hash is the SHA-256 of the session id in the cookie: the
-- id itself is never stored.
CREATE TABLE sessions (
    id           INTEGER PRIMARY KEY,
    hash         BLOB    NOT NULL UNIQUE,
    user_id      INTEGER NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    created_at   INTEGER NOT NULL,
    last_seen_at INTEGER NOT NULL
) STRICT;
CREATE INDEX sessions_by_user ON sessions (user_id);

-- Personal tokens for scripts and agents: hkw_<public_id>_<secret>. hash is a
-- secret.HashToken record of the whole token.
CREATE TABLE tokens (
    id           INTEGER PRIMARY KEY,
    public_id    TEXT    NOT NULL UNIQUE,
    user_id      INTEGER NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    name         TEXT    NOT NULL,
    hash         TEXT    NOT NULL,
    created_at   INTEGER NOT NULL,
    last_used_at INTEGER,
    UNIQUE (user_id, name)
) STRICT;
