package main

// cli_test.go exercises apply/check/verify/status against a real on-disk
// SQLite database, wired up the same way an end user would drive them from
// the shell: generate builds the migration journal, then apply/check/
// verify/status consume it as separate os.Args-shaped invocations.

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestCLI_MultiMigrationUpgradeSequence runs the acceptance scenario for
// Phase 7: generate several migrations (a new table, an additive column,
// and a change requiring a full rebuild), apply them against a real SQLite
// file, verify the result, and check status — end to end, with dry-run
// apply proven not to touch the database first.
func TestCLI_MultiMigrationUpgradeSequence(t *testing.T) {
	dir, schemaPath, migrationsDir := newProject(t)
	dbPath := filepath.Join(dir, "app.db")
	opts := baseOptions(schemaPath, migrationsDir)

	generateStep := func(sql string) {
		t.Helper()
		writeSchema(t, schemaPath, sql)
		res, err := generate(context.Background(), opts, strings.NewReader(""), &bytes.Buffer{}, &bytes.Buffer{})
		if err != nil {
			t.Fatalf("generate: %v", err)
		}
		if !res.written {
			t.Fatalf("expected a migration to be written")
		}
	}

	// Step 1: cold repo, new table (scenario 01).
	generateStep(`CREATE TABLE users (
    id INTEGER PRIMARY KEY,
    email TEXT
) STRICT;`)

	checkOut, checkErr := new(bytes.Buffer), new(bytes.Buffer)
	if code := RunCheck([]string{"-schema", schemaPath, "-dir", migrationsDir}, checkOut, checkErr); code != 0 {
		t.Fatalf("check after first generate: exit %d, stderr: %s", code, checkErr.String())
	}
	if !strings.Contains(checkOut.String(), "ok:") {
		t.Fatalf("expected check to report ok, got: %s", checkOut.String())
	}

	// Dry-run apply must not touch (let alone create) the target database.
	dryOut, dryErr := new(bytes.Buffer), new(bytes.Buffer)
	if code := RunApply([]string{"-db", dbPath, "-dir", migrationsDir}, dryOut, dryErr); code != 0 {
		t.Fatalf("dry-run apply: exit %d, stderr: %s", code, dryErr.String())
	}
	if !strings.Contains(dryOut.String(), "dry run: 1 pending migration(s)") {
		t.Fatalf("expected dry-run apply to report 1 pending migration, got: %s", dryOut.String())
	}
	if !strings.Contains(dryOut.String(), "rerun with --yes to apply") {
		t.Fatalf("expected dry-run apply to mention --yes, got: %s", dryOut.String())
	}
	if _, err := os.Stat(dbPath); !os.IsNotExist(err) {
		t.Fatalf("dry-run apply must not create the database file, stat err: %v", err)
	}

	// Real apply with --yes.
	applyOut, applyErr := new(bytes.Buffer), new(bytes.Buffer)
	if code := RunApply([]string{"-db", dbPath, "-dir", migrationsDir, "-yes"}, applyOut, applyErr); code != 0 {
		t.Fatalf("apply --yes: exit %d, stderr: %s", code, applyErr.String())
	}
	if !strings.Contains(applyOut.String(), "applied 1 migration(s)") {
		t.Fatalf("expected apply to report 1 applied migration, got: %s", applyOut.String())
	}
	if _, err := os.Stat(dbPath); err != nil {
		t.Fatalf("expected apply --yes to create the database file: %v", err)
	}

	verifyOut, verifyErr := new(bytes.Buffer), new(bytes.Buffer)
	if code := RunVerify([]string{"-db", dbPath, "-dir", migrationsDir}, verifyOut, verifyErr); code != 0 {
		t.Fatalf("verify after first apply: exit %d, stderr: %s", code, verifyErr.String())
	}
	if !strings.Contains(verifyOut.String(), "ok:") {
		t.Fatalf("expected verify to report no drift, got: %s", verifyOut.String())
	}

	statusOut, statusErr := new(bytes.Buffer), new(bytes.Buffer)
	if code := RunStatus([]string{"-db", dbPath, "-dir", migrationsDir}, statusOut, statusErr); code != 0 {
		t.Fatalf("status after first apply: exit %d, stderr: %s", code, statusErr.String())
	}
	if !strings.Contains(statusOut.String(), "applied at") {
		t.Fatalf("expected status to report the migration as applied, got: %s", statusOut.String())
	}

	// Step 2: additive column (scenario 02) — a second migration, applied
	// on top of the first.
	generateStep(`CREATE TABLE users (
    id INTEGER PRIMARY KEY,
    email TEXT,
    name TEXT
) STRICT;`)

	dryOut2 := new(bytes.Buffer)
	if code := RunApply([]string{"-db", dbPath, "-dir", migrationsDir}, dryOut2, new(bytes.Buffer)); code != 0 {
		t.Fatalf("dry-run apply after second generate: exit %d", code)
	}
	if !strings.Contains(dryOut2.String(), "dry run: 1 pending migration(s)") {
		t.Fatalf("expected exactly 1 newly pending migration, got: %s", dryOut2.String())
	}

	applyOut2 := new(bytes.Buffer)
	if code := RunApply([]string{"-db", dbPath, "-dir", migrationsDir, "-yes"}, applyOut2, new(bytes.Buffer)); code != 0 {
		t.Fatalf("apply --yes for second migration: exit %d", code)
	}
	if !strings.Contains(applyOut2.String(), "applied 1 migration(s)") {
		t.Fatalf("expected exactly 1 newly applied migration, got: %s", applyOut2.String())
	}

	// Step 3: a UNIQUE constraint addition (scenario 09) — forces a full
	// table rebuild rather than a plain ALTER TABLE, exercising apply
	// against rebuild-generated SQL specifically.
	generateStep(`CREATE TABLE users (
    id INTEGER PRIMARY KEY,
    email TEXT UNIQUE,
    name TEXT
) STRICT;`)

	if code := RunApply([]string{"-db", dbPath, "-dir", migrationsDir, "-yes"}, new(bytes.Buffer), new(bytes.Buffer)); code != 0 {
		t.Fatalf("apply --yes for rebuild migration: exit %d", code)
	}

	if code := RunVerify([]string{"-db", dbPath, "-dir", migrationsDir}, new(bytes.Buffer), new(bytes.Buffer)); code != 0 {
		t.Fatalf("verify after rebuild migration applied")
	}

	finalStatusOut := new(bytes.Buffer)
	if code := RunStatus([]string{"-db", dbPath, "-dir", migrationsDir}, finalStatusOut, new(bytes.Buffer)); code != 0 {
		t.Fatalf("final status: exit %d", code)
	}
	if strings.Contains(finalStatusOut.String(), "pending") {
		t.Fatalf("expected every migration to be applied, got: %s", finalStatusOut.String())
	}
	if got := strings.Count(finalStatusOut.String(), "applied at"); got != 3 {
		t.Fatalf("expected 3 applied migrations in status output, got %d:\n%s", got, finalStatusOut.String())
	}

	// A subsequent apply, with nothing left pending, is a no-op.
	noopOut := new(bytes.Buffer)
	if code := RunApply([]string{"-db", dbPath, "-dir", migrationsDir, "-yes"}, noopOut, new(bytes.Buffer)); code != 0 {
		t.Fatalf("no-op apply: exit %d", code)
	}
	if !strings.Contains(noopOut.String(), "no pending migrations") {
		t.Fatalf("expected a no-op apply to report no pending migrations, got: %s", noopOut.String())
	}
}

