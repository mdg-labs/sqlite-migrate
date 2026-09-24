package sqldefwrap

import (
	"context"
	"database/sql"
	"fmt"
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
// TestDiff_ExistingColumnNamedSQLdefKeyword covers issue #45: sqldef's
// shared multi-dialect grammar rejects a long list of bare words as
// column/table names that SQLite itself accepts as ordinary identifiers.
// Adding a plain nullable column to a table that already has one of these
// words as an unquoted existing column — the shape a replayed, already-
// applied migration hands Diff (td.Before.SQL/td.After.SQL come straight
// from sqlite_master, not from any re-quoted schema.sql) — used to fail
// Diff outright with a sqldef syntax error instead of producing the direct
// ADD COLUMN this additive change should be.
func TestDiff_ExistingColumnNamedSQLdefKeyword(t *testing.T) {
	words := []string{
		"serial", "bigserial", "smallserial",
		"key", "value", "text", "date", "time", "json", "uuid",
	}
	for _, w := range words {
		t.Run(w, func(t *testing.T) {
			current := fmt.Sprintf(`CREATE TABLE t (id INTEGER PRIMARY KEY, %s TEXT) STRICT;`, w)
			desired := fmt.Sprintf(`CREATE TABLE t (id INTEGER PRIMARY KEY, %s TEXT, extra TEXT) STRICT;`, w)

			ddls, err := New().Diff(desired, current)
			if err != nil {
				t.Fatalf("Diff: %v", err)
			}
			if len(ddls) != 1 || !strings.Contains(ddls[0], "ADD COLUMN extra") {
				t.Fatalf("want a single ADD COLUMN extra statement, got %v", ddls)
			}

			db := openSeeded(t, current)
			execAll(t, db, ddls)
		})
	}
}

// TestDiff_PrimaryKeyAndTextTypeStayUnquoted covers the other half of
// issue #45's fix: quoting a colliding bare word is scoped to specific
// identifier positions (table name, column-definition name, PRIMARY
// KEY/UNIQUE/FOREIGN KEY column lists, a REFERENCES target), never to
// every occurrence of the word. "TEXT" the column type and "PRIMARY KEY"
// the inline constraint are both words sqldef's parser also rejects bare
// as a column name, but neither is in a quotable position here, so both
// must reach sqldef exactly as written.
func TestDiff_PrimaryKeyAndTextTypeStayUnquoted(t *testing.T) {
	current := `CREATE TABLE widgets (id INTEGER PRIMARY KEY, serial TEXT) STRICT;`
	desired := `CREATE TABLE widgets (id INTEGER PRIMARY KEY, serial TEXT, name TEXT) STRICT;`

	ddls, err := New().Diff(desired, current)
	if err != nil {
		t.Fatalf("Diff: %v", err)
	}
	want := `ALTER TABLE widgets ADD COLUMN name text`
	if len(ddls) != 1 || ddls[0] != want {
		t.Fatalf("ddls = %v, want [%q]", ddls, want)
	}

	db := openSeeded(t, current)
	execAll(t, db, ddls)
}

// TestQuoteCollidingKeywords_ScopesQuotingToIdentifierPositions is a direct
// check on the rewrite quoteCollidingKeywords performs, independent of
// what sqldef then does with it: a colliding word is quoted at a column-
// definition name position (serial, key) but left bare as a column type
// (TEXT) or inside an inline constraint keyword sequence (PRIMARY KEY).
func TestQuoteCollidingKeywords_ScopesQuotingToIdentifierPositions(t *testing.T) {
	in := `CREATE TABLE t (id INTEGER PRIMARY KEY, serial TEXT, key TEXT, name TEXT) STRICT;`
	want := `CREATE TABLE t (id INTEGER PRIMARY KEY, "serial" TEXT, "key" TEXT, name TEXT) STRICT;`
	if got := quoteCollidingKeywords(in); got != want {
		t.Fatalf("quoteCollidingKeywords(%q) = %q, want %q", in, got, want)
	}
}

// TestDiff_UniqueConstraintOnKeywordColumn covers testdata scenario 22: a
// table-level UNIQUE (…) column list naming a colliding word must be
// quoted too, or sqldef fails the same way it does for the column
// definition itself.
func TestDiff_UniqueConstraintOnKeywordColumn(t *testing.T) {
	current := `CREATE TABLE widgets (id INTEGER PRIMARY KEY, key TEXT, UNIQUE (key)) STRICT;`
	desired := `CREATE TABLE widgets (id INTEGER PRIMARY KEY, key TEXT, value TEXT, UNIQUE (key)) STRICT;`

	ddls, err := New().Diff(desired, current)
	if err != nil {
		t.Fatalf("Diff: %v", err)
	}
	if len(ddls) != 1 || !strings.Contains(ddls[0], "ADD COLUMN") {
		t.Fatalf("want a single ADD COLUMN statement, got %v", ddls)
	}

	db := openSeeded(t, current)
	execAll(t, db, ddls)
}

// TestDiff_IndexOnKeywordNamedTable covers a CREATE [UNIQUE] INDEX whose
// index name or ON table is a colliding word: sqldef rejects both bare, so
// an unrelated additive change on a schema carrying such an index used to
// fail Diff with a syntax error.
func TestDiff_IndexOnKeywordNamedTable(t *testing.T) {
	cases := map[string]struct{ table, index string }{
		"table name": {
			table: `CREATE TABLE value (id INTEGER PRIMARY KEY, x TEXT%s) STRICT;`,
			index: `CREATE INDEX value_x ON value (x);`,
		},
		"index name": {
			table: `CREATE TABLE t (id INTEGER PRIMARY KEY, x TEXT%s) STRICT;`,
			index: `CREATE INDEX serial ON t (x);`,
		},
		"unique if not exists": {
			table: `CREATE TABLE key (id INTEGER PRIMARY KEY, date TEXT%s) STRICT;`,
			index: `CREATE UNIQUE INDEX IF NOT EXISTS key_date ON key (date DESC);`,
		},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			current := fmt.Sprintf(c.table, "") + "\n" + c.index
			desired := fmt.Sprintf(c.table, ", extra TEXT") + "\n" + c.index

			ddls, err := New().Diff(desired, current)
			if err != nil {
				t.Fatalf("Diff: %v", err)
			}
			if len(ddls) != 1 || !strings.Contains(ddls[0], "ADD COLUMN extra") {
				t.Fatalf("want a single ADD COLUMN extra statement, got %v", ddls)
			}

			db := openSeeded(t, current)
			execAll(t, db, ddls)
		})
	}
}

