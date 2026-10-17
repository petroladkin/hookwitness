-- Endpoints are exact paths that catch requests. last_no numbers the requests
-- an endpoint has caught (the register number), and keeps counting after a
-- clear or a prune, so a number is never reused within an endpoint.
CREATE TABLE endpoints (
    id         INTEGER PRIMARY KEY,
    path       TEXT    NOT NULL UNIQUE,
    created_at INTEGER NOT NULL, -- unix milliseconds, UTC
    last_no    INTEGER NOT NULL DEFAULT 0
) STRICT;

-- Caught requests. endpoint_id NULL is the Unmatched register: requests to a
-- path no endpoint has. id is AUTOINCREMENT so it is never reused, which the
-- live feed relies on (it is the SSE event id).
CREATE TABLE requests (
    id          INTEGER PRIMARY KEY AUTOINCREMENT,
    endpoint_id INTEGER REFERENCES endpoints (id) ON DELETE CASCADE,
    no          INTEGER NOT NULL, -- number within its register
    received_at INTEGER NOT NULL, -- unix milliseconds, UTC
    method      TEXT    NOT NULL,
    path        TEXT    NOT NULL, -- as received, percent-encoding kept
    raw_query   TEXT    NOT NULL,
    proto       TEXT    NOT NULL,
    remote_addr TEXT    NOT NULL,
    headers     TEXT    NOT NULL, -- JSON [[name, value], ...] in received order
    body        BLOB,             -- NULL when the body was over the limit
    body_sha256 BLOB,
    size        INTEGER NOT NULL, -- body bytes; for a refused body the declared length, -1 if unknown
    too_large   INTEGER NOT NULL DEFAULT 0,
    status      INTEGER NOT NULL  -- the status hookwitness answered with
) STRICT;

CREATE INDEX requests_by_register ON requests (endpoint_id, id);
CREATE INDEX requests_by_body ON requests (endpoint_id, body_sha256);

-- The Unmatched register's numbering lives with the other settings.
INSERT INTO settings (key, value) VALUES ('unmatched.last_no', '0');
