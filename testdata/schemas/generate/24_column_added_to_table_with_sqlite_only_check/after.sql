CREATE TABLE external_disks (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    device TEXT NOT NULL,
    mountpoint TEXT NOT NULL CHECK (mountpoint GLOB '/mnt/disks/*'),
    label TEXT
) STRICT;
