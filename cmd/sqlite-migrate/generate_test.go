package main

// Test names reference the Testing & Verification Strategy scenario
// matrix in the spec doc where a scenario applies.

import (
	"bytes"
	"context"
	"errors"
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mdg-labs/sqlite-migrate/internal/rename"
	"github.com/mdg-labs/sqlite-migrate/internal/schemadiff"

	_ "modernc.org/sqlite"
)

var update = flag.Bool("update", false, "update golden files")

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

	journal, err := readJournal(ctx, migrationsDir)
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
		var stderr bytes.Buffer
		res, err := generate(context.Background(), o, strings.NewReader(""), &bytes.Buffer{}, &stderr)
		if err != nil {
			t.Fatalf("generate: %v", err)
		}
		if !res.written {
			t.Fatalf("expected a migration to be written")
		}
		if stderr.Len() != 0 {
			t.Fatalf("expected no stderr note, got: %q", stderr.String())
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
	if _, err := generate(context.Background(), opts, strings.NewReader(""), &bytes.Buffer{}, &bytes.Buffer{}); err == nil {
		t.Fatalf("expected refusal for a dropped column without --allow-destructive")
	} else if !strings.Contains(err.Error(), "credits") {
		t.Fatalf("expected the refusal to name the dropped column, got: %v", err)
	}
	dropColOpts := opts
	dropColOpts.allowDestructive = true
	dropColRes, err := generate(context.Background(), dropColOpts, strings.NewReader(""), &bytes.Buffer{}, &bytes.Buffer{})
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
	if _, err := generate(context.Background(), opts, strings.NewReader(""), &bytes.Buffer{}, &bytes.Buffer{}); err == nil {
		t.Fatalf("expected refusal for a dropped table without --allow-destructive")
	} else if !strings.Contains(err.Error(), "purchases") {
		t.Fatalf("expected the refusal to name the dropped table, got: %v", err)
	}
	dropTableOpts := opts
	dropTableOpts.allowDestructive = true
	dropTableRes, err := generate(context.Background(), dropTableOpts, strings.NewReader(""), &bytes.Buffer{}, &bytes.Buffer{})
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

// TestGenerate_ColumnAddedToTableWithAppliedKeywordNamedColumn covers issue
// #45's replay case: the unquoted "serial" column doesn't come from a
// fresh schema.sql edit this run, but from a migration already written and
// applied by an earlier generate — the shape schemadiff.Table.SQL (and so
// sqldefwrap.Diff's currentDDL) always takes it in, replayed straight from
// sqlite_master rather than re-parsed from schema.sql. A later, unrelated
// additive change to that same table must still succeed.
func TestGenerate_ColumnAddedToTableWithAppliedKeywordNamedColumn(t *testing.T) {
	_, schemaPath, migrationsDir := newProject(t)
	opts := baseOptions(schemaPath, migrationsDir)

	writeSchema(t, schemaPath, `CREATE TABLE disks (
    id INTEGER PRIMARY KEY,
    serial TEXT
) STRICT;`)
	if _, err := generate(context.Background(), opts, strings.NewReader(""), &bytes.Buffer{}, &bytes.Buffer{}); err != nil {
		t.Fatalf("initial generate: %v", err)
	}

	writeSchema(t, schemaPath, `CREATE TABLE disks (
    id INTEGER PRIMARY KEY,
    serial TEXT,
    capacity_gb INTEGER
) STRICT;`)

	res, err := generate(context.Background(), opts, strings.NewReader(""), &bytes.Buffer{}, &bytes.Buffer{})
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	if !res.written {
		t.Fatalf("expected a migration to be written")
	}
	if !strings.Contains(readFileString(t, res.path), "ADD COLUMN capacity_gb") {
		t.Fatalf("expected a direct ADD COLUMN capacity_gb statement, got:\n%s", readFileString(t, res.path))
	}
	assertJournalMatchesSchema(t, migrationsDir, schemaPath)
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
	if _, err := generate(context.Background(), opts, strings.NewReader(""), &bytes.Buffer{}, &bytes.Buffer{}); err != nil {
		t.Fatalf("initial generate: %v", err)
	}

	writeSchema(t, schemaPath, `CREATE TABLE users (
    id INTEGER PRIMARY KEY,
    full_name TEXT
) STRICT;`)

	var out bytes.Buffer
	res, err := generate(context.Background(), opts, strings.NewReader("y\n"), &out, &bytes.Buffer{})
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
	if _, err := generate(context.Background(), opts, strings.NewReader(""), &bytes.Buffer{}, &bytes.Buffer{}); err != nil {
		t.Fatalf("initial generate: %v", err)
	}

	writeSchema(t, schemaPath, `CREATE TABLE users (
    id INTEGER PRIMARY KEY,
    full_name TEXT
) STRICT;`)

	var out bytes.Buffer
	_, err := generate(context.Background(), opts, strings.NewReader("n\n"), &out, &bytes.Buffer{})
	if err == nil {
		t.Fatalf("expected refusal after declining the rename")
	}
	if !strings.Contains(err.Error(), "name") {
		t.Fatalf("expected the refusal to name the dropped column, got: %v", err)
	}
}

// TestGenerate_NoChanges covers an unchanged schema.sql: nothing is
// written and no error is returned.
func TestGenerate_NoChanges(t *testing.T) {
	_, schemaPath, migrationsDir := newProject(t)
	opts := baseOptions(schemaPath, migrationsDir)

	writeSchema(t, schemaPath, `CREATE TABLE users (id INTEGER PRIMARY KEY) STRICT;`)
	if _, err := generate(context.Background(), opts, strings.NewReader(""), &bytes.Buffer{}, &bytes.Buffer{}); err != nil {
		t.Fatalf("initial generate: %v", err)
	}

	res, err := generate(context.Background(), opts, strings.NewReader(""), &bytes.Buffer{}, &bytes.Buffer{})
	if err != nil {
		t.Fatalf("generate with no changes: %v", err)
	}
	if res.written {
		t.Fatalf("expected nothing to be written for an unchanged schema")
	}
}

// TestGenerate_TestdataScenarios runs every rebuild-flavored testdata
// scenario (07-11, 16, 18) through the full generate pipeline: seed the
// migrations directory with the scenario's "before" schema as if it were
// already applied, point schema.sql at "after", and confirm generate
// writes a migration that reproduces "after" exactly. None of these add a
// column alongside their rebuild-worthy change, so needsRebuild's earlier
// structural checks decide the rebuild directly and buildMigrationBody
// must print no additiveChangeReproducesAfter stderr note for them.
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
			var stderr bytes.Buffer
			res, err := generate(context.Background(), opts, strings.NewReader(""), &bytes.Buffer{}, &stderr)
			if err != nil {
				t.Fatalf("generate: %v", err)
			}
			if !res.written {
				t.Fatalf("expected a migration to be written")
			}
			if stderr.Len() != 0 {
				t.Fatalf("expected no stderr note for a structural rebuild, got: %q", stderr.String())
			}
			assertJournalMatchesSchema(t, migrationsDir, schemaPath)
		})
	}
	if found == 0 {
		t.Fatal("no scenario directories found under testdata/schemas")
	}
}

