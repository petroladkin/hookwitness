-- The signatures kept requests carried, as keyed digests (never the value):
-- a request whose signature an earlier kept request of the same endpoint
-- carried gets a note naming it. Rows go with their request (pruned,
-- cleared, its endpoint deleted).
CREATE TABLE signature_seen (
    request_id  INTEGER NOT NULL REFERENCES requests (id) ON DELETE CASCADE,
    endpoint_id INTEGER NOT NULL,
    check_no    INTEGER NOT NULL,
    digest      TEXT NOT NULL,
    PRIMARY KEY (request_id, check_no)
) STRICT;
CREATE INDEX signature_seen_digest ON signature_seen (endpoint_id, digest, request_id);