// TestCLI_Apply_DetectsChecksumTamper confirms a migration file edited
// after being applied is refused by dry-run apply (and would equally be
// refused by --yes, since Runner.Apply performs the same check), rather
// than silently skipped or silently reapplied.
func TestCLI_Apply_DetectsChecksumTamper(t *testing.T) {
	dir, schemaPath, migrationsDir := newProject(t)
	dbPath := filepath.Join(dir, "app.db")
	opts := baseOptions(schemaPath, migrationsDir)

	writeSchema(t, schemaPath, `CREATE TABLE users (
    id INTEGER PRIMARY KEY
) STRICT;`)
	res, err := generate(context.Background(), opts, strings.NewReader(""), &bytes.Buffer{}, &bytes.Buffer{})
	if err != nil || !res.written {
		t.Fatalf("generate: res=%+v err=%v", res, err)
	}

	if code := RunApply([]string{"-db", dbPath, "-dir", migrationsDir, "-yes"}, new(bytes.Buffer), new(bytes.Buffer)); code != 0 {
		t.Fatalf("apply --yes: exit %d", code)
	}

	tampered := readFileString(t, res.path) + "\n-- tampered\n"
	if err := os.WriteFile(res.path, []byte(tampered), 0o644); err != nil {
		t.Fatalf("tamper with migration file: %v", err)
	}

	stderr := new(bytes.Buffer)
	if code := RunApply([]string{"-db", dbPath, "-dir", migrationsDir}, new(bytes.Buffer), stderr); code == 0 {
		t.Fatalf("expected dry-run apply to refuse a tampered migration file")
	}
	if !strings.Contains(stderr.String(), "checksum mismatch") {
		t.Fatalf("expected a checksum mismatch error, got: %s", stderr.String())
	}
}