// TestGenerate_CompoundTableAndColumnRename covers a table rename and a
// rename of one of that table's own columns made in the same schema.sql
// edit: at raw-diff time the column lives inside the whole dropped/added
// table pair, never in a ChangedTables entry rename.Detect could see, so
// without re-resolving after the table rename is applied this used to be
// misclassified destructive and silently drop the renamed column's data
// under --allow-destructive.
func TestGenerate_CompoundTableAndColumnRename(t *testing.T) {
	_, schemaPath, migrationsDir := newProject(t)
	opts := baseOptions(schemaPath, migrationsDir)

	writeSchema(t, schemaPath, `CREATE TABLE orders (
    id INTEGER PRIMARY KEY,
    qty INTEGER
) STRICT;`)
	if _, err := generate(context.Background(), opts, strings.NewReader(""), &bytes.Buffer{}, &bytes.Buffer{}); err != nil {
		t.Fatalf("initial generate: %v", err)
	}

	writeSchema(t, schemaPath, `CREATE TABLE purchases (
    id INTEGER PRIMARY KEY,
    amount INTEGER
) STRICT;`)

	// Confirm the table rename (orders -> purchases), then the column
	// rename it reveals (qty -> amount).
	res, err := generate(context.Background(), opts, strings.NewReader("y\ny\n"), &bytes.Buffer{}, &bytes.Buffer{})
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	if !res.written {
		t.Fatalf("expected a migration to be written")
	}
	body := readFileString(t, res.path)
	if !strings.Contains(body, "RENAME TO") || !strings.Contains(body, "RENAME COLUMN") {
		t.Fatalf("expected both a table and column rename statement, got:\n%s", body)
	}
	if strings.Contains(body, "_sqlite_migrate_new") {
		t.Fatalf("expected a plain rename, not a rebuild that would drop data, got:\n%s", body)
	}
	assertJournalMatchesSchema(t, migrationsDir, schemaPath)
}

