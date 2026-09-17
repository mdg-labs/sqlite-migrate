package rebuild

// Phase 8 fuzz targets for the rebuild generator's own hand-rolled
// text-scanning layer, per the spec doc's Testing & Verification Strategy:
// renameCreateTableSQL, hasAutoincrement, and referencesTable never
// reconstruct SQL from parsed fields, so a bug in any of them silently
// misgenerates (or fails to protect) live table data during a rebuild.
// Every target here is run with a bounded -fuzztime (see the phase's
// report), never left running unbounded.

import (
	"context"
	"database/sql"
	"strings"
	"testing"

	"github.com/mdg-labs/sqlite-migrate/internal/schemadiff"

	_ "modernc.org/sqlite"
)

// FuzzRenameCreateTableSQLRoundTrip is renameCreateTableSQL's core safety
// property, fuzzed directly: for any CREATE TABLE text SQLite itself
// accepted (so schemadiff.Parse could read it back from sqlite_master),
// renaming it must produce SQL that (a) SQLite itself still accepts and
// (b) still declares every one of the original table's columns, in order,
// under the new name — the exact guarantee generator.go's rebuild step
// depends on to carry a table's real structure into its renamed
// replacement. A byte-offset bug in the hand-written CREATE/TABLE/IF NOT
// EXISTS/identifier scanner ahead of the column list is exactly the class
// of bug this catches.
func FuzzRenameCreateTableSQLRoundTrip(f *testing.F) {
	for _, seed := range renameRoundTripSeeds {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, ddl string) {
		ctx := context.Background()
		before, err := schemadiff.Parse(ctx, ddl)
		if err != nil || len(before.Tables) != 1 {
			return
		}
		var tbl *schemadiff.Table
		for _, tb := range before.Tables {
			tbl = tb
		}
		if tbl.Virtual {
			return // CREATE VIRTUAL TABLE isn't renameCreateTableSQL's grammar
		}

		renamed, err := renameCreateTableSQL(tbl.SQL, "zzz_fuzz_new")
		if err != nil {
			t.Fatalf("renameCreateTableSQL failed on SQLite-parsed CREATE TABLE text %q: %v", tbl.SQL, err)
		}

		after, err := schemadiff.Parse(ctx, renamed)
		if err != nil {
			t.Fatalf("renameCreateTableSQL(%q) produced unparseable SQL %q: %v", tbl.SQL, renamed, err)
		}
		newTbl, ok := after.Tables["zzz_fuzz_new"]
		if !ok {
			t.Fatalf("renameCreateTableSQL(%q) = %q, want a table named zzz_fuzz_new", tbl.SQL, renamed)
		}
		if len(newTbl.Columns) != len(tbl.Columns) {
			t.Fatalf("renameCreateTableSQL(%q) = %q, got %d columns, want %d", tbl.SQL, renamed, len(newTbl.Columns), len(tbl.Columns))
		}
		for i, c := range tbl.Columns {
			nc := newTbl.Columns[i]
			if asciiLower(nc.Name) != asciiLower(c.Name) || nc.Type != c.Type {
				t.Fatalf("renameCreateTableSQL(%q) = %q, column %d = %+v, want name/type matching %+v", tbl.SQL, renamed, i, nc, c)
			}
		}
	})
}

var renameRoundTripSeeds = []string{
	"CREATE TABLE users (id INTEGER PRIMARY KEY) STRICT",
	`CREATE TABLE "users" (id INTEGER PRIMARY KEY) STRICT`,
	`CREATE TABLE IF NOT EXISTS users (id INTEGER PRIMARY KEY) STRICT`,
	"create table users (id integer primary key) strict",
	"CREATE TEMP TABLE users (id INTEGER PRIMARY KEY) STRICT",
	"CREATE TABLE bücher (id INTEGER PRIMARY KEY) STRICT",
	`CREATE TABLE "weird""quote" (id INTEGER PRIMARY KEY) STRICT`,
	"CREATE TABLE `backtick_tbl` (id INTEGER PRIMARY KEY) STRICT",
	"CREATE TABLE [bracket_tbl] (id INTEGER PRIMARY KEY) STRICT",
	"CREATE TABLE t ( -- comment right after the paren\n id INTEGER PRIMARY KEY, name TEXT /* block */ ) STRICT",
	"CREATE TABLE t (id INTEGER PRIMARY KEY, v TEXT CHECK (v <> 'CREATE TABLE')) STRICT",
}

