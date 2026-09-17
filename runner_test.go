package sqlitemigrate

import (
	"bufio"
	"bytes"
	"context"
	"database/sql"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	_ "modernc.org/sqlite"
)

func newRunner(t *testing.T, dbPath string) *Runner {
	t.Helper()
	return &Runner{
		DBPath:          dbPath,
		SnapshotDir:     filepath.Join(filepath.Dir(dbPath), "snapshots"),
		RetainSnapshots: 5,
	}
}

func TestApply_AppliesPendingMigrationsInOrderAndRecordsThem(t *testing.T) {
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "app.db")
	r := newRunner(t, dbPath)

	migrations := []Migration{
		{Version: "20260101000000", Slug: "init", Filename: "20260101000000_init.sql", SQL: `CREATE TABLE users (id INTEGER PRIMARY KEY, name TEXT NOT NULL) STRICT;`},
		{Version: "20260102000000", Slug: "add_index", Filename: "20260102000000_add_index.sql", SQL: `CREATE INDEX idx_users_name ON users(name);`},
	}
	migrations[0].Checksum = Checksum(migrations[0].SQL)
	migrations[1].Checksum = Checksum(migrations[1].SQL)

	applied, err := r.Apply(ctx, migrations)
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if len(applied) != 2 {
		t.Fatalf("Apply applied %d migrations, want 2", len(applied))
	}

	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer func() { _ = db.Close() }()

	var count int
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM schema_migrations`).Scan(&count); err != nil {
		t.Fatalf("query bookkeeping table: %v", err)
	}
	if count != 2 {
		t.Fatalf("schema_migrations has %d rows, want 2", count)
	}

	// A second Apply with the same migrations must be a no-op: both are
	// already recorded with matching checksums.
	applied, err = r.Apply(ctx, migrations)
	if err != nil {
		t.Fatalf("second Apply: %v", err)
	}
	if len(applied) != 0 {
		t.Fatalf("second Apply re-applied %d migrations, want 0", len(applied))
	}

	// Each Apply call snapshots first; two back-to-back calls, even
	// landing in the same second, must leave two distinct snapshots, not
	// one silently overwriting the other.
	entries, err := os.ReadDir(r.snapshotDir())
	if err != nil {
		t.Fatalf("read snapshot dir: %v", err)
	}
	if len(entries) != 2 {
		names := make([]string, len(entries))
		for i, e := range entries {
			names[i] = e.Name()
		}
		t.Fatalf("snapshot dir has %d files after two Apply calls, want 2: %v", len(entries), names)
	}
}

func TestApply_RollsBackFailingMigration_DatabaseByteIdentical(t *testing.T) {
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "app.db")
	r := newRunner(t, dbPath)

	init := Migration{
		Version:  "20260101000000",
		Slug:     "init",
		Filename: "20260101000000_init.sql",
		SQL: `
			CREATE TABLE users (id INTEGER PRIMARY KEY, name TEXT NOT NULL) STRICT;
			INSERT INTO users (id, name) VALUES (1, 'alice');
		`,
	}
	init.Checksum = Checksum(init.SQL)

	if _, err := r.Apply(ctx, []Migration{init}); err != nil {
		t.Fatalf("seed Apply: %v", err)
	}

	before, err := os.ReadFile(dbPath)
	if err != nil {
		t.Fatalf("read database before failing attempt: %v", err)
	}

	// SQLite itself refuses a NOT NULL column added via ALTER TABLE
	// without a non-NULL default when the table already has rows it has
	// no way to backfill — a deliberately failing migration with a bad
	// constraint.
	bad := Migration{Version: "20260102000000", Slug: "bad", Filename: "20260102000000_bad.sql", SQL: `ALTER TABLE users ADD COLUMN email TEXT NOT NULL;`}
	bad.Checksum = Checksum(bad.SQL)

	if _, err := r.Apply(ctx, []Migration{init, bad}); err == nil {
		t.Fatal("Apply succeeded on a migration with an unsatisfiable NOT NULL constraint")
	}

	after, err := os.ReadFile(dbPath)
	if err != nil {
		t.Fatalf("read database after failed attempt: %v", err)
	}
	if !bytes.Equal(before, after) {
		t.Fatal("database file changed after a failed migration; rollback did not restore it byte-for-byte")
	}

	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer func() { _ = db.Close() }()

	var count int
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM schema_migrations`).Scan(&count); err != nil {
		t.Fatalf("query bookkeeping table: %v", err)
	}
	if count != 1 {
		t.Fatalf("schema_migrations has %d rows after failed apply, want 1 (only the seed migration)", count)
	}
}

