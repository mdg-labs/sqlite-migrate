package sqlitemigrate

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"sync"
	"testing"

	_ "modernc.org/sqlite"
)

func TestSnapshot_CapturesRealDatabaseContent(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "app.db")

	db := openFileDB(t, dbPath)
	if _, err := db.ExecContext(ctx, `CREATE TABLE t (id INTEGER PRIMARY KEY, name TEXT) STRICT;`); err != nil {
		t.Fatalf("seed database: %v", err)
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO t (id, name) VALUES (1, 'alice')`); err != nil {
		t.Fatalf("insert row: %v", err)
	}

	snapshotDir := filepath.Join(dir, "snapshots")
	path, err := Snapshot(ctx, dbPath, snapshotDir, 0)
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}

	if _, err := os.Stat(path); err != nil {
		t.Fatalf("snapshot file missing: %v", err)
	}

	entries, err := os.ReadDir(snapshotDir)
	if err != nil {
		t.Fatalf("read snapshot dir: %v", err)
	}
	for _, e := range entries {
		if filepath.Ext(e.Name()) == ".tmp" {
			t.Errorf("temp snapshot file left behind: %s", e.Name())
		}
	}

	snapDB, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("open snapshot: %v", err)
	}
	defer func() { _ = snapDB.Close() }()

	var name string
	if err := snapDB.QueryRowContext(ctx, `SELECT name FROM t WHERE id = 1`).Scan(&name); err != nil {
		t.Fatalf("query snapshot: %v", err)
	}
	if name != "alice" {
		t.Errorf("snapshot row name = %q, want alice", name)
	}
}

func TestSnapshot_ConsecutiveCallsNeverOverwritePreviousSnapshot(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "app.db")
	snapshotDir := filepath.Join(dir, "snapshots")

	db := openFileDB(t, dbPath)
	if _, err := db.ExecContext(ctx, `CREATE TABLE t (id INTEGER PRIMARY KEY, name TEXT) STRICT;`); err != nil {
		t.Fatalf("seed database: %v", err)
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO t (id, name) VALUES (1, 'alice')`); err != nil {
		t.Fatalf("insert row: %v", err)
	}

	// Two Snapshot calls in immediate succession, likely landing in the
	// same second, must still never collide: a snapshot is the safety
	// model's undo mechanism and must never be silently destroyed by a
	// later one.
	first, err := Snapshot(ctx, dbPath, snapshotDir, 0)
	if err != nil {
		t.Fatalf("first Snapshot: %v", err)
	}

	if _, err := db.ExecContext(ctx, `INSERT INTO t (id, name) VALUES (2, 'bob')`); err != nil {
		t.Fatalf("insert second row: %v", err)
	}

	second, err := Snapshot(ctx, dbPath, snapshotDir, 0)
	if err != nil {
		t.Fatalf("second Snapshot: %v", err)
	}

	if first == second {
		t.Fatalf("two Snapshot calls returned the same path %q; the first snapshot was silently overwritten", first)
	}

	entries, err := os.ReadDir(snapshotDir)
	if err != nil {
		t.Fatalf("read snapshot dir: %v", err)
	}
	if len(entries) != 2 {
		names := make([]string, len(entries))
		for i, e := range entries {
			names[i] = e.Name()
		}
		t.Fatalf("snapshot dir has %d files after two Snapshot calls, want 2: %v", len(entries), names)
	}

	firstDB, err := sql.Open("sqlite", first)
	if err != nil {
		t.Fatalf("open first snapshot: %v", err)
	}
	defer func() { _ = firstDB.Close() }()

	var count int
	if err := firstDB.QueryRowContext(ctx, `SELECT COUNT(*) FROM t`).Scan(&count); err != nil {
		t.Fatalf("query first snapshot: %v", err)
	}
	if count != 1 {
		t.Fatalf("first snapshot has %d rows after a later Snapshot call, want 1 (it must still reflect state at the time it was taken, not the state at some later overwrite)", count)
	}
}

