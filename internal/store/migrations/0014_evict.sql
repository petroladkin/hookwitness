-- A size limit for the database: a request whose body was dropped to stay
-- under it (its size, digest, headers, outcome and verdicts stay), and the
-- index that finds what to drop next, oldest first per class.
ALTER TABLE requests ADD COLUMN body_dropped INTEGER NOT NULL DEFAULT 0;
CREATE INDEX requests_evict ON requests (pinned, anomaly, body_dropped, id);
