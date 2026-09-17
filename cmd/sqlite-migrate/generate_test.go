package main

// Test names reference the Testing & Verification Strategy scenario
// matrix in the spec doc where a scenario applies.

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mdg-labs/sqlite-migrate/internal/schemadiff"

	_ "modernc.org/sqlite"
)

func fixedNow() time.Time {
	return time.Date(2026, 9, 17, 12, 0, 0, 0, time.UTC)
}

func newProject(t *testing.T) (dir, schemaPath, migrationsDir string) {
	t.Helper()
	dir = t.TempDir()
	schemaPath = filepath.Join(dir, "schema.sql")
	migrationsDir = filepath.Join(dir, "migrations")
	return dir, schemaPath, migrationsDir
}

func writeSchema(t *testing.T, path, sql string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(sql), 0o644); err != nil {
		t.Fatalf("write schema: %v", err)
	}
}

func baseOptions(schemaPath, migrationsDir string) generateOptions {
	return generateOptions{
		schemaPath:    schemaPath,
		migrationsDir: migrationsDir,
		now:           fixedNow,
	}
}

// assertJournalMatchesSchema replays every migration file currently in dir
// and confirms the result is structurally identical to schema.sql — the
// same replay-based comparison verifyCandidate already performs before
// writing, re-run here as an independent end-to-end check across the
// whole accumulated journal, not just the latest migration.
func assertJournalMatchesSchema(t *testing.T, migrationsDir, schemaPath string) {
	t.Helper()
	ctx := context.Background()

	journal, err := readJournal(migrationsDir)
	if err != nil {
		t.Fatalf("readJournal: %v", err)
	}
	replayed, err := schemadiff.Parse(ctx, journal)
	if err != nil {
		t.Fatalf("replay journal: %v", err)
	}

	schemaBytes, err := os.ReadFile(schemaPath)
	if err != nil {
		t.Fatalf("read schema: %v", err)
	}
	desired, err := schemadiff.Parse(ctx, string(schemaBytes))
	if err != nil {
		t.Fatalf("parse schema.sql: %v", err)
	}

	diff := schemadiff.Diff(replayed, desired)
	if !diff.Empty() {
		t.Fatalf("journal does not reproduce schema.sql: %+v", diff)
	}
}

