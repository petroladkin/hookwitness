-- Retention in lanes: a register keeps normal requests and anomalies (not
-- answered 2xx, a failed verdict, a redelivery) under separate limits, and
-- pinned requests are never pruned by retention.
ALTER TABLE requests ADD COLUMN pinned INTEGER NOT NULL DEFAULT 0;
ALTER TABLE requests ADD COLUMN anomaly INTEGER NOT NULL DEFAULT 0;

-- An endpoint's own policy (JSON, '' for the server's defaults), how many
-- requests retention removed, and up to when each lane lost requests (unix
-- milliseconds of the newest removed one; 0: never): the strip hatches the
-- time before them.
ALTER TABLE endpoints ADD COLUMN retention TEXT NOT NULL DEFAULT '';
ALTER TABLE endpoints ADD COLUMN pruned INTEGER NOT NULL DEFAULT 0;
ALTER TABLE endpoints ADD COLUMN pruned_normal_at INTEGER NOT NULL DEFAULT 0;
ALTER TABLE endpoints ADD COLUMN pruned_anomaly_at INTEGER NOT NULL DEFAULT 0;
INSERT INTO settings (key, value) VALUES
    ('unmatched.retention', ''),
    ('unmatched.pruned', '0'),
    ('unmatched.pruned_normal_at', '0'),
    ('unmatched.pruned_anomaly_at', '0');

CREATE INDEX requests_by_lane ON requests (endpoint_id, pinned, anomaly, id);
CREATE INDEX requests_by_time ON requests (endpoint_id, received_at);

-- Existing requests classified as the server classifies new ones
-- (store.anomalyExpr, emulate.Anomalous).
UPDATE requests SET anomaly = 1 WHERE endpoint_id IS NOT NULL AND (
    too_large
    OR CASE WHEN json_valid(outcome) THEN json_extract(outcome, '$.action') IS NOT 'reply'
        OR ifnull(json_extract(outcome, '$.status'), 0) NOT BETWEEN 200 AND 299
        ELSE status NOT BETWEEN 200 AND 299 END
    OR verdict = 'fail'
    OR EXISTS (SELECT 1 FROM signature_seen s JOIN signature_seen t
        ON t.endpoint_id = s.endpoint_id AND t.digest = s.digest AND t.request_id <> s.request_id
        WHERE s.request_id = requests.id));

-- What 0.2 had already lost stays hatched: before the oldest kept request,
-- when it is not the register's first, nothing is kept.
UPDATE endpoints SET
    pruned_normal_at = ifnull((SELECT r.received_at FROM requests r WHERE r.endpoint_id = endpoints.id
        AND r.no > 1 AND r.id = (SELECT min(id) FROM requests WHERE endpoint_id = endpoints.id)), 0),
    pruned_anomaly_at = ifnull((SELECT r.received_at FROM requests r WHERE r.endpoint_id = endpoints.id
        AND r.no > 1 AND r.id = (SELECT min(id) FROM requests WHERE endpoint_id = endpoints.id)), 0);
UPDATE settings SET value = CAST(ifnull((SELECT r.received_at FROM requests r WHERE r.endpoint_id IS NULL
        AND r.no > 1 AND r.id = (SELECT min(id) FROM requests WHERE endpoint_id IS NULL)), 0) AS TEXT)
    WHERE key IN ('unmatched.pruned_normal_at', 'unmatched.pruned_anomaly_at');