func TestQuoteCollidingKeywords_CreateIndex(t *testing.T) {
	in := `CREATE UNIQUE INDEX IF NOT EXISTS serial ON value (key DESC, name) WHERE key IS NOT NULL;`
	want := `CREATE UNIQUE INDEX IF NOT EXISTS "serial" ON "value" ("key" DESC, name) WHERE key IS NOT NULL;`
	if got := quoteCollidingKeywords(in); got != want {
		t.Fatalf("quoteCollidingKeywords(%q) = %q, want %q", in, got, want)
	}
}

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

// TestDiff_SQLiteOnlyExpressionOperators covers issue #53: sqldef's shared
// multi-dialect grammar rejects several valid SQLite operators
// (GLOB/MATCH/IS <expr>) in a CHECK/DEFAULT/GENERATED expression body,
// which used to fail generate outright for any additive change to a table
// containing one. Diff now masks a rejected body behind an opaque
// placeholder before calling sqldef and restores it afterward, so the
// additive change still succeeds — covering, across the five cases, each
// of a column-level CHECK, a table-level CHECK, a DEFAULT (…) and a
// GENERATED … AS (…).
func TestDiff_SQLiteOnlyExpressionOperators(t *testing.T) {
	cases := []struct {
		name    string
		current string
	}{
		{
			name:    "glob_column_check",
			current: `CREATE TABLE t (id INTEGER PRIMARY KEY, a TEXT CHECK (a GLOB '/x/*')) STRICT;`,
		},
		{
			name:    "not_glob_table_check",
			current: `CREATE TABLE t (id INTEGER PRIMARY KEY, a TEXT, CHECK (a NOT GLOB '*.tmp')) STRICT;`,
		},
		{
			name:    "match_default",
			current: `CREATE TABLE t (id INTEGER PRIMARY KEY, a TEXT DEFAULT ('foo' MATCH 'bar')) STRICT;`,
		},
		{
			name:    "is_generated",
			current: `CREATE TABLE t (id INTEGER PRIMARY KEY, a INTEGER, b INTEGER GENERATED ALWAYS AS (a IS 5) VIRTUAL) STRICT;`,
		},
		{
			name:    "is_not_column_check",
			current: `CREATE TABLE t (id INTEGER PRIMARY KEY, a TEXT CHECK (a IS NOT 'x')) STRICT;`,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			desired := strings.Replace(c.current, ") STRICT;", ", extra TEXT) STRICT;", 1)
			if desired == c.current {
				t.Fatalf("test fixture bug: no ') STRICT;' to replace in %q", c.current)
			}

			ddls, err := New().Diff(desired, c.current)
			if err != nil {
				t.Fatalf("Diff: %v", err)
			}
			if len(ddls) != 1 || !strings.Contains(ddls[0], "ADD COLUMN extra") {
				t.Fatalf("want a single ADD COLUMN extra statement, got %v", ddls)
			}

			db := openSeeded(t, c.current)
			execAll(t, db, ddls)
		})
	}
}

