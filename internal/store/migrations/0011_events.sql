-- Delivery checks: each request's event (the id a sender keeps on every
-- retry; '' when attempts are counted per body, NULL when the configured
-- id was missing), its attempt among the kept requests of that event, and
-- the order check's value and partition. Rows go with their request.
CREATE TABLE event_seen (
    request_id  INTEGER PRIMARY KEY REFERENCES requests (id) ON DELETE CASCADE,
    endpoint_id INTEGER NOT NULL,
    event       TEXT,
    attempt     INTEGER NOT NULL DEFAULT 0,
    order_value REAL,
    order_group TEXT NOT NULL DEFAULT ''
) STRICT;
CREATE INDEX event_seen_by_event ON event_seen (endpoint_id, event, request_id);
CREATE INDEX event_seen_by_order ON event_seen (endpoint_id, order_group, order_value);
