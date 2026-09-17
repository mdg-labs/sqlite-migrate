package sqldefwrap

import (
	"context"
	"database/sql"
	"strings"
	"testing"

	_ "modernc.org/sqlite"
)

// execAll applies every statement in ddls in order against db, failing the
// test immediately with the failing statement if any of them errors.
func execAll(t *testing.T, db *sql.DB, ddls []string) {
	t.Helper()
	for _, ddl := range ddls {
		if _, err := db.ExecContext(context.Background(), ddl); err != nil {
			t.Fatalf("exec %q: %v", ddl, err)
		}
	}
}

func openSeeded(t *testing.T, schemaSQL string) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	db.SetMaxOpenConns(1)
	if _, err := db.ExecContext(context.Background(), schemaSQL); err != nil {
		t.Fatalf("seed schema: %v", err)
	}
	return db
}

func TestDiff_NewTable(t *testing.T) {
	current := `CREATE TABLE users (id INTEGER PRIMARY KEY) STRICT;`
	desired := current + `
CREATE TABLE tags (id INTEGER PRIMARY KEY, name TEXT) STRICT;`

	ddls, err := New().Diff(desired, current)
	if err != nil {
		t.Fatalf("Diff: %v", err)
	}
	if len(ddls) != 1 || !strings.Contains(ddls[0], "CREATE TABLE tags") {
		t.Fatalf("want a single CREATE TABLE tags statement, got %v", ddls)
	}

	db := openSeeded(t, current)
	execAll(t, db, ddls)
}

func TestDiff_NewColumnNoConstraints(t *testing.T) {
	current := `CREATE TABLE users (id INTEGER PRIMARY KEY) STRICT;`
	desired := `CREATE TABLE users (id INTEGER PRIMARY KEY, name TEXT) STRICT;`

	ddls, err := New().Diff(desired, current)
	if err != nil {
		t.Fatalf("Diff: %v", err)
	}
	if len(ddls) != 1 || ddls[0] != `ALTER TABLE users ADD COLUMN name text` {
		t.Fatalf("want a single plain ADD COLUMN statement, got %v", ddls)
	}

	db := openSeeded(t, current)
	execAll(t, db, ddls)
}

