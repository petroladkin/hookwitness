-- Replays: a caught request sent again to a URL, and what that target
-- answered. They go with their request (clear, delete, pruning); the store
-- keeps the newest 50 per request. Header lists are JSON [[name, value], ...]
-- in order; times are unix milliseconds, UTC.
CREATE TABLE replays (
    id          INTEGER PRIMARY KEY AUTOINCREMENT,
    request_id  INTEGER NOT NULL REFERENCES requests (id) ON DELETE CASCADE,
    created_at  INTEGER NOT NULL,
    by_user     TEXT    NOT NULL DEFAULT '', -- who replayed; '' without login
    url         TEXT    NOT NULL,            -- the target as given (never with a user name or password)
    options     TEXT    NOT NULL,            -- JSON: host, insecure_tls, timeout_ms
    line        TEXT    NOT NULL,            -- the request line sent
    sent        TEXT    NOT NULL,            -- the header lines written
    notes       TEXT    NOT NULL DEFAULT '[]', -- JSON: where the request sent differs from the one received
    proto       TEXT    NOT NULL DEFAULT '',
    status      INTEGER NOT NULL DEFAULT 0,  -- 0 when there was no answer
    reason      TEXT    NOT NULL DEFAULT '',
    headers     TEXT    NOT NULL DEFAULT '[]', -- the answer's header lines
    body        BLOB,                        -- the answer's first 1 MiB
    size        INTEGER NOT NULL DEFAULT 0,  -- answer body bytes; -1 when cut before its end
    truncated   INTEGER NOT NULL DEFAULT 0,
    duration_ms INTEGER NOT NULL DEFAULT 0,
    ttfb_ms     INTEGER NOT NULL DEFAULT 0,
    error_kind  TEXT    NOT NULL DEFAULT '', -- dial, tls, timeout, reset, write, answer
    error       TEXT    NOT NULL DEFAULT ''
) STRICT;

CREATE INDEX replays_by_request ON replays (request_id, id);
