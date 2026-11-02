-- An email lets a provider sign-in find its user; lower-case, unique when set.
ALTER TABLE users ADD COLUMN email TEXT;
CREATE UNIQUE INDEX users_by_email ON users (email) WHERE email IS NOT NULL;

-- Identities at OpenID Connect providers, bound by issuer and subject.
-- users.password '' means no password (provider sign-in only).
CREATE TABLE identities (
    id            INTEGER PRIMARY KEY,
    user_id       INTEGER NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    issuer        TEXT    NOT NULL,
    subject       TEXT    NOT NULL,
    provider      TEXT    NOT NULL, -- the configured name at the last sign-in, for display
    email         TEXT,             -- as the provider sent it at the last sign-in
    linked_at     INTEGER NOT NULL, -- unix ms, UTC
    last_login_at INTEGER NOT NULL,
    UNIQUE (issuer, subject),
    UNIQUE (user_id, issuer)
) STRICT;
