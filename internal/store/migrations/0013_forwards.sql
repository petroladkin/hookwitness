-- Forwards: on an endpoint that passes requests through to a receiver, the
-- exchange with the receiver, one per request: what was sent and what the
-- receiver answered (its body's first 64 KiB). It goes with its request
-- (clear, delete, pruning). Header lists are JSON [[name, value], ...] in
-- order; times are unix milliseconds, UTC.
CREATE TABLE forwards (
    request_id  INTEGER PRIMARY KEY REFERENCES requests (id) ON DELETE CASCADE,
    created_at  INTEGER NOT NULL,
    url         TEXT    NOT NULL,              -- the target at the time
    options     TEXT    NOT NULL,              -- JSON: host, insecure_tls, timeout_ms, by_rule
    line        TEXT    NOT NULL DEFAULT '',   -- the request line sent
    sent        TEXT    NOT NULL DEFAULT '[]', -- the header lines written
    notes       TEXT    NOT NULL DEFAULT '[]', -- JSON: where the request sent differs from the one received
    proto       TEXT    NOT NULL DEFAULT '',
    status      INTEGER NOT NULL DEFAULT 0,    -- the receiver's; 0 when there was no answer
    reason      TEXT    NOT NULL DEFAULT '',
    headers     TEXT    NOT NULL DEFAULT '[]', -- the receiver's header lines
    body        BLOB,                          -- the receiver's first 64 KiB
    size        INTEGER NOT NULL DEFAULT 0,    -- answer body bytes; -1 when cut before its end
    truncated   INTEGER NOT NULL DEFAULT 0,
    duration_ms INTEGER NOT NULL DEFAULT 0,
    ttfb_ms     INTEGER NOT NULL DEFAULT 0,
    error_kind  TEXT    NOT NULL DEFAULT '',   -- dial, tls, timeout, reset, write, answer, refused, too_large, upgrade, busy
    error       TEXT    NOT NULL DEFAULT ''
) STRICT;