// TestDiff_AddedColumnCarriesSQLiteOnlyExpression covers the added column
// itself carrying a rejected expression: the placeholder minted for its
// CHECK body must be restored byte-for-byte in the ADD COLUMN statement
// sqldef emits, with no placeholder left in the output.
func TestDiff_AddedColumnCarriesSQLiteOnlyExpression(t *testing.T) {
	current := `CREATE TABLE t (id INTEGER PRIMARY KEY) STRICT;`
	desired := `CREATE TABLE t (id INTEGER PRIMARY KEY, b TEXT CHECK (b GLOB '*.img')) STRICT;`

	ddls, err := New().Diff(desired, current)
	if err != nil {
		t.Fatalf("Diff: %v", err)
	}
	want := `ALTER TABLE t ADD COLUMN b text CHECK (b GLOB '*.img')`
	if len(ddls) != 1 || ddls[0] != want {
		t.Fatalf("ddls = %v, want [%q]", ddls, want)
	}
	if strings.Contains(ddls[0], "sqlite_migrate_expr") {
		t.Fatalf("expected no placeholder left in the output, got %q", ddls[0])
	}

	db := openSeeded(t, current)
	execAll(t, db, ddls)
}

// TestDiff_AcceptedExpressionNotMasked is a direct check on exprMasker,
// independent of what sqldef then does with it: an expression sqldef's
// own parser already accepts (scenario 03's shape) passes through
// mask unchanged, never replaced by a placeholder.
func TestDiff_AcceptedExpressionNotMasked(t *testing.T) {
	in := `CREATE TABLE t (id INTEGER PRIMARY KEY, age INTEGER CHECK (age >= 1)) STRICT;`
	m := newExprMasker(in)
	if got := m.mask(in); got != in {
		t.Fatalf("mask(%q) = %q, want it unchanged", in, got)
	}
}

// TestExprMasker_UnmaskErrorsOnUnresolvedPlaceholder covers the second
// layer guarding a placeholder from ever reaching a migration file: if a
// bare identifier shaped like this masker's placeholders shows up in
// generated DDL without a matching minted body, unmask refuses it outright
// instead of handing back a statement carrying it.
func TestExprMasker_UnmaskErrorsOnUnresolvedPlaceholder(t *testing.T) {
	m := newExprMasker("")
	_, err := m.unmask([]string{fmt.Sprintf("ALTER TABLE t ADD COLUMN b TEXT CHECK (%s_99)", m.nonce)})
	if err == nil {
		t.Fatalf("want an error for an unresolved placeholder")
	}
	if !strings.Contains(err.Error(), "placeholder") {
		t.Fatalf("want the error to name the placeholder, got: %v", err)
	}
}
