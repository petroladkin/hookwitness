-- Key/value settings of this instance. The first migration exists so that the
-- migration machinery is exercised from day one; feature tables follow.
CREATE TABLE settings (
    key   TEXT PRIMARY KEY,
    value TEXT NOT NULL
) STRICT;