func TestSnapshot_ConcurrentCallsAllSucceedWithDistinctNames(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "app.db")
	snapshotDir := filepath.Join(dir, "snapshots")

	db := openFileDB(t, dbPath)
	if _, err := db.ExecContext(ctx, `CREATE TABLE t (id INTEGER PRIMARY KEY) STRICT;`); err != nil {
		t.Fatalf("seed database: %v", err)
	}

	// Fire many Snapshot calls at the same directory concurrently: with
	// nanosecond-resolution, strictly increasing names, no two calls should
	// ever need the O_EXCL retry loop to avoid colliding.
	const n = 128
	var wg sync.WaitGroup
	paths := make([]string, n)
	errs := make([]error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			paths[i], errs[i] = Snapshot(ctx, dbPath, snapshotDir, 0)
		}(i)
	}
	wg.Wait()

	seen := make(map[string]bool, n)
	for i, err := range errs {
		if err != nil {
			t.Fatalf("Snapshot call %d failed: %v", i, err)
		}
		if seen[paths[i]] {
			t.Fatalf("Snapshot call %d returned duplicate path %q", i, paths[i])
		}
		seen[paths[i]] = true
		if !newSnapshotNamePattern(dbPath).MatchString(filepath.Base(paths[i])) {
			t.Errorf("Snapshot call %d returned name %q not in the collision-free nanosecond format; it must have needed the attempt-retry fallback", i, filepath.Base(paths[i]))
		}
	}

	entries, err := os.ReadDir(snapshotDir)
	if err != nil {
		t.Fatalf("read snapshot dir: %v", err)
	}
	if len(entries) != n {
		names := make([]string, len(entries))
		for i, e := range entries {
			names[i] = e.Name()
		}
		t.Fatalf("snapshot dir has %d files after %d concurrent Snapshot calls, want %d: %v", len(entries), n, n, names)
	}
}

func TestSnapshot_PrunesOldAndNewFormatSnapshotsTogether(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "app.db")

	db := openFileDB(t, dbPath)
	if _, err := db.ExecContext(ctx, `CREATE TABLE t (id INTEGER PRIMARY KEY) STRICT;`); err != nil {
		t.Fatalf("seed database: %v", err)
	}

	snapshotDir := filepath.Join(dir, "snapshots")
	if err := os.MkdirAll(snapshotDir, 0o755); err != nil {
		t.Fatalf("mkdir snapshot dir: %v", err)
	}

	// A directory upgraded from the previous naming scheme realistically
	// holds a mix of old-format (one-second timestamp, optional numeric
	// attempt suffix) and new-format (fixed-width nanosecond) names at once.
	oldest := filepath.Join(snapshotDir, "app.db.20200101000000.snapshot")
	middle := filepath.Join(snapshotDir, "app.db.20210101000000-3.snapshot")
	for _, p := range []string{oldest, middle} {
		if err := os.WriteFile(p, []byte("placeholder"), 0o644); err != nil {
			t.Fatalf("seed snapshot file %q: %v", p, err)
		}
	}

	newest, err := Snapshot(ctx, dbPath, snapshotDir, 2)
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}

	entries, err := os.ReadDir(snapshotDir)
	if err != nil {
		t.Fatalf("read snapshot dir: %v", err)
	}
	if len(entries) != 2 {
		names := make([]string, len(entries))
		for i, e := range entries {
			names[i] = e.Name()
		}
		t.Fatalf("snapshot dir has %d files after retention pruning, want 2: %v", len(entries), names)
	}

	if _, err := os.Stat(oldest); !os.IsNotExist(err) {
		t.Errorf("oldest snapshot %q was not pruned", oldest)
	}
	for _, p := range []string{middle, newest} {
		if _, err := os.Stat(p); err != nil {
			t.Errorf("retained snapshot %q missing: %v", p, err)
		}
	}
}

func TestSnapshot_PrunesOldSnapshotsPastRetention(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "app.db")

	db := openFileDB(t, dbPath)
	if _, err := db.ExecContext(ctx, `CREATE TABLE t (id INTEGER PRIMARY KEY) STRICT;`); err != nil {
		t.Fatalf("seed database: %v", err)
	}

	snapshotDir := filepath.Join(dir, "snapshots")
	if err := os.MkdirAll(snapshotDir, 0o755); err != nil {
		t.Fatalf("mkdir snapshot dir: %v", err)
	}
	// Pre-seed two older snapshots with fixed, distinct timestamps rather
	// than sleeping between real Snapshot calls to force distinct
	// filenames: pruneSnapshots only needs the filenames to sort in the
	// right order.
	oldest := filepath.Join(snapshotDir, "app.db.20200101000000.snapshot")
	middle := filepath.Join(snapshotDir, "app.db.20210101000000.snapshot")
	for _, p := range []string{oldest, middle} {
		if err := os.WriteFile(p, []byte("placeholder"), 0o644); err != nil {
			t.Fatalf("seed snapshot file %q: %v", p, err)
		}
	}

	newest, err := Snapshot(ctx, dbPath, snapshotDir, 2)
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}

	entries, err := os.ReadDir(snapshotDir)
	if err != nil {
		t.Fatalf("read snapshot dir: %v", err)
	}
	if len(entries) != 2 {
		names := make([]string, len(entries))
		for i, e := range entries {
			names[i] = e.Name()
		}
		t.Fatalf("snapshot dir has %d files after retention pruning, want 2: %v", len(entries), names)
	}

	if _, err := os.Stat(oldest); !os.IsNotExist(err) {
		t.Errorf("oldest snapshot %q was not pruned", oldest)
	}
	for _, p := range []string{middle, newest} {
		if _, err := os.Stat(p); err != nil {
			t.Errorf("retained snapshot %q missing: %v", p, err)
		}
	}
}

