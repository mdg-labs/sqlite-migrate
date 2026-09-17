package sqlitemigrate

// Snapshot backs up a database via VACUUM INTO to a temporary file, then
// atomically renames it into place, pruning older snapshots per the
// configured retention policy. VACUUM INTO is required here rather than a
// raw file copy so the backup is always a consistent, single-file image
// even while other pragmas or a WAL journal are in play.

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

const snapshotTimestampLayout = "20060102150405"

// maxSnapshotNameAttempts bounds the retry loop reserveSnapshotName uses to
// find a filename nothing else has already claimed. One-second timestamp
// resolution means two Snapshot calls within the same second need a second
// attempt; this bound only exists to turn a pathological run of collisions
// into an error rather than an infinite loop.
const maxSnapshotNameAttempts = 10000

// snapshotName builds the final (non-temporary) snapshot filename for dbFile
// taken at ts: "<dbfile>.<timestamp>.snapshot", or
// "<dbfile>.<timestamp>-<attempt>.snapshot" for attempt > 0, used when the
// unsuffixed name is already taken.
func snapshotName(dbFile string, ts time.Time, attempt int) string {
	stamp := ts.UTC().Format(snapshotTimestampLayout)
	if attempt == 0 {
		return fmt.Sprintf("%s.%s.snapshot", filepath.Base(dbFile), stamp)
	}
	return fmt.Sprintf("%s.%s-%d.snapshot", filepath.Base(dbFile), stamp, attempt)
}

// reserveSnapshotName atomically claims a snapshot filename inside dir that
// nothing else has already claimed, by creating it exclusively (O_EXCL) —
// so two Snapshot calls landing in the same second, or any other name
// collision, retry under a new name instead of one silently overwriting the
// other's backup once VACUUM INTO finishes. The safety model calls the
// pre-apply snapshot "the actual undo mechanism"; it must never be
// destroyed by a later snapshot.
func reserveSnapshotName(dir, dbPath string) (string, error) {
	now := time.Now()
	for attempt := 0; attempt < maxSnapshotNameAttempts; attempt++ {
		candidate := filepath.Join(dir, snapshotName(dbPath, now, attempt))
		f, err := os.OpenFile(candidate, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
		if err == nil {
			_ = f.Close()
			return candidate, nil
		}
		if !os.IsExist(err) {
			return "", fmt.Errorf("sqlitemigrate: reserve snapshot name %q: %w", candidate, err)
		}
	}
	return "", fmt.Errorf("sqlitemigrate: could not allocate a unique snapshot name in %q after %d attempts", dir, maxSnapshotNameAttempts)
}

// Snapshot backs up the database at dbPath into a fresh file inside dir via
// VACUUM INTO. It first reserves a final filename nothing else has claimed
// (see reserveSnapshotName), writes VACUUM INTO's output to a temporary
// name next to it, fsyncs that temporary file, then renames it over its own
// just-reserved placeholder — the same guarantee an apply mid-crash needs
// from its own transaction, applied here to the backup file itself, so a
// crash during the backup can never leave a half-written file looking like
// a valid snapshot, and a name collision can never silently destroy an
// earlier snapshot. After a successful snapshot, retain prunes older
// snapshots for dbPath in dir down to at most retain of them (retain <= 0
// keeps every snapshot). Snapshot returns the final snapshot file's path.
func Snapshot(ctx context.Context, dbPath, dir string, retain int) (string, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", fmt.Errorf("sqlitemigrate: create snapshot directory %q: %w", dir, err)
	}

	final, err := reserveSnapshotName(dir, dbPath)
	if err != nil {
		return "", err
	}
	tmp := final + ".tmp"

	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		_ = os.Remove(final)
		return "", fmt.Errorf("sqlitemigrate: open database %q for snapshot: %w", dbPath, err)
	}
	defer func() { _ = db.Close() }()
	db.SetMaxOpenConns(1)

	if _, err := db.ExecContext(ctx, `VACUUM INTO ?`, tmp); err != nil {
		_ = os.Remove(tmp)
		_ = os.Remove(final)
		return "", fmt.Errorf("sqlitemigrate: VACUUM INTO %q: %w", tmp, err)
	}

	if err := syncFile(tmp); err != nil {
		_ = os.Remove(tmp)
		_ = os.Remove(final)
		return "", err
	}

	if err := os.Rename(tmp, final); err != nil {
		_ = os.Remove(tmp)
		_ = os.Remove(final)
		return "", fmt.Errorf("sqlitemigrate: rename snapshot into place: %w", err)
	}

	if err := pruneSnapshots(dbPath, dir, retain); err != nil {
		return final, err
	}

	return final, nil
}

func syncFile(path string) error {
	f, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		return fmt.Errorf("sqlitemigrate: open snapshot %q to sync: %w", path, err)
	}
	syncErr := f.Sync()
	closeErr := f.Close()
	if syncErr != nil {
		return fmt.Errorf("sqlitemigrate: sync snapshot %q: %w", path, syncErr)
	}
	if closeErr != nil {
		return fmt.Errorf("sqlitemigrate: close snapshot %q after sync: %w", path, closeErr)
	}
	return nil
}

// pruneSnapshots removes the oldest snapshot files for dbPath in dir until
// at most retain remain. retain <= 0 disables pruning: every snapshot is
// kept.
func pruneSnapshots(dbPath, dir string, retain int) error {
	if retain <= 0 {
		return nil
	}

	prefix := filepath.Base(dbPath) + "."
	entries, err := os.ReadDir(dir)
	if err != nil {
		return fmt.Errorf("sqlitemigrate: list snapshot directory %q: %w", dir, err)
	}

	var names []string
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasPrefix(name, prefix) || !strings.HasSuffix(name, ".snapshot") {
			continue
		}
		names = append(names, name)
	}
	sort.Strings(names)

	if len(names) <= retain {
		return nil
	}
	for _, name := range names[:len(names)-retain] {
		if err := os.Remove(filepath.Join(dir, name)); err != nil {
			return fmt.Errorf("sqlitemigrate: prune snapshot %q: %w", name, err)
		}
	}
	return nil
}
