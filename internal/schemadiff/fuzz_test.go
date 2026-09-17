package schemadiff

// Phase 8 fuzz targets for the classifier's own layer, per the spec doc's
// Testing & Verification Strategy: internal/schemadiff's hand-rolled
// sqlTokens tokenizer and the structural Parse/Diff/Classify pipeline built
// on top of it are exactly where a Hoserva-style tokenizer bug hides. Every
// target here is run with a bounded -fuzztime (see the phase's report),
// never left running unbounded.

import (
	"context"
	"testing"
)

// FuzzParseNoPanic feeds arbitrary bytes to Parse as a schema.sql source.
// Parse must never panic on malformed input — SQLite itself is expected to
// reject nonsense text with an ordinary error, which Parse should simply
// propagate — regardless of quoting, comments, or keyword case.
func FuzzParseNoPanic(f *testing.F) {
	for _, seed := range fuzzSeedSchemas {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, schemaSQL string) {
		_, _ = Parse(context.Background(), schemaSQL)
	})
}

// FuzzSQLTokensNoPanic feeds arbitrary bytes directly at the tokenizer
// normalizeSQL is built on. sqlTokens (and normalizeSQL, which calls it and
// then foldIdentifierPositions) must never panic on arbitrary input — an
// empty quoted identifier (a double-quoted, backtick-quoted, or
// bracket-quoted empty string) is a legitimate, if unusual, zero-length
// token, so there is no fixed shape to assert beyond that.
func FuzzSQLTokensNoPanic(f *testing.F) {
	for _, seed := range fuzzSeedSchemas {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, sql string) {
		_ = sqlTokens(sql)
		_ = normalizeSQL(sql)
	})
}

// FuzzDiffClassifyInvariants is the classifier's core safety property,
// fuzzed directly: Classify must report Destructive, naming exactly the
// right table or column, whenever a table or column genuinely present
// before is genuinely absent after — regardless of what quoting, comments,
// case, or unrelated structure surrounds it. A miss here is precisely the
// failure mode that would let generate silently produce a migration that
// drops real data without the destructive-change gate ever firing.
func FuzzDiffClassifyInvariants(f *testing.F) {
	for _, seed := range fuzzSeedPairs {
		f.Add(seed[0], seed[1])
	}
	f.Fuzz(func(t *testing.T, before, after string) {
		ctx := context.Background()
		b, err := Parse(ctx, before)
		if err != nil {
			return
		}
		a, err := Parse(ctx, after)
		if err != nil {
			return
		}

		d := Diff(b, a)
		c := Classify(d)

		afterKeys := make(map[string]bool, len(a.Tables))
		for name := range a.Tables {
			afterKeys[asciiLower(name)] = true
		}

		for name, bt := range b.Tables {
			key := asciiLower(name)
			if !afterKeys[key] {
				requireDestructiveTable(t, c, before, after, bt.Name)
				continue
			}
			var at *Table
			for n2, t2 := range a.Tables {
				if asciiLower(n2) == key {
					at = t2
					break
				}
			}
			for _, col := range bt.Columns {
				if _, ok := at.Column(col.Name); ok {
					continue
				}
				stillThere := false
				for _, ac := range at.Columns {
					if asciiLower(ac.Name) == asciiLower(col.Name) {
						stillThere = true
						break
					}
				}
				if stillThere {
					continue
				}
				requireDestructiveColumn(t, c, before, after, bt.Name, col.Name)
			}
		}
	})
}

func requireDestructiveTable(t *testing.T, c Classification, before, after, table string) {
	t.Helper()
	if c.Verdict != Destructive {
		t.Fatalf("table %q dropped (before=%q after=%q) but Classify said %v", table, before, after, c.Verdict)
	}
	for _, rt := range c.RemovedTables {
		if asciiLower(rt) == asciiLower(table) {
			return
		}
	}
	t.Fatalf("table %q dropped (before=%q after=%q) but missing from RemovedTables=%v", table, before, after, c.RemovedTables)
}

func requireDestructiveColumn(t *testing.T, c Classification, before, after, table, column string) {
	t.Helper()
	if c.Verdict != Destructive {
		t.Fatalf("column %q dropped from table %q (before=%q after=%q) but Classify said %v", column, table, before, after, c.Verdict)
	}
	for name, cols := range c.RemovedColumns {
		if asciiLower(name) != asciiLower(table) {
			continue
		}
		for _, rc := range cols {
			if asciiLower(rc) == asciiLower(column) {
				return
			}
		}
	}
	t.Fatalf("column %q dropped from table %q (before=%q after=%q) but missing from RemovedColumns=%v", column, table, before, after, c.RemovedColumns)
}

// fuzzSeedSchemas seeds the tokenizer/parser fuzzers with the exact edge
// cases the phase's Deliverable names: quoted identifiers (three quoting
// styles), SQL comments (line and block, adjacent to identifiers), a
// trigger with a nested BEGIN...END-shaped construct (CASE...END inside a
// trigger body, which is not itself a statement terminator), and
// case-insensitive keyword matching.
var fuzzSeedSchemas = []string{
	`CREATE TABLE "My Table" ("Col One" INTEGER, "quote""inside" TEXT) STRICT;`,
	"CREATE TABLE `backtick_tbl` (`col` INTEGER) STRICT;",
	`CREATE TABLE [bracket_tbl] ([col one] INTEGER) STRICT;`,
	`-- leading comment
	CREATE TABLE t ( -- trailing comment on id
		id INTEGER PRIMARY KEY, /* block comment */ name TEXT
	) STRICT;`,
	`create TABLE Foo ( Id integer PRIMARY key, Name text NOT null ) STRICT;`,
	`CREATE temp TABLE IF NOT EXISTS t (id INTEGER PRIMARY KEY) STRICT;`,
	`CREATE TABLE t (id INTEGER PRIMARY KEY) STRICT;
	CREATE TRIGGER trg AFTER UPDATE ON t BEGIN
		INSERT INTO t (id) SELECT CASE WHEN NEW.id > 0 THEN NEW.id ELSE 0 END;
	END;`,
	`CREATE TABLE café (naïve INTEGER, 日本語 TEXT) STRICT;`,
	`CREATE VIRTUAL TABLE docs USING fts5(body);`,
}

var fuzzSeedPairs = [][2]string{
	{
		`CREATE TABLE users (id INTEGER PRIMARY KEY, ssn TEXT) STRICT;`,
		`CREATE TABLE users (id INTEGER PRIMARY KEY) STRICT;`,
	},
	{
		`CREATE TABLE "Users" (id INTEGER PRIMARY KEY, "SSN" TEXT) STRICT;`,
		`CREATE TABLE users (id INTEGER PRIMARY KEY) STRICT;`,
	},
	{
		`CREATE TABLE t (id INTEGER PRIMARY KEY) STRICT;
		CREATE TABLE sessions (id INTEGER PRIMARY KEY) STRICT;`,
		`create TABLE T (id integer primary key) strict;`,
	},
	{
		`CREATE TABLE café (naïve INTEGER) STRICT;`,
		`CREATE TABLE café (naïve INTEGER CHECK (naïve >= 0)) STRICT;`,
	},
	{
		`CREATE TABLE t ( -- comment
			id INTEGER PRIMARY KEY, name TEXT
		) STRICT;`,
		`CREATE TABLE t (id INTEGER PRIMARY KEY) STRICT;`,
	},
}
