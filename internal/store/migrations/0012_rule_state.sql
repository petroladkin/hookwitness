-- Rules by time and state: an endpoint's clock and its rules' state, as
-- JSON written in the transaction that counts a request (internal/emulate
-- State: the start in unix ms, the rate rules' recent requests and blocks).
-- '' before the first request after a reset; the reset clears it with the
-- counter.
ALTER TABLE endpoints ADD COLUMN state TEXT NOT NULL DEFAULT '';
