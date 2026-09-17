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
	"regexp"
	"sort"
	"strconv"
	"sync"
	"time"

	_ "modernc.org/sqlite"
)

// SnapshotWarning reports that Snapshot successfully created and renamed a
// backup file at a final, valid path, but a step that affects only that
// backup's durability or housekeeping — never its validity as a restorable,
// byte-correct image — failed afterward. Callers may treat the returned
// path as a good backup and proceed; Runner.Apply does.
type SnapshotWarning struct {
	Err error
}

func (e *SnapshotWarning) Error() string { return e.Err.Error() }
func (e *SnapshotWarning) Unwrap() error { return e.Err }

// snapshotTimestampLayout parses the one-second-resolution timestamp used by
// snapshot files created under the previous naming scheme
// ("<dbfile>.<ts>.snapshot" or "<dbfile>.<ts>-<attempt>.snapshot"). New
// snapshot names no longer use it; it exists only so pruneSnapshots keeps
// ordering those older files correctly after an upgrade.
const snapshotTimestampLayout = "20060102150405"

// maxSnapshotNameAttempts bounds the retry loop reserveSnapshotName uses to
// find a filename nothing else has already claimed. Names are built from a
// strictly increasing nanosecond timestamp (see nextSnapshotTimestamp), so a
// collision should never happen; this bound only exists to turn a
// pathological run of collisions — e.g. another process reserving the exact
// same name at the exact same nanosecond — into an error rather than an
// infinite loop.
const maxSnapshotNameAttempts = 10000

var (
	snapshotClockMu   sync.Mutex
	snapshotClockLast int64
)

// nextSnapshotTimestamp returns a nanosecond timestamp that is always
// strictly greater than every value it has previously returned in this
// process, even when two calls land on the same wall-clock instant or the
// clock's resolution can't otherwise distinguish them. This is what makes
// snapshot names collision-free by construction instead of by retrying
// under an attempt suffix.
func nextSnapshotTimestamp() int64 {
	snapshotClockMu.Lock()
	defer snapshotClockMu.Unlock()
	now := time.Now().UnixNano()
	if now <= snapshotClockLast {
		now = snapshotClockLast + 1
	}
	snapshotClockLast = now
	return now
}

// snapshotName builds the final (non-temporary) snapshot filename for dbFile
// from a strictly increasing nanosecond timestamp (see
// nextSnapshotTimestamp): "<dbfile>.<20-digit nanoseconds>.snapshot". The
// fixed-width, zero-padded digits keep names lexicographically sortable by
// construction, so no attempt suffix is ever needed to keep them ordered.
func snapshotName(dbFile string, ns int64) string {
	return fmt.Sprintf("%s.%020d.snapshot", filepath.Base(dbFile), ns)
}

