-- Authorization verdicts of each request: the overall one (pass, fail, n/v;
-- empty without authorization checks) for lists and counts, and the
-- per-check list as JSON (internal/auth.Verdict).
ALTER TABLE requests ADD COLUMN verdict TEXT NOT NULL DEFAULT '';
ALTER TABLE requests ADD COLUMN verdicts TEXT NOT NULL DEFAULT '';
CREATE INDEX requests_verdict ON requests (endpoint_id, verdict);