// TestGenerate_NewTableWithForeignKeyColumnAddedTogether covers adding a
// brand-new table and, in the same schema.sql edit, a new column on an
// existing table that references it: addColumnStatements used to scope
// the new table into its sqldef call regardless, so sqldef emitted its
// CREATE TABLE a second time on top of buildMigrationBody's own
// AddedTables statement, and generate failed with "table ... already
// exists" for an entirely valid schema change.
func TestGenerate_NewTableWithForeignKeyColumnAddedTogether(t *testing.T) {
	_, schemaPath, migrationsDir := newProject(t)
	opts := baseOptions(schemaPath, migrationsDir)

	writeSchema(t, schemaPath, `CREATE TABLE orders (
    id INTEGER PRIMARY KEY
) STRICT;`)
	if _, err := generate(context.Background(), opts, strings.NewReader(""), &bytes.Buffer{}, &bytes.Buffer{}); err != nil {
		t.Fatalf("initial generate: %v", err)
	}

	writeSchema(t, schemaPath, `CREATE TABLE orders (
    id INTEGER PRIMARY KEY,
    region_id INTEGER REFERENCES regions(id)
) STRICT;

CREATE TABLE regions (
    id INTEGER PRIMARY KEY
) STRICT;`)

	res, err := generate(context.Background(), opts, strings.NewReader(""), &bytes.Buffer{}, &bytes.Buffer{})
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	if !res.written {
		t.Fatalf("expected a migration to be written")
	}
	assertJournalMatchesSchema(t, migrationsDir, schemaPath)
}

// TestGenerate_AddedColumnWithCheckConstraintChange covers adding a new
// column to a table while also tightening an existing CHECK constraint on
// that same table in the same schema.sql edit: needsRebuild used to
// assume any added column fully explained the table's CREATE TABLE text
// change, routing this straight to sqldefwrap (ADD COLUMN only) and
// silently dropping the CHECK edit, which then failed the drift check. It
// also covers issue #52's stderr note: additiveChangeReproducesAfter
// really does find a residual difference here (the tightened CHECK), so
// the rebuild it falls back to must be announced on stderr, naming the
// table.
func TestGenerate_AddedColumnWithCheckConstraintChange(t *testing.T) {
	_, schemaPath, migrationsDir := newProject(t)
	opts := baseOptions(schemaPath, migrationsDir)

	writeSchema(t, schemaPath, `CREATE TABLE users (
    id INTEGER PRIMARY KEY,
    age INTEGER
) STRICT;`)
	if _, err := generate(context.Background(), opts, strings.NewReader(""), &bytes.Buffer{}, &bytes.Buffer{}); err != nil {
		t.Fatalf("initial generate: %v", err)
	}

	writeSchema(t, schemaPath, `CREATE TABLE users (
    id INTEGER PRIMARY KEY,
    age INTEGER CHECK (age >= 0),
    name TEXT
) STRICT;`)

	var stderr bytes.Buffer
	res, err := generate(context.Background(), opts, strings.NewReader(""), &bytes.Buffer{}, &stderr)
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	if !res.written {
		t.Fatalf("expected a migration to be written")
	}
	if !strings.Contains(readFileString(t, res.path), "_sqlite_migrate_new") {
		t.Fatalf("expected a full rebuild (the CHECK change isn't expressible as ADD COLUMN), got:\n%s", readFileString(t, res.path))
	}
	if !strings.Contains(stderr.String(), `generate: note: rebuilding "users" instead of ADD COLUMN:`) {
		t.Fatalf("expected a stderr note naming the rebuilt table, got: %q", stderr.String())
	}
	assertJournalMatchesSchema(t, migrationsDir, schemaPath)
}