// reserveSnapshotName atomically claims a snapshot filename inside dir that
// nothing else has already claimed, by creating it exclusively (O_EXCL) —
// so a name collision retries under a new name instead of one silently
// overwriting another backup once VACUUM INTO finishes. The safety model
// calls the pre-apply snapshot "the actual undo mechanism"; it must never be
// destroyed by a later snapshot.
func reserveSnapshotName(dir, dbPath string) (string, error) {
	for attempt := 0; attempt < maxSnapshotNameAttempts; attempt++ {
		candidate := filepath.Join(dir, snapshotName(dbPath, nextSnapshotTimestamp()))
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

	if _, err := db.ExecContext(ctx, fmt.Sprintf("PRAGMA busy_timeout = %d", BusyTimeoutMillis)); err != nil {
		_ = os.Remove(final)
		return "", fmt.Errorf("sqlitemigrate: set busy_timeout: %w", err)
	}

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

	// The rename above is on disk and final is a valid, complete snapshot
	// from here on regardless of what follows; syncing the directory entry
	// only strengthens the guarantee that the rename survives a crash, so
	// its failure is a durability warning, not a reason to discard a good
	// backup.
	if err := syncDir(dir); err != nil {
		return final, &SnapshotWarning{Err: err}
	}

	if err := pruneSnapshots(dbPath, dir, retain); err != nil {
		return final, &SnapshotWarning{Err: err}
	}

	return final, nil
}

func syncDir(dir string) error {
	f, err := os.Open(dir)
	if err != nil {
		return fmt.Errorf("sqlitemigrate: open snapshot directory %q to sync: %w", dir, err)
	}
	syncErr := f.Sync()
	closeErr := f.Close()
	if syncErr != nil {
		return fmt.Errorf("sqlitemigrate: sync snapshot directory %q: %w", dir, syncErr)
	}
	if closeErr != nil {
		return fmt.Errorf("sqlitemigrate: close snapshot directory %q after sync: %w", dir, closeErr)
	}
	return nil
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

// newSnapshotNamePattern matches names built by the current snapshotName for
// dbFile: a fixed-width, zero-padded nanosecond timestamp with no attempt
// suffix. Anchored on dbFile's exact base name, not just checked as a
// prefix — a bare prefix test would let pruning for "app" match and delete
// "app.db"'s snapshots whenever the two share a SnapshotDir.
func newSnapshotNamePattern(dbFile string) *regexp.Regexp {
	return regexp.MustCompile(`^` + regexp.QuoteMeta(filepath.Base(dbFile)) + `\.([0-9]{20})\.snapshot$`)
}

// oldSnapshotNamePattern matches names built by the one-second-resolution
// scheme this replaces, with its optional numeric attempt suffix
// ("...-10.snapshot" vs "...-2.snapshot") — still present on disk in any
// directory that has snapshots from before an upgrade. Anchored the same
// way as newSnapshotNamePattern, for the same reason.
func oldSnapshotNamePattern(dbFile string) *regexp.Regexp {
	return regexp.MustCompile(`^` + regexp.QuoteMeta(filepath.Base(dbFile)) + `\.([0-9]{14})(?:-([0-9]+))?\.snapshot$`)
}

// snapshotFileAge parses name against newPattern/oldPattern (dbFile's own
// patterns from newSnapshotNamePattern/oldSnapshotNamePattern) into a
// nanosecond value usable only to order snapshots relative to each other,
// not as a real timestamp: an old-format name's attempt suffix (bounded
// well under a second's worth of nanoseconds) is folded in as a sub-second
// tiebreaker so files from the same one-second bucket still sort in the
// order they were reserved. A name that doesn't belong to dbFile at all —
// including another database's, in a shared SnapshotDir — matches neither
// pattern.
func snapshotFileAge(newPattern, oldPattern *regexp.Regexp, name string) (int64, bool) {
	if m := newPattern.FindStringSubmatch(name); m != nil {
		ns, err := strconv.ParseInt(m[1], 10, 64)
		if err != nil {
			return 0, false
		}
		return ns, true
	}
	if m := oldPattern.FindStringSubmatch(name); m != nil {
		t, err := time.Parse(snapshotTimestampLayout, m[1])
		if err != nil {
			return 0, false
		}
		attempt := 0
		if m[2] != "" {
			attempt, _ = strconv.Atoi(m[2])
		}
		return t.UnixNano() + int64(attempt), true
	}
	return 0, false
}

// pruneSnapshots removes the oldest snapshot files for dbPath in dir until
// at most retain remain. retain <= 0 disables pruning: every snapshot is
// kept.
func pruneSnapshots(dbPath, dir string, retain int) error {
	if retain <= 0 {
		return nil
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		return fmt.Errorf("sqlitemigrate: list snapshot directory %q: %w", dir, err)
	}

	newPattern := newSnapshotNamePattern(dbPath)
	oldPattern := oldSnapshotNamePattern(dbPath)

	type snapshotFile struct {
		name string
		age  int64
	}

	var files []snapshotFile
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() {
			continue
		}
		age, ok := snapshotFileAge(newPattern, oldPattern, name)
		if !ok {
			continue
		}
		files = append(files, snapshotFile{name: name, age: age})
	}
	sort.Slice(files, func(i, j int) bool {
		return files[i].age < files[j].age
	})

	if len(files) <= retain {
		return nil
	}
	for _, f := range files[:len(files)-retain] {
		if err := os.Remove(filepath.Join(dir, f.name)); err != nil {
			return fmt.Errorf("sqlitemigrate: prune snapshot %q: %w", f.name, err)
		}
	}
	return nil
}
