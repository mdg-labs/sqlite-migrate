package sqlitemigrate

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
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