func TestSnapshot_PrunesByNumericAttemptNotLexicographicOrder(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "app.db")

	db := openFileDB(t, dbPath)
	if _, err := db.ExecContext(ctx, `CREATE TABLE t (id INTEGER PRIMARY KEY) STRICT;`); err != nil {
		t.Fatalf("seed database: %v", err)
	}

	snapshotDir := filepath.Join(dir, "snapshots")
	if err := os.MkdirAll(snapshotDir, 0o755); err != nil {
		t.Fatalf("mkdir snapshot dir: %v", err)
	}
	// Same-second collision names: a plain lexicographic sort orders
	// "-10" before "-2" (ASCII '1' < '2'), which would make pruneSnapshots
	// delete the numerically newest attempt while keeping older ones.
	stamp := "20260101000000"
	oldest := filepath.Join(snapshotDir, "app.db."+stamp+".snapshot")
	attempt2 := filepath.Join(snapshotDir, "app.db."+stamp+"-2.snapshot")
	attempt10 := filepath.Join(snapshotDir, "app.db."+stamp+"-10.snapshot")
	for _, p := range []string{oldest, attempt2, attempt10} {
		if err := os.WriteFile(p, []byte("placeholder"), 0o644); err != nil {
			t.Fatalf("seed snapshot file %q: %v", p, err)
		}
	}

	if err := pruneSnapshots(dbPath, snapshotDir, 2); err != nil {
		t.Fatalf("pruneSnapshots: %v", err)
	}

	if _, err := os.Stat(oldest); !os.IsNotExist(err) {
		t.Errorf("oldest snapshot %q was not pruned", oldest)
	}
	for _, p := range []string{attempt2, attempt10} {
		if _, err := os.Stat(p); err != nil {
			t.Errorf("retained snapshot %q missing: %v", p, err)
		}
	}
}

func TestSnapshot_PruneDoesNotCrossDatabasesSharingASnapshotDir(t *testing.T) {
	dir := t.TempDir()
	snapshotDir := filepath.Join(dir, "snapshots")
	if err := os.MkdirAll(snapshotDir, 0o755); err != nil {
		t.Fatalf("mkdir snapshot dir: %v", err)
	}

	// "app" is a dotted prefix of "app.db": a bare filepath.Base(dbPath)+"."
	// HasPrefix test on "app.db.20200101000000.snapshot" is satisfied by
	// "app." just as much as it is by "app.db.", so pruning for "app" must
	// not be able to see, let alone delete, snapshots that belong to
	// "app.db".
	shortDB := filepath.Join(dir, "app")
	longDB := filepath.Join(dir, "app.db")

	// longSnapshot is the older file: under the bug, both files match "app."
	// as a prefix, so with retain=1 the merged, age-sorted list has 2
	// entries and the oldest — longSnapshot, another database's only
	// backup — is the one pruning removes.
	longSnapshot := filepath.Join(snapshotDir, filepath.Base(longDB)+".20200101000000.snapshot")
	shortSnapshot := filepath.Join(snapshotDir, filepath.Base(shortDB)+".20210101000000.snapshot")
	for _, p := range []string{longSnapshot, shortSnapshot} {
		if err := os.WriteFile(p, []byte("placeholder"), 0o644); err != nil {
			t.Fatalf("seed snapshot file %q: %v", p, err)
		}
	}

	if err := pruneSnapshots(shortDB, snapshotDir, 1); err != nil {
		t.Fatalf("pruneSnapshots: %v", err)
	}

	if _, err := os.Stat(longSnapshot); err != nil {
		t.Errorf("longDB's snapshot %q was pruned by a call for shortDB: %v", longSnapshot, err)
	}
	if _, err := os.Stat(shortSnapshot); err != nil {
		t.Errorf("shortDB's own retained snapshot %q missing: %v", shortSnapshot, err)
	}
}
