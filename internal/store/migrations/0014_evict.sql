-- A size limit for the database: a request whose body was dropped to stay
-- under it (its size, digest, headers, outcome and verdicts stay), the
-- answer bodies of its replays and forward dropped with it, and the index
-- that finds what to drop next, oldest first per class.
ALTER TABLE requests ADD COLUMN body_dropped INTEGER NOT NULL DEFAULT 0;
ALTER TABLE replays ADD COLUMN body_dropped INTEGER NOT NULL DEFAULT 0;
ALTER TABLE forwards ADD COLUMN body_dropped INTEGER NOT NULL DEFAULT 0;
-- The order check's definition each order mark was recorded under: marks
-- of another definition are not compared (a re-check cannot write again
-- the marks of requests whose bodies were dropped). '' in rows recorded
-- before: compared with any.
ALTER TABLE event_seen ADD COLUMN order_def TEXT NOT NULL DEFAULT '';
CREATE INDEX requests_evict ON requests (pinned, anomaly, body_dropped, id);
