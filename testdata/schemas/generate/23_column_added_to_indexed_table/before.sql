CREATE TABLE array_disks (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    role TEXT NOT NULL CHECK (role IN ('parity', 'data', 'cache')),
    role_index INTEGER NOT NULL CHECK (role_index >= 1),
    device TEXT NOT NULL,
    filesystem TEXT NOT NULL,
    fs_uuid TEXT NOT NULL,
    size_bytes INTEGER,
    wwn TEXT,
    serial TEXT,
    by_id_name TEXT,
    weak_identity INTEGER NOT NULL DEFAULT 0 CHECK (weak_identity IN (0, 1)),
    mountpoint TEXT NOT NULL,
    UNIQUE (role, role_index),
    UNIQUE (device),
    UNIQUE (fs_uuid),
    UNIQUE (mountpoint)
) STRICT;
CREATE INDEX array_disks_role_idx ON array_disks (role, role_index);
