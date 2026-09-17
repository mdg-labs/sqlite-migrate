package rebuild

// Scenario numbers in test names refer to the Testing & Verification
// Strategy scenario matrix in the spec doc.

import (
	"context"
	"database/sql"
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mdg-labs/sqlite-migrate/internal/schemadiff"

	_ "modernc.org/sqlite"
)

var update = flag.Bool("update", false, "update golden files")

// rebuildSelection names, for scenarios where not every changed table needs
// a full rebuild, which ones do — mirroring the sqldefwrap-vs-rebuild
// routing decision Phase 6 will eventually make. A scenario absent from
// this map sends every changed table to Generate.
var rebuildSelection = map[string][]string{
	// orders only gains a column here (sqldefwrap's job); users' type
	// change is the half this package owns.
	"16_two_unrelated_changes": {"users"},
}

func mustParse(t *testing.T, ddl string) *schemadiff.Schema {
	t.Helper()
	s, err := schemadiff.Parse(context.Background(), ddl)
	if err != nil {
		t.Fatalf("Parse failed: %v\nDDL:\n%s", err, ddl)
	}
	return s
}

// inlineDiffs builds TableDiffs directly from inline before/after SQL
// rather than a testdata scenario directory, for execute tests covering a
// specific finding rather than a numbered scenario.
func inlineDiffs(t *testing.T, beforeSQL, afterSQL string, tableNames ...string) []schemadiff.TableDiff {
	t.Helper()
	before := mustParse(t, beforeSQL)
	after := mustParse(t, afterSQL)
	d := schemadiff.Diff(before, after)

	diffs := make([]schemadiff.TableDiff, 0, len(tableNames))
	for _, tn := range tableNames {
		diffs = append(diffs, findTableDiff(t, d, tn))
	}
	return diffs
}

func findTableDiff(t *testing.T, d *schemadiff.SchemaDiff, name string) schemadiff.TableDiff {
	t.Helper()
	for _, td := range d.ChangedTables {
		if td.Name == name {
			return td
		}
	}
	t.Fatalf("no TableDiff for %q in %+v", name, d.ChangedTables)
	return schemadiff.TableDiff{}
}

func scenarioDiffs(t *testing.T, name, beforeSQL, afterSQL string) (*schemadiff.SchemaDiff, []schemadiff.TableDiff) {
	t.Helper()
	before := mustParse(t, beforeSQL)
	after := mustParse(t, afterSQL)
	d := schemadiff.Diff(before, after)

	if want, ok := rebuildSelection[name]; ok {
		diffs := make([]schemadiff.TableDiff, 0, len(want))
		for _, tn := range want {
			diffs = append(diffs, findTableDiff(t, d, tn))
		}
		return d, diffs
	}
	return d, d.ChangedTables
}

func TestGolden(t *testing.T) {
	dirs, err := filepath.Glob("../../testdata/schemas/*")
	if err != nil {
		t.Fatalf("glob scenario dirs: %v", err)
	}
	if len(dirs) == 0 {
		t.Fatal("no scenario directories found under testdata/schemas")
	}

	for _, dir := range dirs {
		if _, err := os.Stat(filepath.Join(dir, "before.sql")); err != nil {
			continue // e.g. testdata/schemas/.gitkeep, not a scenario directory
		}
		name := filepath.Base(dir)
		t.Run(name, func(t *testing.T) {
			beforeSQL := readFile(t, filepath.Join(dir, "before.sql"))
			afterSQL := readFile(t, filepath.Join(dir, "after.sql"))

			_, diffs := scenarioDiffs(t, name, beforeSQL, afterSQL)
			if len(diffs) == 0 {
				t.Fatalf("scenario %s: no table diffs selected for rebuild", name)
			}

			got, err := Generate(context.Background(), beforeSQL, afterSQL, diffs)
			if err != nil {
				t.Fatalf("Generate: %v", err)
			}

			goldenPath := filepath.Join("..", "..", "testdata", "golden", name+".sql")
			if *update {
				if err := os.WriteFile(goldenPath, []byte(got), 0o644); err != nil {
					t.Fatalf("write golden file: %v", err)
				}
				return
			}

			wantBytes, err := os.ReadFile(goldenPath)
			if err != nil {
				t.Fatalf("read golden file %s: %v (run `make golden-update` first)", goldenPath, err)
			}
			if got != string(wantBytes) {
				t.Errorf("generated SQL doesn't match golden file %s\n--- got ---\n%s\n--- want ---\n%s", goldenPath, got, string(wantBytes))
			}
		})
	}
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(b)
}

