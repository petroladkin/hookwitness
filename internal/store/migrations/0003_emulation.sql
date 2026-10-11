-- How each endpoint answers (JSON: default reply, rules, checks, variables;
-- empty means 200 with an empty body), and its request counter: requests that
-- passed the checks since the last reset, which the rules count.
ALTER TABLE endpoints ADD COLUMN config TEXT NOT NULL DEFAULT '';
ALTER TABLE endpoints ADD COLUMN counter INTEGER NOT NULL DEFAULT 0;

-- What Hookwitness did with each request (JSON: rule or default, action,
-- status and headers sent, measured delay or wait, render error).
ALTER TABLE requests ADD COLUMN outcome TEXT NOT NULL DEFAULT '';
-- When the outcome was last updated (unix ms; 0 = never), so live feeds can
-- refresh rows they have already shown.
ALTER TABLE requests ADD COLUMN changed_at INTEGER NOT NULL DEFAULT 0;
