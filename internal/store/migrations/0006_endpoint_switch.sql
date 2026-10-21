-- An endpoint that is off keeps its path, behaviour, secrets, requests and
-- counter, but catches nothing: requests to its path go to Unmatched.
-- Every existing endpoint is on.
ALTER TABLE endpoints ADD COLUMN enabled INTEGER NOT NULL DEFAULT 1 CHECK (enabled IN (0, 1));