// TestGenerate_ColumnAddedToIndexedTableRebuiltByEarlierMigration covers
// scenario 23's replayed-journal form: a table with an explicit CREATE
// INDEX that was already rebuilt once (a tightened CHECK, applied in an
// earlier migration), then gets a further nullable column added. The
// index fix in additiveChangeReproducesAfter must still recognize this as
// a plain additive change, not fall back to a second rebuild just because
// the table carries an index.
func TestGenerate_ColumnAddedToIndexedTableRebuiltByEarlierMigration(t *testing.T) {
	_, schemaPath, migrationsDir := newProject(t)
	opts := baseOptions(schemaPath, migrationsDir)

	writeSchema(t, schemaPath, `CREATE TABLE widgets (
    id INTEGER PRIMARY KEY,
    status TEXT NOT NULL
) STRICT;
CREATE INDEX widgets_status_idx ON widgets (status);`)
	if _, err := generate(context.Background(), opts, strings.NewReader(""), &bytes.Buffer{}, &bytes.Buffer{}); err != nil {
		t.Fatalf("initial generate: %v", err)
	}

	// Tighten the CHECK: not expressible as ALTER TABLE, so this is a
	// genuine rebuild — the journal now holds a rebuilt CREATE TABLE for
	// widgets alongside its still-untouched explicit index.
	writeSchema(t, schemaPath, `CREATE TABLE widgets (
    id INTEGER PRIMARY KEY,
    status TEXT NOT NULL CHECK (status IN ('open', 'closed'))
) STRICT;
CREATE INDEX widgets_status_idx ON widgets (status);`)
	rebuiltRes, err := generate(context.Background(), opts, strings.NewReader(""), &bytes.Buffer{}, &bytes.Buffer{})
	if err != nil {
		t.Fatalf("generate (rebuild): %v", err)
	}
	if !strings.Contains(readFileString(t, rebuiltRes.path), "_sqlite_migrate_new") {
		t.Fatalf("expected the CHECK tightening to produce a rebuild, got:\n%s", readFileString(t, rebuiltRes.path))
	}

	// Now add a plain nullable column on top of the rebuilt, indexed table.
	writeSchema(t, schemaPath, `CREATE TABLE widgets (
    id INTEGER PRIMARY KEY,
    status TEXT NOT NULL CHECK (status IN ('open', 'closed')),
    notes TEXT
) STRICT;
CREATE INDEX widgets_status_idx ON widgets (status);`)
	res, err := generate(context.Background(), opts, strings.NewReader(""), &bytes.Buffer{}, &bytes.Buffer{})
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	if !res.written {
		t.Fatalf("expected a migration to be written")
	}
	body := readFileString(t, res.path)
	if !strings.Contains(body, "ADD COLUMN notes") {
		t.Fatalf("expected a direct ADD COLUMN notes statement, got:\n%s", body)
	}
	if strings.Contains(body, "_sqlite_migrate_new") {
		t.Fatalf("expected a plain ADD COLUMN, not a rebuild, got:\n%s", body)
	}
	assertJournalMatchesSchema(t, migrationsDir, schemaPath)
}