// applyLikeRunner executes stmts against db mirroring the runtime's apply
// model (Runner.Apply, Phase 5): PRAGMA foreign_keys off outside the
// transaction, everything else inside one transaction, PRAGMA
// foreign_key_check and PRAGMA integrity_check before COMMIT — never a
// per-statement FK check, since that's precisely what makes a genuine FK
// cycle between two rebuilt tables resolvable at all.
func applyLikeRunner(t *testing.T, db *sql.DB, stmts []string) {
	t.Helper()

	if _, err := db.Exec(`PRAGMA foreign_keys = OFF`); err != nil {
		t.Fatalf("disable foreign_keys: %v", err)
	}

	tx, err := db.Begin()
	if err != nil {
		t.Fatalf("begin transaction: %v", err)
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback()
		}
	}()

	for _, stmt := range stmts {
		if _, err := tx.Exec(stmt); err != nil {
			t.Fatalf("exec statement failed: %v\nstatement:\n%s", err, stmt)
		}
	}

	rows, err := tx.Query(`PRAGMA foreign_key_check`)
	if err != nil {
		t.Fatalf("foreign_key_check: %v", err)
	}
	var violations []string
	for rows.Next() {
		violations = append(violations, "violation row")
	}
	_ = rows.Close()
	if err := rows.Err(); err != nil {
		t.Fatalf("foreign_key_check: %v", err)
	}
	if len(violations) > 0 {
		t.Fatalf("foreign_key_check found %d violation(s)", len(violations))
	}

	var integrityResult string
	if err := tx.QueryRow(`PRAGMA integrity_check`).Scan(&integrityResult); err != nil {
		t.Fatalf("integrity_check: %v", err)
	}
	if integrityResult != "ok" {
		t.Fatalf("integrity_check: want ok, got %q", integrityResult)
	}

	if err := tx.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}
	committed = true

	if _, err := db.Exec(`PRAGMA foreign_keys = ON`); err != nil {
		t.Fatalf("re-enable foreign_keys: %v", err)
	}
}

func openSeededDB(t *testing.T, schemaSQL, seedSQL string) *sql.DB {
	t.Helper()
	path := filepath.Join(t.TempDir(), "seed.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("open %s: %v", path, err)
	}
	t.Cleanup(func() { _ = db.Close() })
	db.SetMaxOpenConns(1)

	if _, err := db.Exec(schemaSQL); err != nil {
		t.Fatalf("replay schema: %v", err)
	}
	if seedSQL != "" {
		if _, err := db.Exec(seedSQL); err != nil {
			t.Fatalf("seed rows: %v", err)
		}
	}
	return db
}

// TestExecute_07_TypeChangePreservesData proves the rebuild's INSERT...
// SELECT column mapping actually carries real rows across a type change,
// not just an empty schema.
func TestExecute_07_TypeChangePreservesData(t *testing.T) {
	beforeSQL := readFile(t, "../../testdata/schemas/07_type_change/before.sql")
	afterSQL := readFile(t, "../../testdata/schemas/07_type_change/after.sql")
	_, diffs := scenarioDiffs(t, "07_type_change", beforeSQL, afterSQL)

	db := openSeededDB(t, beforeSQL, `
		INSERT INTO users (id, age) VALUES (1, '30');
		INSERT INTO users (id, age) VALUES (2, '45');
	`)

	stmts, err := Statements(context.Background(), beforeSQL, afterSQL, diffs)
	if err != nil {
		t.Fatalf("Statements: %v", err)
	}
	applyLikeRunner(t, db, stmts)

	rows, err := db.Query(`SELECT id, age FROM users ORDER BY id`)
	if err != nil {
		t.Fatalf("query users: %v", err)
	}
	defer func() { _ = rows.Close() }()

	want := map[int64]int64{1: 30, 2: 45}
	got := map[int64]int64{}
	for rows.Next() {
		var id, age int64
		if err := rows.Scan(&id, &age); err != nil {
			t.Fatalf("scan: %v", err)
		}
		got[id] = age
	}
	if len(got) != len(want) {
		t.Fatalf("want %d rows, got %d (%v)", len(want), len(got), got)
	}
	for id, age := range want {
		if got[id] != age {
			t.Errorf("row %d: want age %d, got %d", id, age, got[id])
		}
	}

	var colType string
	if err := db.QueryRow(`SELECT type FROM pragma_table_info('users') WHERE name = 'age'`).Scan(&colType); err != nil {
		t.Fatalf("read rebuilt column type: %v", err)
	}
	if colType != "INTEGER" {
		t.Errorf("want users.age to be INTEGER after rebuild, got %q", colType)
	}
}