// TestGenerate_SequentialSchemaChanges walks a project from a cold repo
// (no existing migrations) through several sequential schema.sql changes,
// covering scenarios 01 (new table), 02 (new column, no constraints), 03
// (new column with NOT NULL DEFAULT), 04 (new column with REFERENCES), 06
// (new index), 14 (column renamed), 15 (table renamed), 12 (column
// dropped) and 13 (table dropped) — the acceptance criterion's "cold repo
// through several sequential schema.sql changes" end-to-end path.
func TestGenerate_SequentialSchemaChanges(t *testing.T) {
	_, schemaPath, migrationsDir := newProject(t)
	opts := baseOptions(schemaPath, migrationsDir)

	step := func(sql string, configure func(*generateOptions)) generateResult {
		t.Helper()
		writeSchema(t, schemaPath, sql)
		o := opts
		if configure != nil {
			configure(&o)
		}
		res, err := generate(context.Background(), o, strings.NewReader(""), &bytes.Buffer{})
		if err != nil {
			t.Fatalf("generate: %v", err)
		}
		if !res.written {
			t.Fatalf("expected a migration to be written")
		}
		assertJournalMatchesSchema(t, migrationsDir, schemaPath)
		return res
	}

	// Scenario 01: new table.
	step(`CREATE TABLE users (
    id INTEGER PRIMARY KEY,
    email TEXT
) STRICT;`, nil)

	// Scenario 02: new column, no constraints.
	users1 := step(`CREATE TABLE users (
    id INTEGER PRIMARY KEY,
    email TEXT,
    name TEXT
) STRICT;`, nil)
	if !strings.Contains(readFileString(t, users1.path), "ADD COLUMN name") {
		t.Fatalf("expected an ADD COLUMN name statement, got:\n%s", readFileString(t, users1.path))
	}

	// Scenario 03: new column with NOT NULL DEFAULT.
	step(`CREATE TABLE users (
    id INTEGER PRIMARY KEY,
    email TEXT,
    name TEXT,
    credits INTEGER NOT NULL DEFAULT 0
) STRICT;

CREATE TABLE orders (
    id INTEGER PRIMARY KEY
) STRICT;`, nil)

	// Scenario 04: new column with REFERENCES.
	ordersRef := step(`CREATE TABLE users (
    id INTEGER PRIMARY KEY,
    email TEXT,
    name TEXT,
    credits INTEGER NOT NULL DEFAULT 0
) STRICT;

CREATE TABLE orders (
    id INTEGER PRIMARY KEY,
    user_id INTEGER REFERENCES users(id)
) STRICT;`, nil)
	if !strings.Contains(readFileString(t, ordersRef.path), "REFERENCES") {
		t.Fatalf("expected a REFERENCES clause, got:\n%s", readFileString(t, ordersRef.path))
	}

	// Scenario 06: new index.
	idxSchema := `CREATE TABLE users (
    id INTEGER PRIMARY KEY,
    email TEXT,
    name TEXT,
    credits INTEGER NOT NULL DEFAULT 0
) STRICT;

CREATE TABLE orders (
    id INTEGER PRIMARY KEY,
    user_id INTEGER REFERENCES users(id)
) STRICT;

CREATE INDEX idx_orders_user_id ON orders(user_id);`
	idxResult := step(idxSchema, nil)
	if !strings.Contains(readFileString(t, idxResult.path), "CREATE INDEX idx_orders_user_id") {
		t.Fatalf("expected a direct CREATE INDEX statement, got:\n%s", readFileString(t, idxResult.path))
	}

	// Scenario 14: column renamed (type/position match), confirmed via
	// --assume-renames for a non-interactive run.
	renameColSchema := strings.Replace(idxSchema, "name TEXT,", "full_name TEXT,", 1)
	renameCol := step(renameColSchema, func(o *generateOptions) { o.assumeRenames = true })
	if !strings.Contains(readFileString(t, renameCol.path), "RENAME COLUMN") {
		t.Fatalf("expected a RENAME COLUMN statement, got:\n%s", readFileString(t, renameCol.path))
	}

	// Scenario 15: table renamed, confirmed via --assume-renames.
	renameTableSchema := strings.ReplaceAll(renameColSchema, "orders", "purchases")
	renameTable := step(renameTableSchema, func(o *generateOptions) { o.assumeRenames = true })
	if !strings.Contains(readFileString(t, renameTable.path), `RENAME TO "purchases"`) {
		t.Fatalf("expected an ALTER TABLE ... RENAME TO purchases statement, got:\n%s", readFileString(t, renameTable.path))
	}

	// Scenario 12: column dropped — refused without --allow-destructive.
	dropColSchema := `CREATE TABLE users (
    id INTEGER PRIMARY KEY,
    email TEXT,
    full_name TEXT
) STRICT;

CREATE TABLE purchases (
    id INTEGER PRIMARY KEY,
    user_id INTEGER REFERENCES users(id)
) STRICT;

CREATE INDEX idx_orders_user_id ON purchases(user_id);`
	writeSchema(t, schemaPath, dropColSchema)
	if _, err := generate(context.Background(), opts, strings.NewReader(""), &bytes.Buffer{}); err == nil {
		t.Fatalf("expected refusal for a dropped column without --allow-destructive")
	} else if !strings.Contains(err.Error(), "credits") {
		t.Fatalf("expected the refusal to name the dropped column, got: %v", err)
	}
	dropColOpts := opts
	dropColOpts.allowDestructive = true
	dropColRes, err := generate(context.Background(), dropColOpts, strings.NewReader(""), &bytes.Buffer{})
	if err != nil {
		t.Fatalf("generate with --allow-destructive: %v", err)
	}
	assertJournalMatchesSchema(t, migrationsDir, schemaPath)

	// Scenario 13: table dropped — refused without --allow-destructive.
	dropTableSchema := `CREATE TABLE users (
    id INTEGER PRIMARY KEY,
    email TEXT,
    full_name TEXT
) STRICT;`
	writeSchema(t, schemaPath, dropTableSchema)
	if _, err := generate(context.Background(), opts, strings.NewReader(""), &bytes.Buffer{}); err == nil {
		t.Fatalf("expected refusal for a dropped table without --allow-destructive")
	} else if !strings.Contains(err.Error(), "purchases") {
		t.Fatalf("expected the refusal to name the dropped table, got: %v", err)
	}
	dropTableOpts := opts
	dropTableOpts.allowDestructive = true
	dropTableRes, err := generate(context.Background(), dropTableOpts, strings.NewReader(""), &bytes.Buffer{})
	if err != nil {
		t.Fatalf("generate with --allow-destructive: %v", err)
	}
	assertJournalMatchesSchema(t, migrationsDir, schemaPath)

	if !strings.Contains(readFileString(t, dropColRes.path), "DROP") && !strings.Contains(readFileString(t, dropColRes.path), "credits") {
		t.Fatalf("expected the column-drop migration to mention the dropped column")
	}
	if !strings.Contains(readFileString(t, dropTableRes.path), `DROP TABLE "purchases"`) {
		t.Fatalf("expected DROP TABLE purchases, got:\n%s", readFileString(t, dropTableRes.path))
	}
}

