package sqlitemigrate

// Snapshot backs up a database via VACUUM INTO to a temporary file, then
// atomically renames it into place, pruning older snapshots per the
// configured retention policy. VACUUM INTO is required here rather than a
// raw file copy so the backup is always a consistent, single-file image
// even while other pragmas or a WAL journal are in play.
