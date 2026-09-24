CREATE TABLE widgets (
    id INTEGER PRIMARY KEY,
    serial TEXT,
    key TEXT,
    UNIQUE (key)
) STRICT;