// TestExecute_11_IndexTriggerViewStillWork covers scenario 11: an explicit
// index, a trigger, and a view all reference the rebuilt table, and all
// three must still exist and function afterward, not merely still be
// present in sqlite_master.
func TestExecute_11_IndexTriggerViewStillWork(t *testing.T) {
	beforeSQL := readFile(t, "../../testdata/schemas/11_rebuild_with_index_trigger_view/before.sql")
	afterSQL := readFile(t, "../../testdata/schemas/11_rebuild_with_index_trigger_view/after.sql")
	_, diffs := scenarioDiffs(t, "11_rebuild_with_index_trigger_view", beforeSQL, afterSQL)

	db := openSeededDB(t, beforeSQL, `
		INSERT INTO items (id, price) VALUES (1, '100');
		INSERT INTO items (id, price) VALUES (2, '250');
	`)

	stmts, err := Statements(context.Background(), beforeSQL, afterSQL, diffs)
	if err != nil {
		t.Fatalf("Statements: %v", err)
	}
	applyLikeRunner(t, db, stmts)

	// Data preserved.
	var price int64
	if err := db.QueryRow(`SELECT price FROM items WHERE id = 1`).Scan(&price); err != nil {
		t.Fatalf("query items: %v", err)
	}
	if price != 100 {
		t.Errorf("want price 100, got %d", price)
	}

	// The index still exists and the query planner still uses it.
	var indexName sql.NullString
	if err := db.QueryRow(`SELECT name FROM sqlite_master WHERE type = 'index' AND name = 'idx_items_price'`).Scan(&indexName); err != nil {
		t.Fatalf("index missing after rebuild: %v", err)
	}
	var plan string
	if err := db.QueryRow(`EXPLAIN QUERY PLAN SELECT * FROM items WHERE price = 100`).Scan(new(int), new(int), new(int), &plan); err != nil {
		t.Fatalf("explain query plan: %v", err)
	}
	if !strings.Contains(plan, "idx_items_price") {
		t.Errorf("want query plan to use idx_items_price, got %q", plan)
	}

	// The trigger still fires.
	if _, err := db.Exec(`UPDATE items SET price = 150 WHERE id = 1`); err != nil {
		t.Fatalf("update items: %v", err)
	}
	var auditCount int
	if err := db.QueryRow(`SELECT COUNT(*) FROM items_audit WHERE item_id = 1`).Scan(&auditCount); err != nil {
		t.Fatalf("query items_audit: %v", err)
	}
	if auditCount != 1 {
		t.Errorf("want the AFTER UPDATE trigger to have fired once, got %d audit rows", auditCount)
	}

	// The view still queries correctly.
	var viewPrice int64
	if err := db.QueryRow(`SELECT price FROM items_view WHERE id = 2`).Scan(&viewPrice); err != nil {
		t.Fatalf("query items_view: %v", err)
	}
	if viewPrice != 250 {
		t.Errorf("want items_view to report price 250, got %d", viewPrice)
	}
}