// TestCLI_Check_DetectsFileTamper confirms check catches a migration
// file's body no longer matching its recorded checksum header, without
// needing a database connection.
func TestCLI_Check_DetectsFileTamper(t *testing.T) {
	_, schemaPath, migrationsDir := newProject(t)
	opts := baseOptions(schemaPath, migrationsDir)

	writeSchema(t, schemaPath, `CREATE TABLE users (
    id INTEGER PRIMARY KEY
) STRICT;`)
	res, err := generate(context.Background(), opts, strings.NewReader(""), &bytes.Buffer{}, &bytes.Buffer{})
	if err != nil || !res.written {
		t.Fatalf("generate: res=%+v err=%v", res, err)
	}

	original := readFileString(t, res.path)
	tampered := strings.Replace(original, "CREATE TABLE", "CREATE TABLE /* tampered */", 1)
	if tampered == original {
		t.Fatalf("test setup: tamper did not change the file")
	}
	if err := os.WriteFile(res.path, []byte(tampered), 0o644); err != nil {
		t.Fatalf("tamper with migration file: %v", err)
	}

	stderr := new(bytes.Buffer)
	if code := RunCheck([]string{"-schema", schemaPath, "-dir", migrationsDir}, new(bytes.Buffer), stderr); code == 0 {
		t.Fatalf("expected check to refuse a tampered migration file")
	}
	if !strings.Contains(stderr.String(), "checksum mismatch") {
		t.Fatalf("expected a checksum mismatch error, got: %s", stderr.String())
	}
}

// TestCLI_Status_ColdDatabase confirms status against a database that
// doesn't exist yet reports every migration pending, without creating the
// file.
func TestCLI_Status_ColdDatabase(t *testing.T) {
	dir, schemaPath, migrationsDir := newProject(t)
	dbPath := filepath.Join(dir, "app.db")
	opts := baseOptions(schemaPath, migrationsDir)

	writeSchema(t, schemaPath, `CREATE TABLE users (
    id INTEGER PRIMARY KEY
) STRICT;`)
	if _, err := generate(context.Background(), opts, strings.NewReader(""), &bytes.Buffer{}, &bytes.Buffer{}); err != nil {
		t.Fatalf("generate: %v", err)
	}

	stdout := new(bytes.Buffer)
	if code := RunStatus([]string{"-db", dbPath, "-dir", migrationsDir}, stdout, new(bytes.Buffer)); code != 0 {
		t.Fatalf("status: exit %d", code)
	}
	if !strings.Contains(stdout.String(), "pending") {
		t.Fatalf("expected the migration to be reported pending, got: %s", stdout.String())
	}
	if _, err := os.Stat(dbPath); !os.IsNotExist(err) {
		t.Fatalf("status must not create the database file, stat err: %v", err)
	}
}

// TestCLI_Verify_RequiresExistingDatabase confirms verify refuses to run
// against a target database that hasn't been created yet, rather than
// silently treating an absent file as an empty schema.
func TestCLI_Verify_RequiresExistingDatabase(t *testing.T) {
	dir, _, migrationsDir := newProject(t)
	dbPath := filepath.Join(dir, "app.db")

	stderr := new(bytes.Buffer)
	if code := RunVerify([]string{"-db", dbPath, "-dir", migrationsDir}, new(bytes.Buffer), stderr); code == 0 {
		t.Fatalf("expected verify to fail against a nonexistent database")
	}
	if stderr.Len() == 0 {
		t.Fatalf("expected an error message on stderr")
	}
}

// TestCLI_LoadMigrations_ColdRepo confirms a missing migrations directory
// is treated as a cold repo (no migrations) rather than an error, matching
// generate's own readJournal behavior.
func TestCLI_LoadMigrations_ColdRepo(t *testing.T) {
	_, _, migrationsDir := newProject(t)
	migrations, err := loadMigrations(context.Background(), migrationsDir)
	if err != nil {
		t.Fatalf("loadMigrations on a missing directory: %v", err)
	}
	if len(migrations) != 0 {
		t.Fatalf("expected no migrations, got %d", len(migrations))
	}
}
