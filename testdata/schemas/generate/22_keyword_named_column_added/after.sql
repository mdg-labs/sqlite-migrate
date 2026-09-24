CREATE TABLE widgets (
    id INTEGER PRIMARY KEY,
    serial TEXT,
    key TEXT,
    value TEXT,
    UNIQUE (key)
) STRICT;