// TestExecute_18_FKCyclePreservesDataAndOrdering covers scenario 18: two
// tables that both need a rebuild and reference each other via a genuine FK
// cycle. It asserts both that every create-and-copy step precedes every
// drop (the ordering the spec's "Multi-table rebuild ordering" decision
// requires), and that the resulting data and FK relationships survive a
// real apply relying on foreign_key_check at commit rather than any
// statement order.
func TestExecute_18_FKCyclePreservesDataAndOrdering(t *testing.T) {
	beforeSQL := readFile(t, "../../testdata/schemas/18_fk_cycle_rebuild/before.sql")
	afterSQL := readFile(t, "../../testdata/schemas/18_fk_cycle_rebuild/after.sql")
	_, diffs := scenarioDiffs(t, "18_fk_cycle_rebuild", beforeSQL, afterSQL)
	if len(diffs) != 2 {
		t.Fatalf("want both a and b to need a rebuild, got %d diffs", len(diffs))
	}

	stmts, err := Statements(context.Background(), beforeSQL, afterSQL, diffs)
	if err != nil {
		t.Fatalf("Statements: %v", err)
	}

	lastCreateOrCopy := -1
	firstDrop := -1
	for i, stmt := range stmts {
		upper := strings.ToUpper(stmt)
		switch {
		case strings.HasPrefix(upper, "CREATE TABLE") || strings.HasPrefix(upper, "INSERT INTO"):
			lastCreateOrCopy = i
		case strings.HasPrefix(upper, "DROP TABLE") && firstDrop == -1:
			firstDrop = i
		}
	}
	if firstDrop == -1 || lastCreateOrCopy == -1 || firstDrop < lastCreateOrCopy {
		t.Fatalf("want every create/copy step before every drop step; last create/copy at %d, first drop at %d\nstatements:\n%s",
			lastCreateOrCopy, firstDrop, strings.Join(stmts, "\n\n"))
	}

	db := openSeededDB(t, beforeSQL, `
		INSERT INTO a (id, b_id, tag) VALUES (1, 1, '10');
		INSERT INTO b (id, a_id, label) VALUES (1, 1, '20');
	`)
	applyLikeRunner(t, db, stmts)

	var tag, label int64
	if err := db.QueryRow(`SELECT tag FROM a WHERE id = 1`).Scan(&tag); err != nil {
		t.Fatalf("query a: %v", err)
	}
	if err := db.QueryRow(`SELECT label FROM b WHERE id = 1`).Scan(&label); err != nil {
		t.Fatalf("query b: %v", err)
	}
	if tag != 10 || label != 20 {
		t.Fatalf("want tag/label copied and coerced to INTEGER, got tag=%d label=%d", tag, label)
	}

	var aBID, bAID int64
	if err := db.QueryRow(`SELECT b_id FROM a WHERE id = 1`).Scan(&aBID); err != nil {
		t.Fatalf("query a.b_id: %v", err)
	}
	if err := db.QueryRow(`SELECT a_id FROM b WHERE id = 1`).Scan(&bAID); err != nil {
		t.Fatalf("query b.a_id: %v", err)
	}
	if aBID != 1 || bAID != 1 {
		t.Errorf("want the FK cycle's row references preserved, got a.b_id=%d b.a_id=%d", aBID, bAID)
	}
}

// TestExecute_TriggerOnUnrebuiltTableReferencingRebuiltTableFires covers
// finding (a): a trigger attached to a table that isn't itself rebuilt, but
// whose body references a table that is, must survive the rename and still
// fire correctly afterward.
func TestExecute_TriggerOnUnrebuiltTableReferencingRebuiltTableFires(t *testing.T) {
	beforeSQL := `
		CREATE TABLE users (id INTEGER PRIMARY KEY, age TEXT) STRICT;
		CREATE TABLE log (id INTEGER PRIMARY KEY, note TEXT) STRICT;
		CREATE TRIGGER trg_log_touch AFTER INSERT ON log
		BEGIN
			UPDATE users SET age = age + 1 WHERE id = NEW.id;
		END;
	`
	afterSQL := `
		CREATE TABLE users (id INTEGER PRIMARY KEY, age INTEGER) STRICT;
		CREATE TABLE log (id INTEGER PRIMARY KEY, note TEXT) STRICT;
		CREATE TRIGGER trg_log_touch AFTER INSERT ON log
		BEGIN
			UPDATE users SET age = age + 1 WHERE id = NEW.id;
		END;
	`
	diffs := inlineDiffs(t, beforeSQL, afterSQL, "users")

	db := openSeededDB(t, beforeSQL, `INSERT INTO users (id, age) VALUES (1, '30');`)

	stmts, err := Statements(context.Background(), beforeSQL, afterSQL, diffs)
	if err != nil {
		t.Fatalf("Statements: %v", err)
	}
	applyLikeRunner(t, db, stmts)

	if _, err := db.Exec(`INSERT INTO log (id, note) VALUES (1, 'hi')`); err != nil {
		t.Fatalf("insert into log (trigger fire): %v", err)
	}
	var age int64
	if err := db.QueryRow(`SELECT age FROM users WHERE id = 1`).Scan(&age); err != nil {
		t.Fatalf("query users: %v", err)
	}
	if age != 31 {
		t.Errorf("want trigger to have incremented age to 31, got %d", age)
	}
}