func readFileString(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(b)
}

// TestGenerate_RenamePromptInteractive covers the interactive confirmation
// path itself (scripted stdin), rather than the --assume-renames shortcut
// used by the sequential test above.
func TestGenerate_RenamePromptInteractive(t *testing.T) {
	_, schemaPath, migrationsDir := newProject(t)
	opts := baseOptions(schemaPath, migrationsDir)

	writeSchema(t, schemaPath, `CREATE TABLE users (
    id INTEGER PRIMARY KEY,
    name TEXT
) STRICT;`)
	if _, err := generate(context.Background(), opts, strings.NewReader(""), &bytes.Buffer{}); err != nil {
		t.Fatalf("initial generate: %v", err)
	}

	writeSchema(t, schemaPath, `CREATE TABLE users (
    id INTEGER PRIMARY KEY,
    full_name TEXT
) STRICT;`)

	var out bytes.Buffer
	res, err := generate(context.Background(), opts, strings.NewReader("y\n"), &out)
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	if !strings.Contains(out.String(), "Rename users.name to users.full_name?") {
		t.Fatalf("expected a rename prompt, got: %q", out.String())
	}
	if !strings.Contains(readFileString(t, res.path), "RENAME COLUMN") {
		t.Fatalf("expected a RENAME COLUMN statement after confirming, got:\n%s", readFileString(t, res.path))
	}
	assertJournalMatchesSchema(t, migrationsDir, schemaPath)
}

// TestGenerate_RenameDeclinedFallsBackToDestructive covers declining a
// rename candidate: it falls back to a genuine drop+add, still gated
// behind --allow-destructive.
func TestGenerate_RenameDeclinedFallsBackToDestructive(t *testing.T) {
	_, schemaPath, migrationsDir := newProject(t)
	opts := baseOptions(schemaPath, migrationsDir)

	writeSchema(t, schemaPath, `CREATE TABLE users (
    id INTEGER PRIMARY KEY,
    name TEXT
) STRICT;`)
	if _, err := generate(context.Background(), opts, strings.NewReader(""), &bytes.Buffer{}); err != nil {
		t.Fatalf("initial generate: %v", err)
	}

	writeSchema(t, schemaPath, `CREATE TABLE users (
    id INTEGER PRIMARY KEY,
    full_name TEXT
) STRICT;`)

	var out bytes.Buffer
	_, err := generate(context.Background(), opts, strings.NewReader("n\n"), &out)
	if err == nil {
		t.Fatalf("expected refusal after declining the rename")
	}
	if !strings.Contains(err.Error(), "name") {
		t.Fatalf("expected the refusal to name the dropped column, got: %v", err)
	}
}

// TestGenerate_NotStrictRefused covers scenario 17: a schema.sql table
// missing STRICT is refused with a clear error, and nothing is written.
func TestGenerate_NotStrictRefused(t *testing.T) {
	dir, schemaPath, migrationsDir := newProject(t)
	opts := baseOptions(schemaPath, migrationsDir)

	writeSchema(t, schemaPath, `CREATE TABLE users (
    id INTEGER PRIMARY KEY,
    name TEXT
);`)

	if _, err := generate(context.Background(), opts, strings.NewReader(""), &bytes.Buffer{}); err == nil {
		t.Fatalf("expected refusal for a non-STRICT table")
	} else if !strings.Contains(err.Error(), "STRICT") {
		t.Fatalf("expected the error to mention STRICT, got: %v", err)
	}

	entries, err := os.ReadDir(migrationsDir)
	if err == nil && len(entries) != 0 {
		t.Fatalf("expected no migration files to be written, found %v", entries)
	}
	_ = dir
}