// TestDiff_NewColumnWithReferences covers testdata scenario 04: a new
// column with a REFERENCES clause must come back as one direct ADD COLUMN
// statement with the FK clause preserved, not sqldef's default two
// statements (the second of which — ALTER TABLE ... ADD CONSTRAINT ...
// FOREIGN KEY — SQLite's ALTER TABLE has never supported).
func TestDiff_NewColumnWithReferences(t *testing.T) {
	current := `CREATE TABLE users (id INTEGER PRIMARY KEY) STRICT;
CREATE TABLE orders (id INTEGER PRIMARY KEY) STRICT;`
	desired := `CREATE TABLE users (id INTEGER PRIMARY KEY) STRICT;
CREATE TABLE orders (id INTEGER PRIMARY KEY, user_id INTEGER REFERENCES users(id)) STRICT;`

	ddls, err := New().Diff(desired, current)
	if err != nil {
		t.Fatalf("Diff: %v", err)
	}
	if len(ddls) != 1 {
		t.Fatalf("want a single folded ADD COLUMN statement, got %v", ddls)
	}
	want := `ALTER TABLE orders ADD COLUMN user_id integer REFERENCES users (id)`
	if ddls[0] != want {
		t.Fatalf("ddls[0] = %q, want %q", ddls[0], want)
	}

	db := openSeeded(t, current)
	if _, err := db.Exec(`PRAGMA foreign_keys = ON`); err != nil {
		t.Fatalf("enable foreign_keys: %v", err)
	}
	execAll(t, db, ddls)

	if _, err := db.Exec(`INSERT INTO users (id) VALUES (1)`); err != nil {
		t.Fatalf("seed users: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO orders (id, user_id) VALUES (1, 1)`); err != nil {
		t.Fatalf("insert satisfying the FK should succeed: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO orders (id, user_id) VALUES (2, 99)`); err == nil {
		t.Fatalf("insert violating the FK should have been refused")
	}
}

// TestDiff_NewColumnWithReferencesAndDefault covers testdata scenario 05:
// a new column combining REFERENCES with a non-NULL default is something
// SQLite itself refuses outright once the table already has rows and
// foreign key enforcement is on — the wrapper's job is only to hand back
// valid SQL for SQLite to judge, not to special-case this combination
// itself.
func TestDiff_NewColumnWithReferencesAndDefault(t *testing.T) {
	current := `CREATE TABLE users (id INTEGER PRIMARY KEY) STRICT;
CREATE TABLE orders (id INTEGER PRIMARY KEY) STRICT;`
	desired := `CREATE TABLE users (id INTEGER PRIMARY KEY) STRICT;
CREATE TABLE orders (id INTEGER PRIMARY KEY, user_id INTEGER NOT NULL DEFAULT 0 REFERENCES users(id)) STRICT;`

	ddls, err := New().Diff(desired, current)
	if err != nil {
		t.Fatalf("Diff: %v", err)
	}
	if len(ddls) != 1 {
		t.Fatalf("want a single folded ADD COLUMN statement, got %v", ddls)
	}
	want := `ALTER TABLE orders ADD COLUMN user_id integer NOT NULL DEFAULT 0 REFERENCES users (id)`
	if ddls[0] != want {
		t.Fatalf("ddls[0] = %q, want %q", ddls[0], want)
	}

	db := openSeeded(t, current)
	if _, err := db.Exec(`PRAGMA foreign_keys = ON`); err != nil {
		t.Fatalf("enable foreign_keys: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO orders (id) VALUES (1)`); err != nil {
		t.Fatalf("seed orders: %v", err)
	}

	_, err = db.Exec(ddls[0])
	if err == nil {
		t.Fatalf("SQLite should have refused REFERENCES + non-NULL DEFAULT on an existing table")
	}
	if !strings.Contains(err.Error(), "REFERENCES") {
		t.Fatalf("want a clear error naming the REFERENCES restriction, got: %v", err)
	}
}

// TestDiff_NewColumnOnNonASCIITable covers issue #29: adding a column to a
// table named with a non-ASCII identifier used to fail Diff outright with a
// sqldef parser syntax error, even though the change is an ordinary safe
// additive one internal/rebuild already supports for the same identifier
// class (scenario 19).
func TestDiff_NewColumnOnNonASCIITable(t *testing.T) {
	current := `CREATE TABLE bücher (id INTEGER PRIMARY KEY) STRICT;`
	desired := `CREATE TABLE bücher (id INTEGER PRIMARY KEY, titel TEXT) STRICT;`

	ddls, err := New().Diff(desired, current)
	if err != nil {
		t.Fatalf("Diff: %v", err)
	}
	want := `ALTER TABLE "bücher" ADD COLUMN titel text`
	if len(ddls) != 1 || ddls[0] != want {
		t.Fatalf("ddls = %v, want [%q]", ddls, want)
	}

	db := openSeeded(t, current)
	execAll(t, db, ddls)
}

// TestDiff_NewColumnContainingDollarSign covers issue #29's other named
// identifier class: a column name containing '$', which sqldef's own
// parser also rejects unquoted (scenario 20 covers the same class on the
// internal/rebuild path).
func TestDiff_NewColumnContainingDollarSign(t *testing.T) {
	current := `CREATE TABLE foo$bar (id INTEGER PRIMARY KEY) STRICT;`
	desired := `CREATE TABLE foo$bar (id INTEGER PRIMARY KEY, "titel$x" TEXT) STRICT;`

	ddls, err := New().Diff(desired, current)
	if err != nil {
		t.Fatalf("Diff: %v", err)
	}
	want := `ALTER TABLE "foo$bar" ADD COLUMN "titel$x" text`
	if len(ddls) != 1 || ddls[0] != want {
		t.Fatalf("ddls = %v, want [%q]", ddls, want)
	}

	db := openSeeded(t, current)
	execAll(t, db, ddls)
}

// TestDiff_NewColumnWithReferencesOnNonASCIITable covers the fold path
// (foldForeignKeysIntoAddColumn) with a non-ASCII table/column name, since
// quoteExoticIdentifiers changes what sqldef's raw output looks like for
// this identifier class and the fold logic re-parses that output itself.
func TestDiff_NewColumnWithReferencesOnNonASCIITable(t *testing.T) {
	current := `CREATE TABLE bücher (id INTEGER PRIMARY KEY) STRICT;
CREATE TABLE orders (id INTEGER PRIMARY KEY) STRICT;`
	desired := `CREATE TABLE bücher (id INTEGER PRIMARY KEY) STRICT;
CREATE TABLE orders (id INTEGER PRIMARY KEY, buch_id INTEGER REFERENCES bücher(id)) STRICT;`

	ddls, err := New().Diff(desired, current)
	if err != nil {
		t.Fatalf("Diff: %v", err)
	}
	want := `ALTER TABLE orders ADD COLUMN buch_id integer REFERENCES "bücher" (id)`
	if len(ddls) != 1 || ddls[0] != want {
		t.Fatalf("ddls = %v, want [%q]", ddls, want)
	}

	db := openSeeded(t, current)
	if _, err := db.Exec(`PRAGMA foreign_keys = ON`); err != nil {
		t.Fatalf("enable foreign_keys: %v", err)
	}
	execAll(t, db, ddls)
}

// TestDiff_ReferencesOnExistingColumn covers a REFERENCES clause added to a
// column that already existed rather than one this Diff call is adding.
// sqldef still emits it as "ALTER TABLE ... ADD CONSTRAINT ... FOREIGN KEY
// ..." — syntax SQLite's ALTER TABLE has never supported — but there is no
// ADD COLUMN statement in this diff to fold it into, so Diff must report an
// error instead of handing back a statement that would fail at exec time.
func TestDiff_ReferencesOnExistingColumn(t *testing.T) {
	current := `CREATE TABLE users (id INTEGER PRIMARY KEY) STRICT;
CREATE TABLE orders (id INTEGER PRIMARY KEY, user_id INTEGER) STRICT;`
	desired := `CREATE TABLE users (id INTEGER PRIMARY KEY) STRICT;
CREATE TABLE orders (id INTEGER PRIMARY KEY, user_id INTEGER REFERENCES users(id)) STRICT;`

	ddls, err := New().Diff(desired, current)
	if err == nil {
		t.Fatalf("want an error for a REFERENCES clause on an existing column, got ddls %v", ddls)
	}
	if !strings.Contains(err.Error(), "ADD CONSTRAINT") && !strings.Contains(err.Error(), "foreign key") {
		t.Fatalf("want an error naming the unsupported ADD CONSTRAINT/foreign key statement, got: %v", err)
	}
}