// TestGenerate_ColumnAddedToIndexedTableWithCheckChangeStillRebuilds
// covers scenario 08's shape (a tightened CHECK) plus an explicit index
// plus a new column, all in the same schema.sql edit: the index fix must
// not turn this genuine rebuild into an incorrect ADD COLUMN.
func TestGenerate_ColumnAddedToIndexedTableWithCheckChangeStillRebuilds(t *testing.T) {
	_, schemaPath, migrationsDir := newProject(t)
	opts := baseOptions(schemaPath, migrationsDir)

	writeSchema(t, schemaPath, `CREATE TABLE widgets (
    id INTEGER PRIMARY KEY,
    status TEXT
) STRICT;
CREATE INDEX widgets_status_idx ON widgets (status);`)
	if _, err := generate(context.Background(), opts, strings.NewReader(""), &bytes.Buffer{}, &bytes.Buffer{}); err != nil {
		t.Fatalf("initial generate: %v", err)
	}

	writeSchema(t, schemaPath, `CREATE TABLE widgets (
    id INTEGER PRIMARY KEY,
    status TEXT CHECK (status IN ('open', 'closed')),
    notes TEXT
) STRICT;
CREATE INDEX widgets_status_idx ON widgets (status);`)

	var stderr bytes.Buffer
	res, err := generate(context.Background(), opts, strings.NewReader(""), &bytes.Buffer{}, &stderr)
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	if !res.written {
		t.Fatalf("expected a migration to be written")
	}
	body := readFileString(t, res.path)
	if !strings.Contains(body, "_sqlite_migrate_new") {
		t.Fatalf("expected a full rebuild (the CHECK change isn't expressible as ADD COLUMN), got:\n%s", body)
	}
	if strings.Contains(body, "ADD COLUMN notes") {
		t.Fatalf("expected no direct ADD COLUMN, got:\n%s", body)
	}
	if !strings.Contains(stderr.String(), `generate: note: rebuilding "widgets" instead of ADD COLUMN:`) {
		t.Fatalf("expected a stderr note naming the rebuilt table, got: %q", stderr.String())
	}
	assertJournalMatchesSchema(t, migrationsDir, schemaPath)
}

// TestGenerate_ConflictingAssumeRenameFlags covers passing both
// --assume-renames and --assume-no-renames when the diff has no rename
// candidates at all (e.g. a plain added column): the conflict used to be
// enforced only inside rename.Confirm's per-candidate loop, so it was
// silently accepted whenever that loop never ran.
func TestGenerate_ConflictingAssumeRenameFlags(t *testing.T) {
	_, schemaPath, migrationsDir := newProject(t)
	opts := baseOptions(schemaPath, migrationsDir)
	opts.assumeRenames = true
	opts.assumeNoRenames = true

	writeSchema(t, schemaPath, `CREATE TABLE users (id INTEGER PRIMARY KEY) STRICT;`)

	_, err := generate(context.Background(), opts, strings.NewReader(""), &bytes.Buffer{}, &bytes.Buffer{})
	if !errors.Is(err, rename.ErrConflictingAssumeFlags) {
		t.Fatalf("expected ErrConflictingAssumeFlags, got: %v", err)
	}
}

// goldenAssumeRenames names the scenarios whose expected outcome (matrix
// rows 14/15: "prompts; on confirm, RENAME COLUMN"/"RENAME TO") needs
// --assume-renames to reach that confirmed-rename branch non-interactively.
var goldenAssumeRenames = map[string]bool{
	"14_column_renamed": true,
	"15_table_renamed":  true,
}

// goldenRefused names the scenarios whose expected outcome per the spec
// doc's scenario matrix is generate refusing outright (12/13: destructive
// without --allow-destructive; 17: STRICT missing) — for these, the
// "golden" file under testdata/golden/generate records the exact expected
// error message rather than SQL.
//
// Scenario 05 (REFERENCES + non-NULL default) is deliberately absent from
// this map even though the matrix once listed it as "refused outright":
// SQLite only rejects that combination once the table already holds rows
// and foreign_keys enforcement is on, a state neither this schema-only
// replay nor Runner.Apply (which suspends foreign_keys for the whole
// migration transaction) ever reaches, so generate hands back the same
// valid-looking ADD COLUMN statement sqldefwrap produces for any other
// added column.
var goldenRefused = map[string]bool{
	"12_column_dropped":     true,
	"13_table_dropped":      true,
	"17_not_strict_refused": true,
}