// FuzzHasAutoincrementMatchesSQLite checks hasAutoincrement against ground
// truth: SQLite itself creates the sqlite_sequence bookkeeping table if and
// only if some table it just created really does declare
// INTEGER PRIMARY KEY AUTOINCREMENT — a keyword restrictive enough that it
// can't appear as a bare unquoted identifier elsewhere in valid SQL
// (verified directly: SQLite rejects autoincrement as a bare column name).
// So for any ddl SQLite accepts, hasAutoincrement's verbatim-text scan and
// sqlite_sequence's presence must agree.
func FuzzHasAutoincrementMatchesSQLite(f *testing.F) {
	for _, seed := range hasAutoincrementFuzzSeeds {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, ddl string) {
		// A NUL byte makes this oracle itself unsound: db.Exec below
		// silently stops at the first one with no error (the real bug
		// loadCatalog/schemadiff.Parse now refuse upstream — see their
		// NUL-byte guards), while hasAutoincrement keeps scanning the
		// full Go string past it. hasAutoincrement is only ever called on
		// text already read back from sqlite_master via one of those two
		// guarded replays, so a NUL byte reaching it is exactly the
		// "cannot happen" case their guards exist to rule out.
		if strings.IndexByte(ddl, 0) >= 0 {
			return
		}

		got := hasAutoincrement(ddl)

		db, err := sql.Open("sqlite", ":memory:")
		if err != nil {
			t.Fatalf("open: %v", err)
		}
		defer func() { _ = db.Close() }()
		db.SetMaxOpenConns(1)

		if _, err := db.Exec(ddl); err != nil {
			return
		}

		var count int
		if err := db.QueryRow(`SELECT count(*) FROM sqlite_master WHERE type = 'table' AND name = 'sqlite_sequence'`).Scan(&count); err != nil {
			t.Fatalf("query sqlite_sequence: %v", err)
		}
		want := count > 0
		if got != want {
			t.Fatalf("hasAutoincrement(%q) = %v, want %v (sqlite_sequence present = %v)", ddl, got, want, want)
		}
	})
}

var hasAutoincrementFuzzSeeds = []string{
	"CREATE TABLE t (id INTEGER PRIMARY KEY AUTOINCREMENT) STRICT",
	"create table t (id integer primary key autoincrement) strict",
	"CREATE TABLE t (id INTEGER PRIMARY KEY) STRICT",
	"CREATE TABLE t (id INTEGER PRIMARY KEY, mode TEXT CHECK (mode IN ('autoincrement','manual'))) STRICT",
	`CREATE TABLE t (id INTEGER PRIMARY KEY, "autoincrement" TEXT) STRICT`,
	"CREATE TABLE t (id INTEGER PRIMARY KEY, `autoincrement` TEXT) STRICT",
	"CREATE TABLE t (id INTEGER PRIMARY KEY, [autoincrement] TEXT) STRICT",
	"CREATE TABLE t (\n  id INTEGER PRIMARY KEY, -- not AUTOINCREMENT\n  v TEXT\n) STRICT",
	"CREATE TABLE t (id INTEGER PRIMARY KEY /* not AUTOINCREMENT */, v TEXT) STRICT",
	"CREATE TABLE café (id INTEGER PRIMARY KEY AUTOINCREMENT) STRICT",
	"CREATE TABLE t (id INTEGER PRIMARY KEY) STRICT; CREATE TABLE u (id INTEGER PRIMARY KEY AUTOINCREMENT) STRICT;",
}

// FuzzReferencesTableNoPanic is a crash-safety fuzzer for referencesTable
// and the containsIdentifierWord/word-boundary scan underneath it, seeded
// with the quoting styles and comment placements its own table-driven
// tests already assert specific outcomes for.
func FuzzReferencesTableNoPanic(f *testing.F) {
	for _, seed := range referencesTableFuzzSeeds {
		f.Add(seed, "items")
	}
	f.Fuzz(func(t *testing.T, sql, table string) {
		_ = referencesTable(sql, table)
	})
}

var referencesTableFuzzSeeds = []string{
	"SELECT id FROM items",
	`SELECT id FROM "items"`,
	"SELECT id FROM items_audit",
	"SELECT id FROM other",
	`SELECT * FROM "a""b"`,
	"SELECT * FROM `a``b`",
	"SELECT * FROM [a\"b]",
	"-- items\nSELECT 1",
	"/* items */ SELECT 1",
	"CREATE TRIGGER trg AFTER UPDATE ON items BEGIN\n  INSERT INTO log SELECT CASE WHEN NEW.id > 0 THEN NEW.id ELSE 0 END;\nEND",
}