func TestApply_DetectsChecksumTamperingOfAppliedMigration(t *testing.T) {
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "app.db")
	r := newRunner(t, dbPath)

	m := Migration{Version: "20260101000000", Slug: "init", Filename: "20260101000000_init.sql", SQL: `CREATE TABLE users (id INTEGER PRIMARY KEY) STRICT;`}
	m.Checksum = Checksum(m.SQL)

	if _, err := r.Apply(ctx, []Migration{m}); err != nil {
		t.Fatalf("seed Apply: %v", err)
	}

	tampered := m
	tampered.SQL = m.SQL + " -- edited after being applied"
	tampered.Checksum = Checksum(tampered.SQL)

	_, err := r.Apply(ctx, []Migration{tampered})
	if err == nil {
		t.Fatal("Apply accepted a migration file edited after being applied")
	}
	var mismatch *ChecksumMismatchError
	if !asChecksumMismatch(err, &mismatch) {
		t.Fatalf("Apply returned %v, want a *ChecksumMismatchError", err)
	}
}

func asChecksumMismatch(err error, target **ChecksumMismatchError) bool {
	if m, ok := err.(*ChecksumMismatchError); ok {
		*target = m
		return true
	}
	return false
}

func TestApply_ForeignKeyViolationRollsBackWholeBatch(t *testing.T) {
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "app.db")
	r := newRunner(t, dbPath)

	schema := Migration{
		Version:  "20260101000000",
		Slug:     "init",
		Filename: "20260101000000_init.sql",
		SQL: `
			CREATE TABLE parents (id INTEGER PRIMARY KEY) STRICT;
			CREATE TABLE children (id INTEGER PRIMARY KEY, parent_id INTEGER NOT NULL REFERENCES parents(id)) STRICT;
		`,
	}
	schema.Checksum = Checksum(schema.SQL)

	if _, err := r.Apply(ctx, []Migration{schema}); err != nil {
		t.Fatalf("seed Apply: %v", err)
	}

	before, err := os.ReadFile(dbPath)
	if err != nil {
		t.Fatalf("read database before violation attempt: %v", err)
	}

	// PRAGMA foreign_keys is suspended for the whole apply transaction, so
	// this insert against a nonexistent parent succeeds at exec time; only
	// the pre-commit foreign_key_check is left to catch it.
	violates := Migration{
		Version:  "20260102000000",
		Slug:     "orphan",
		Filename: "20260102000000_orphan.sql",
		SQL:      `INSERT INTO children (id, parent_id) VALUES (1, 999);`,
	}
	violates.Checksum = Checksum(violates.SQL)

	_, err = r.Apply(ctx, []Migration{schema, violates})
	if err == nil {
		t.Fatal("Apply committed a migration that leaves a dangling foreign key")
	}
	var fkErr *ForeignKeyViolationError
	if !asForeignKeyViolation(err, &fkErr) {
		t.Fatalf("Apply returned %v, want a *ForeignKeyViolationError", err)
	}

	after, err := os.ReadFile(dbPath)
	if err != nil {
		t.Fatalf("read database after violation attempt: %v", err)
	}
	if !bytes.Equal(before, after) {
		t.Fatal("database file changed after a foreign_key_check violation; rollback did not restore it byte-for-byte")
	}
}

func asForeignKeyViolation(err error, target **ForeignKeyViolationError) bool {
	if v, ok := err.(*ForeignKeyViolationError); ok {
		*target = v
		return true
	}
	return false
}

// crashHelperDBEnv, when set, tells this test binary to act as the
// subprocess for TestApply_KillMidTransactionLeavesDatabaseByteIdentical
// instead of running the normal Go test suite entry point for this
// function: open the database named by its value, start an uncommitted
// write inside a transaction on the same single-pinned-connection,
// PRAGMA-foreign-keys-suspended sequence Runner.Apply itself uses, signal
// readiness, then block forever so the parent process can kill it before
// any COMMIT.
const crashHelperDBEnv = "SQLITE_MIGRATE_CRASH_HELPER_DB"