// TestGenerate_NoChanges covers an unchanged schema.sql: nothing is
// written and no error is returned.
func TestGenerate_NoChanges(t *testing.T) {
	_, schemaPath, migrationsDir := newProject(t)
	opts := baseOptions(schemaPath, migrationsDir)

	writeSchema(t, schemaPath, `CREATE TABLE users (id INTEGER PRIMARY KEY) STRICT;`)
	if _, err := generate(context.Background(), opts, strings.NewReader(""), &bytes.Buffer{}); err != nil {
		t.Fatalf("initial generate: %v", err)
	}

	res, err := generate(context.Background(), opts, strings.NewReader(""), &bytes.Buffer{})
	if err != nil {
		t.Fatalf("generate with no changes: %v", err)
	}
	if res.written {
		t.Fatalf("expected nothing to be written for an unchanged schema")
	}
}

// TestGenerate_ReferencesWithNonNullDefault covers scenario 05: SQLite
// only refuses a REFERENCES column with a non-NULL default once the table
// already holds rows and foreign_keys enforcement is on — a state a
// schema-only replay never reaches, and one Runner.Apply (Phase 5) never
// reaches either, since it suspends foreign_keys for the whole migration
// transaction. generate has no live data to check against (by design —
// see "Constraint tightening failing loudly at apply time" in the spec
// doc), so it hands back the same valid-looking statement sqldefwrap
// already produces; SQLite is the one that would refuse it, at the point
// real data and enforcement coincide.
func TestGenerate_ReferencesWithNonNullDefault(t *testing.T) {
	_, schemaPath, migrationsDir := newProject(t)
	opts := baseOptions(schemaPath, migrationsDir)

	writeSchema(t, schemaPath, `CREATE TABLE users (id INTEGER PRIMARY KEY) STRICT;
CREATE TABLE orders (id INTEGER PRIMARY KEY) STRICT;`)
	if _, err := generate(context.Background(), opts, strings.NewReader(""), &bytes.Buffer{}); err != nil {
		t.Fatalf("initial generate: %v", err)
	}

	writeSchema(t, schemaPath, `CREATE TABLE users (id INTEGER PRIMARY KEY) STRICT;
CREATE TABLE orders (id INTEGER PRIMARY KEY, user_id INTEGER NOT NULL DEFAULT 0 REFERENCES users(id)) STRICT;`)

	res, err := generate(context.Background(), opts, strings.NewReader(""), &bytes.Buffer{})
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	if !res.written {
		t.Fatalf("expected the migration to be written")
	}
	assertJournalMatchesSchema(t, migrationsDir, schemaPath)
}

// TestGenerate_TestdataScenarios runs every rebuild-flavored testdata
// scenario (07-11, 16, 18) through the full generate pipeline: seed the
// migrations directory with the scenario's "before" schema as if it were
// already applied, point schema.sql at "after", and confirm generate
// writes a migration that reproduces "after" exactly.
func TestGenerate_TestdataScenarios(t *testing.T) {
	dirs, err := filepath.Glob("../../testdata/schemas/*")
	if err != nil {
		t.Fatalf("glob scenario dirs: %v", err)
	}
	found := 0
	for _, scenarioDir := range dirs {
		beforePath := filepath.Join(scenarioDir, "before.sql")
		if _, err := os.Stat(beforePath); err != nil {
			continue
		}
		found++
		name := filepath.Base(scenarioDir)
		t.Run(name, func(t *testing.T) {
			before := readFileString(t, beforePath)
			after := readFileString(t, filepath.Join(scenarioDir, "after.sql"))

			_, schemaPath, migrationsDir := newProject(t)
			if err := os.MkdirAll(migrationsDir, 0o755); err != nil {
				t.Fatalf("mkdir migrations: %v", err)
			}
			seedPath := filepath.Join(migrationsDir, "20260101000000_seed.sql")
			if err := os.WriteFile(seedPath, []byte(before), 0o644); err != nil {
				t.Fatalf("seed migration: %v", err)
			}
			writeSchema(t, schemaPath, after)

			opts := baseOptions(schemaPath, migrationsDir)
			res, err := generate(context.Background(), opts, strings.NewReader(""), &bytes.Buffer{})
			if err != nil {
				t.Fatalf("generate: %v", err)
			}
			if !res.written {
				t.Fatalf("expected a migration to be written")
			}
			assertJournalMatchesSchema(t, migrationsDir, schemaPath)
		})
	}
	if found == 0 {
		t.Fatal("no scenario directories found under testdata/schemas")
	}
}