// TestGolden runs generate's full pipeline against every scenario fixture
// under testdata/schemas/generate — the direct/destructive/rename/STRICT-
// refusal scenarios from the spec doc's scenario matrix that the generate
// command itself routes (01-06, 12-15, 17), as opposed to the matrix's
// full-rebuild scenarios (07-11, 16, 18) directly under testdata/schemas,
// which are internal/rebuild's own golden-file scenarios and stay under
// its own TestGolden. Each fixture's expected outcome is recorded
// byte-for-byte in testdata/golden/generate/<name>.sql: the generated
// migration body for a scenario generate is expected to write, or the
// exact refusal error message for one it's expected to refuse.
func TestGolden(t *testing.T) {
	dirs, err := filepath.Glob("../../testdata/schemas/generate/*")
	if err != nil {
		t.Fatalf("glob scenario dirs: %v", err)
	}
	found := 0
	for _, scenarioDir := range dirs {
		beforePath := filepath.Join(scenarioDir, "before.sql")
		if _, err := os.Stat(beforePath); err != nil {
			continue // e.g. testdata/schemas/generate/.gitkeep, not a scenario directory
		}
		found++
		name := filepath.Base(scenarioDir)
		t.Run(name, func(t *testing.T) {
			before := readFileString(t, beforePath)
			after := readFileString(t, filepath.Join(scenarioDir, "after.sql"))

			_, schemaPath, migrationsDir := newProject(t)
			if strings.TrimSpace(before) != "" {
				if err := os.MkdirAll(migrationsDir, 0o755); err != nil {
					t.Fatalf("mkdir migrations: %v", err)
				}
				seedPath := filepath.Join(migrationsDir, "20260101000000_seed.sql")
				if err := os.WriteFile(seedPath, []byte(before), 0o644); err != nil {
					t.Fatalf("seed migration: %v", err)
				}
			}
			writeSchema(t, schemaPath, after)

			opts := baseOptions(schemaPath, migrationsDir)
			opts.assumeRenames = goldenAssumeRenames[name]

			entriesBefore, err := os.ReadDir(migrationsDir)
			if err != nil && !os.IsNotExist(err) {
				t.Fatalf("read migrations dir: %v", err)
			}

			goldenPath := filepath.Join("..", "..", "testdata", "golden", "generate", name+".sql")
			res, genErr := generate(context.Background(), opts, strings.NewReader(""), &bytes.Buffer{}, &bytes.Buffer{})

			if goldenRefused[name] {
				if genErr == nil {
					t.Fatalf("expected generate to refuse scenario %s", name)
				}
				entriesAfter, err := os.ReadDir(migrationsDir)
				if err != nil && !os.IsNotExist(err) {
					t.Fatalf("read migrations dir: %v", err)
				}
				if len(entriesAfter) != len(entriesBefore) {
					t.Fatalf("expected no migration file to be written on refusal, found %v", entriesAfter)
				}
				// The parse-failure error (scenario 17) embeds schemaPath, an
				// absolute t.TempDir() path unique to this run — normalize it
				// to a stable placeholder before recording/comparing.
				msg := strings.ReplaceAll(genErr.Error(), schemaPath, "schema.sql")
				compareOrUpdateGolden(t, goldenPath, msg+"\n")
				return
			}

			if genErr != nil {
				t.Fatalf("generate: %v", genErr)
			}
			if !res.written {
				t.Fatalf("expected a migration to be written")
			}
			assertJournalMatchesSchema(t, migrationsDir, schemaPath)

			body := checksumHeaderPattern.ReplaceAllString(readFileString(t, res.path), "")
			compareOrUpdateGolden(t, goldenPath, body)
		})
	}
	if found == 0 {
		t.Fatal("no scenario directories found under testdata/schemas/generate")
	}
}

func compareOrUpdateGolden(t *testing.T, path, got string) {
	t.Helper()
	if *update {
		if err := os.WriteFile(path, []byte(got), 0o644); err != nil {
			t.Fatalf("write golden file: %v", err)
		}
		return
	}

	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read golden file %s: %v (run `make golden-update` first)", path, err)
	}
	if got != string(want) {
		t.Errorf("generate output doesn't match golden file %s\n--- got ---\n%s\n--- want ---\n%s", path, got, string(want))
	}
}