func TestApply_KillMidTransactionLeavesDatabaseByteIdentical(t *testing.T) {
	if dbPath := os.Getenv(crashHelperDBEnv); dbPath != "" {
		runCrashHelper(dbPath)
		return
	}

	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "app.db")

	seed, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatalf("open seed database: %v", err)
	}
	if _, err := seed.ExecContext(ctx, `CREATE TABLE t (id INTEGER PRIMARY KEY, name TEXT) STRICT;`); err != nil {
		t.Fatalf("seed schema: %v", err)
	}
	if _, err := seed.ExecContext(ctx, `INSERT INTO t (id, name) VALUES (1, 'alice')`); err != nil {
		t.Fatalf("seed row: %v", err)
	}
	if err := seed.Close(); err != nil {
		t.Fatalf("close seed database: %v", err)
	}

	before, err := os.ReadFile(dbPath)
	if err != nil {
		t.Fatalf("read database before kill: %v", err)
	}

	cmd := exec.Command(os.Args[0], "-test.run=^TestApply_KillMidTransactionLeavesDatabaseByteIdentical$")
	cmd.Env = append(os.Environ(), crashHelperDBEnv+"="+dbPath)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatalf("stdout pipe: %v", err)
	}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr

	if err := cmd.Start(); err != nil {
		t.Fatalf("start crash helper subprocess: %v", err)
	}

	reader := bufio.NewReader(stdout)
	line, err := reader.ReadString('\n')
	if err != nil || strings.TrimSpace(line) != "ready" {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		t.Fatalf("crash helper did not signal readiness (line=%q, err=%v); stderr: %s", line, err, stderr.String())
	}

	if err := cmd.Process.Kill(); err != nil {
		t.Fatalf("kill crash helper subprocess: %v", err)
	}
	_ = cmd.Wait()

	after, err := os.ReadFile(dbPath)
	if err != nil {
		t.Fatalf("read database after kill: %v", err)
	}
	if !bytes.Equal(before, after) {
		t.Fatal("database file changed after the process was killed mid-transaction, before COMMIT")
	}

	verify, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatalf("reopen database after kill: %v", err)
	}
	defer func() { _ = verify.Close() }()

	var count int
	if err := verify.QueryRowContext(ctx, `SELECT COUNT(*) FROM t`).Scan(&count); err != nil {
		t.Fatalf("query database after kill: %v", err)
	}
	if count != 1 {
		t.Fatalf("row count after kill = %d, want 1 (the uncommitted insert must not persist)", count)
	}
}

func runCrashHelper(dbPath string) {
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
	db.SetMaxOpenConns(1)

	// SQLite writes a rollback journal (app.db-journal) as soon as a write
	// transaction is about to modify a page, before COMMIT, regardless of
	// page cache size. For a transaction this small, that journal is the
	// only on-disk trace of the in-flight write at kill time — the main
	// database file itself is never touched before COMMIT. PRAGMA
	// cache_size = 1 is set here only to make that explicit rather than
	// leave it to the default cache size. Killing the process before
	// COMMIT leaves the journal on disk; on reopen, SQLite finds this
	// journal's header magic zeroed — SQLite only writes that magic when
	// it syncs the journal, immediately before it starts modifying the
	// main file — so the journal is not hot, and SQLite leaves it in
	// place, ignoring it rather than replaying it. app.db is byte-identical
	// to before the transaction began because its write was never applied
	// to the file, not because anything was rolled back or replayed. The
	// stale journal itself is only cleared by the next write transaction
	// that touches the database.
	if _, err := db.Exec(`PRAGMA cache_size = 1`); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
	if _, err := db.Exec(`PRAGMA foreign_keys = OFF`); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}

	tx, err := db.Begin()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
	if _, err := tx.Exec(`INSERT INTO t (id, name) VALUES (2, 'should-not-persist')`); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}

	fmt.Println("ready")
	_ = os.Stdout.Sync()

	select {}
}
