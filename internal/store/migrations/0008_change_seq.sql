-- The live feed's change sequence: every change to a stored request's
-- outcome or verdicts takes the next value of feed.change_seq, in commit
-- order, and the row keeps it. A reconnecting page asks for the changes past
-- the value it last saw, so none is missed whatever the clocks say.
-- Rows of earlier versions keep 0: there is nothing to send again for them.
ALTER TABLE requests ADD COLUMN change_seq INTEGER NOT NULL DEFAULT 0;
CREATE INDEX requests_change_seq ON requests (endpoint_id, change_seq);
INSERT INTO settings (key, value) VALUES ('feed.change_seq', '0');