// TestExecute_ViewChainReferencingRebuiltTableStillWorks covers finding
// (b): v2 selects from v1, which selects from the rebuilt table. Both must
// survive the rename and v2 must still return rows afterward.
func TestExecute_ViewChainReferencingRebuiltTableStillWorks(t *testing.T) {
	beforeSQL := `
		CREATE TABLE items (id INTEGER PRIMARY KEY, price TEXT) STRICT;
		CREATE VIEW v1 AS SELECT id, price FROM items;
		CREATE VIEW v2 AS SELECT id FROM v1;
	`
	afterSQL := `
		CREATE TABLE items (id INTEGER PRIMARY KEY, price INTEGER) STRICT;
		CREATE VIEW v1 AS SELECT id, price FROM items;
		CREATE VIEW v2 AS SELECT id FROM v1;
	`
	diffs := inlineDiffs(t, beforeSQL, afterSQL, "items")

	db := openSeededDB(t, beforeSQL, `INSERT INTO items (id, price) VALUES (1, '100');`)

	stmts, err := Statements(context.Background(), beforeSQL, afterSQL, diffs)
	if err != nil {
		t.Fatalf("Statements: %v", err)
	}
	applyLikeRunner(t, db, stmts)

	var id int64
	if err := db.QueryRow(`SELECT id FROM v2 WHERE id = 1`).Scan(&id); err != nil {
		t.Fatalf("query v2: %v", err)
	}
	if id != 1 {
		t.Errorf("want v2 to return row id 1, got %d", id)
	}
}

// TestExecute_TriggerAcrossTwoRebuiltTablesSurvivesRename covers finding
// (c): p and c both need a rebuild, and trigger tc — attached to c —
// updates p in its body. Without dropping tc before p's own rename, p's
// rename fails because tc (still attached to the not-yet-dropped old c)
// references it.
func TestExecute_TriggerAcrossTwoRebuiltTablesSurvivesRename(t *testing.T) {
	beforeSQL := `
		CREATE TABLE p (id INTEGER PRIMARY KEY, counter TEXT) STRICT;
		CREATE TABLE c (id INTEGER PRIMARY KEY, p_id INTEGER REFERENCES p(id), val TEXT) STRICT;
		CREATE TRIGGER tc AFTER INSERT ON c
		BEGIN
			UPDATE p SET counter = counter + 1 WHERE id = NEW.p_id;
		END;
	`
	afterSQL := `
		CREATE TABLE p (id INTEGER PRIMARY KEY, counter INTEGER) STRICT;
		CREATE TABLE c (id INTEGER PRIMARY KEY, p_id INTEGER REFERENCES p(id), val INTEGER) STRICT;
		CREATE TRIGGER tc AFTER INSERT ON c
		BEGIN
			UPDATE p SET counter = counter + 1 WHERE id = NEW.p_id;
		END;
	`
	diffs := inlineDiffs(t, beforeSQL, afterSQL, "p", "c")
	if len(diffs) != 2 {
		t.Fatalf("want both p and c to need a rebuild, got %d diffs", len(diffs))
	}

	db := openSeededDB(t, beforeSQL, `
		INSERT INTO p (id, counter) VALUES (1, '0');
		INSERT INTO c (id, p_id, val) VALUES (1, 1, '5');
	`)

	stmts, err := Statements(context.Background(), beforeSQL, afterSQL, diffs)
	if err != nil {
		t.Fatalf("Statements: %v", err)
	}
	applyLikeRunner(t, db, stmts)

	if _, err := db.Exec(`INSERT INTO c (id, p_id, val) VALUES (2, 1, 7)`); err != nil {
		t.Fatalf("insert into c (trigger fire): %v", err)
	}
	var counter int64
	if err := db.QueryRow(`SELECT counter FROM p WHERE id = 1`).Scan(&counter); err != nil {
		t.Fatalf("query p: %v", err)
	}
	// The seed insert into c already fired tc once (bumping counter from 0
	// to 1) before the rebuild even runs; the post-rebuild insert must fire
	// it again.
	if counter != 2 {
		t.Errorf("want trigger tc to have fired both before and after the rebuild, taking p.counter to 2, got %d", counter)
	}
}

// TestExecute_NonASCIITableNameRebuilds covers finding 2: a bare
// non-ASCII table name must still produce valid CREATE TABLE SQL when
// renamed for the temporary table.
func TestExecute_NonASCIITableNameRebuilds(t *testing.T) {
	beforeSQL := `CREATE TABLE bücher (id INTEGER PRIMARY KEY, v TEXT) STRICT;`
	afterSQL := `CREATE TABLE bücher (id INTEGER PRIMARY KEY, v INTEGER) STRICT;`
	diffs := inlineDiffs(t, beforeSQL, afterSQL, "bücher")

	db := openSeededDB(t, beforeSQL, `INSERT INTO bücher (id, v) VALUES (1, '42');`)

	stmts, err := Statements(context.Background(), beforeSQL, afterSQL, diffs)
	if err != nil {
		t.Fatalf("Statements: %v", err)
	}
	applyLikeRunner(t, db, stmts)

	var v int64
	if err := db.QueryRow(`SELECT v FROM bücher WHERE id = 1`).Scan(&v); err != nil {
		t.Fatalf("query bücher: %v", err)
	}
	if v != 42 {
		t.Errorf("want v = 42, got %d", v)
	}
}

// TestExecute_AutoincrementSequenceNotReusedAfterRebuild covers finding 3:
// a rebuild must carry AUTOINCREMENT's high-water mark over, so a deleted
// row's id is never handed out again.
func TestExecute_AutoincrementSequenceNotReusedAfterRebuild(t *testing.T) {
	beforeSQL := `CREATE TABLE t (id INTEGER PRIMARY KEY AUTOINCREMENT, v TEXT) STRICT;`
	afterSQL := `CREATE TABLE t (id INTEGER PRIMARY KEY AUTOINCREMENT, v INTEGER) STRICT;`
	diffs := inlineDiffs(t, beforeSQL, afterSQL, "t")

	db := openSeededDB(t, beforeSQL, `
		INSERT INTO t (v) VALUES ('10');
		INSERT INTO t (v) VALUES ('20');
		INSERT INTO t (v) VALUES ('30');
		DELETE FROM t WHERE id = 3;
	`)

	stmts, err := Statements(context.Background(), beforeSQL, afterSQL, diffs)
	if err != nil {
		t.Fatalf("Statements: %v", err)
	}
	applyLikeRunner(t, db, stmts)

	res, err := db.Exec(`INSERT INTO t (v) VALUES (4)`)
	if err != nil {
		t.Fatalf("insert into t: %v", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		t.Fatalf("LastInsertId: %v", err)
	}
	if id != 4 {
		t.Errorf("want next id 4 (never reusing deleted id 3), got %d", id)
	}
}

// TestExecute_AutoincrementSequenceCarriedWhenRebuildLeavesTableEmpty
// covers finding 3's empty-table case: if the rebuild's copy step inserts
// no rows at all, the temporary table gets no sqlite_sequence row of its
// own from the copy, so the carry-over must still insert one.
func TestExecute_AutoincrementSequenceCarriedWhenRebuildLeavesTableEmpty(t *testing.T) {
	beforeSQL := `CREATE TABLE t (id INTEGER PRIMARY KEY AUTOINCREMENT, v TEXT) STRICT;`
	afterSQL := `CREATE TABLE t (id INTEGER PRIMARY KEY AUTOINCREMENT, v INTEGER) STRICT;`
	diffs := inlineDiffs(t, beforeSQL, afterSQL, "t")

	db := openSeededDB(t, beforeSQL, `
		INSERT INTO t (v) VALUES ('a');
		INSERT INTO t (v) VALUES ('b');
		DELETE FROM t WHERE id IN (1, 2);
	`)

	stmts, err := Statements(context.Background(), beforeSQL, afterSQL, diffs)
	if err != nil {
		t.Fatalf("Statements: %v", err)
	}
	applyLikeRunner(t, db, stmts)

	res, err := db.Exec(`INSERT INTO t (v) VALUES (3)`)
	if err != nil {
		t.Fatalf("insert into t: %v", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		t.Fatalf("LastInsertId: %v", err)
	}
	if id != 3 {
		t.Errorf("want next id 3 (never reusing deleted ids 1 or 2), got %d", id)
	}
}

// TestExecute_RebuildTableWithNonAutoincrementCommentSucceeds covers the
// finding that a "not AUTOINCREMENT" comment — a common idiom, since
// SQLite's own docs discourage the keyword — must not make hasAutoincrement
// emit sqlite_sequence carry-over statements. When no table in the database
// actually uses AUTOINCREMENT, sqlite_sequence doesn't exist at all, so
// emitting them fails the whole migration.
func TestExecute_RebuildTableWithNonAutoincrementCommentSucceeds(t *testing.T) {
	beforeSQL := "CREATE TABLE t (\n" +
		"  id INTEGER PRIMARY KEY, -- deliberately not AUTOINCREMENT\n" +
		"  v TEXT\n" +
		") STRICT;"
	afterSQL := "CREATE TABLE t (\n" +
		"  id INTEGER PRIMARY KEY, -- deliberately not AUTOINCREMENT\n" +
		"  v INTEGER\n" +
		") STRICT;"
	diffs := inlineDiffs(t, beforeSQL, afterSQL, "t")

	db := openSeededDB(t, beforeSQL, `INSERT INTO t (id, v) VALUES (1, '42');`)

	stmts, err := Statements(context.Background(), beforeSQL, afterSQL, diffs)
	if err != nil {
		t.Fatalf("Statements: %v", err)
	}
	applyLikeRunner(t, db, stmts)

	var v int64
	if err := db.QueryRow(`SELECT v FROM t WHERE id = 1`).Scan(&v); err != nil {
		t.Fatalf("query t: %v", err)
	}
	if v != 42 {
		t.Errorf("want v = 42, got %d", v)
	}
}

// TestExecute_RebuildTableWithAutoincrementStringLiteralSucceeds covers the
// same finding as TestExecute_RebuildTableWithNonAutoincrementCommentSucceeds
// for the other reported case: a CHECK constraint whose allowed values
// include the string 'autoincrement'.
func TestExecute_RebuildTableWithAutoincrementStringLiteralSucceeds(t *testing.T) {
	beforeSQL := `CREATE TABLE t (id INTEGER PRIMARY KEY, mode TEXT CHECK (mode IN ('autoincrement','manual')), v TEXT) STRICT;`
	afterSQL := `CREATE TABLE t (id INTEGER PRIMARY KEY, mode TEXT CHECK (mode IN ('autoincrement','manual')), v INTEGER) STRICT;`
	diffs := inlineDiffs(t, beforeSQL, afterSQL, "t")

	db := openSeededDB(t, beforeSQL, `INSERT INTO t (id, mode, v) VALUES (1, 'manual', '42');`)

	stmts, err := Statements(context.Background(), beforeSQL, afterSQL, diffs)
	if err != nil {
		t.Fatalf("Statements: %v", err)
	}
	applyLikeRunner(t, db, stmts)

	var v int64
	if err := db.QueryRow(`SELECT v FROM t WHERE id = 1`).Scan(&v); err != nil {
		t.Fatalf("query t: %v", err)
	}
	if v != 42 {
		t.Errorf("want v = 42, got %d", v)
	}
}

// TestExecute_ViewAndTriggerRemovedAlongsideRebuild covers a view and a
// trigger that exist before the migration but not after it, both
// referencing the rebuilt table: they must still be dropped before the
// rename, or SQLite's rename-time schema recompile fails on them.
func TestExecute_ViewAndTriggerRemovedAlongsideRebuild(t *testing.T) {
	beforeSQL := `
		CREATE TABLE users (id INTEGER PRIMARY KEY, age TEXT) STRICT;
		CREATE TABLE log (id INTEGER PRIMARY KEY, note TEXT) STRICT;
		CREATE VIEW users_view AS SELECT id, age FROM users;
		CREATE TRIGGER trg_log_touch AFTER INSERT ON log
		BEGIN
			UPDATE users SET age = age WHERE id = NEW.id;
		END;
	`
	afterSQL := `
		CREATE TABLE users (id INTEGER PRIMARY KEY, age INTEGER) STRICT;
		CREATE TABLE log (id INTEGER PRIMARY KEY, note TEXT) STRICT;
	`
	diffs := inlineDiffs(t, beforeSQL, afterSQL, "users")

	db := openSeededDB(t, beforeSQL, `INSERT INTO users (id, age) VALUES (1, '30');`)

	stmts, err := Statements(context.Background(), beforeSQL, afterSQL, diffs)
	if err != nil {
		t.Fatalf("Statements: %v", err)
	}
	applyLikeRunner(t, db, stmts)

	var n int
	if err := db.QueryRow(`SELECT count(*) FROM sqlite_master WHERE name IN ('users_view', 'trg_log_touch')`).Scan(&n); err != nil {
		t.Fatalf("query sqlite_master: %v", err)
	}
	if n != 0 {
		t.Errorf("want removed view and trigger gone after rebuild, %d still present", n)
	}
	var age int64
	if err := db.QueryRow(`SELECT age FROM users WHERE id = 1`).Scan(&age); err != nil {
		t.Fatalf("query users: %v", err)
	}
	if age != 30 {
		t.Errorf("want age 30, got %d", age)
	}
}

// TestExecute_RowidPreservedWithoutIntegerPrimaryKey covers a rowid table
// whose primary key isn't a rowid alias: its rowids must survive the copy
// unchanged, or an FTS5 external-content index keyed on them silently
// points at the wrong rows.
func TestExecute_RowidPreservedWithoutIntegerPrimaryKey(t *testing.T) {
	beforeSQL := `
		CREATE TABLE notes (slug TEXT PRIMARY KEY, body TEXT) STRICT;
		CREATE VIRTUAL TABLE notes_fts USING fts5(body, content='notes');
	`
	afterSQL := `
		CREATE TABLE notes (slug TEXT PRIMARY KEY, body TEXT CHECK (length(body) > 0)) STRICT;
		CREATE VIRTUAL TABLE notes_fts USING fts5(body, content='notes');
	`
	diffs := inlineDiffs(t, beforeSQL, afterSQL, "notes")

	db := openSeededDB(t, beforeSQL, `
		INSERT INTO notes (slug, body) VALUES ('a', 'apple'), ('b', 'banana'), ('c', 'cherry');
		INSERT INTO notes_fts (notes_fts) VALUES ('rebuild');
		INSERT INTO notes_fts (notes_fts, rowid, body) VALUES ('delete', 1, 'apple');
		DELETE FROM notes WHERE slug = 'a';
	`)

	stmts, err := Statements(context.Background(), beforeSQL, afterSQL, diffs)
	if err != nil {
		t.Fatalf("Statements: %v", err)
	}
	applyLikeRunner(t, db, stmts)

	want := map[string]int64{"b": 2, "c": 3}
	for slug, rowid := range want {
		var got int64
		if err := db.QueryRow(`SELECT rowid FROM notes WHERE slug = ?`, slug).Scan(&got); err != nil {
			t.Fatalf("query notes %q: %v", slug, err)
		}
		if got != rowid {
			t.Errorf("row %q: want rowid %d, got %d", slug, rowid, got)
		}
	}

	var slug string
	if err := db.QueryRow(`SELECT n.slug FROM notes_fts JOIN notes n ON n.rowid = notes_fts.rowid WHERE notes_fts MATCH 'cherry'`).Scan(&slug); err != nil {
		t.Fatalf("fts lookup: %v", err)
	}
	if slug != "c" {
		t.Errorf("want fts match for 'cherry' to resolve to row c, got %q", slug)
	}
}

// TestExecute_GeneratedColumnBecomingPlainKeepsValues covers a generated
// column turned into an ordinary one: pragma_table_info doesn't list
// generated columns, so without reading pragma_table_xinfo the column
// looks newly added and every row's value is lost.
func TestExecute_GeneratedColumnBecomingPlainKeepsValues(t *testing.T) {
	beforeSQL := `CREATE TABLE sums (id INTEGER PRIMARY KEY, a INTEGER, b INTEGER, total INTEGER AS (a + b)) STRICT;`
	afterSQL := `CREATE TABLE sums (id INTEGER PRIMARY KEY, a INTEGER, b INTEGER, total INTEGER) STRICT;`
	diffs := inlineDiffs(t, beforeSQL, afterSQL, "sums")

	db := openSeededDB(t, beforeSQL, `INSERT INTO sums (id, a, b) VALUES (1, 2, 3), (2, 10, 20);`)

	stmts, err := Statements(context.Background(), beforeSQL, afterSQL, diffs)
	if err != nil {
		t.Fatalf("Statements: %v", err)
	}
	applyLikeRunner(t, db, stmts)

	want := map[int64]int64{1: 5, 2: 30}
	for id, total := range want {
		var got sql.NullInt64
		if err := db.QueryRow(`SELECT total FROM sums WHERE id = ?`, id).Scan(&got); err != nil {
			t.Fatalf("query sums %d: %v", id, err)
		}
		if !got.Valid || got.Int64 != total {
			t.Errorf("row %d: want total %d, got %v", id, total, got)
		}
	}
}

// TestExecute_PlainColumnBecomingGeneratedRebuilds covers the reverse
// direction: the copy must not name the now-generated column as an INSERT
// target, which SQLite rejects.
func TestExecute_PlainColumnBecomingGeneratedRebuilds(t *testing.T) {
	beforeSQL := `CREATE TABLE sums (id INTEGER PRIMARY KEY, a INTEGER, b INTEGER, total INTEGER) STRICT;`
	afterSQL := `CREATE TABLE sums (id INTEGER PRIMARY KEY, a INTEGER, b INTEGER, total INTEGER AS (a + b) STORED) STRICT;`
	diffs := inlineDiffs(t, beforeSQL, afterSQL, "sums")

	db := openSeededDB(t, beforeSQL, `INSERT INTO sums (id, a, b, total) VALUES (1, 2, 3, 99);`)

	stmts, err := Statements(context.Background(), beforeSQL, afterSQL, diffs)
	if err != nil {
		t.Fatalf("Statements: %v", err)
	}
	applyLikeRunner(t, db, stmts)

	var total int64
	if err := db.QueryRow(`SELECT total FROM sums WHERE id = 1`).Scan(&total); err != nil {
		t.Fatalf("query sums: %v", err)
	}
	if total != 5 {
		t.Errorf("want recomputed total 5, got %d", total)
	}
}
