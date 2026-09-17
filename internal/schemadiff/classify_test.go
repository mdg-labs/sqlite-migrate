package schemadiff

import "testing"

// These tests bias heavily toward the destructive side: a false Safe
// verdict here is the failure mode that would let generate silently
// produce a migration that drops real data without the destructive-change
// gate ever firing.

func TestClassify_Safe_NewTableAndColumn(t *testing.T) {
	before := mustParse(t, `CREATE TABLE users (id INTEGER PRIMARY KEY) STRICT;`)
	after := mustParse(t, `
		CREATE TABLE users (id INTEGER PRIMARY KEY, nickname TEXT) STRICT;
		CREATE TABLE orders (id INTEGER PRIMARY KEY) STRICT;
	`)

	c := Classify(Diff(before, after))
	if c.Verdict != Safe {
		t.Fatalf("want Safe, got %v (%+v)", c.Verdict, c)
	}
	if len(c.RemovedTables) != 0 || len(c.RemovedColumns) != 0 {
		t.Errorf("want no removed tables/columns recorded, got %+v", c)
	}
}

func TestClassify_Safe_RebuildRequiringChangesStayGaslighted(t *testing.T) {
	// Type change, CHECK addition, UNIQUE addition, and an FK action change
	// each force a full rebuild internally (create/copy/drop/rename) but
	// lose no data — none of that internal DROP TABLE should ever flip the
	// verdict to Destructive.
	before := mustParse(t, `
		CREATE TABLE users (
			id INTEGER PRIMARY KEY,
			age TEXT,
			email TEXT NOT NULL
		) STRICT;
	`)
	after := mustParse(t, `
		CREATE TABLE users (
			id INTEGER PRIMARY KEY,
			age INTEGER CHECK (age >= 0),
			email TEXT NOT NULL UNIQUE
		) STRICT;
	`)

	c := Classify(Diff(before, after))
	if c.Verdict != Safe {
		t.Fatalf("want Safe for a rebuild-only change, got %v (%+v)", c.Verdict, c)
	}
}

func TestClassify_Destructive_ColumnDropped(t *testing.T) {
	before := mustParse(t, `CREATE TABLE users (id INTEGER PRIMARY KEY, ssn TEXT) STRICT;`)
	after := mustParse(t, `CREATE TABLE users (id INTEGER PRIMARY KEY) STRICT;`)

	c := Classify(Diff(before, after))
	if c.Verdict != Destructive {
		t.Fatalf("want Destructive, got %v", c.Verdict)
	}
	if got := c.RemovedColumns["users"]; len(got) != 1 || got[0] != "ssn" {
		t.Fatalf("want RemovedColumns[users]=[ssn], got %+v", c.RemovedColumns)
	}
	if len(c.RemovedTables) != 0 {
		t.Errorf("want no whole tables reported removed, got %+v", c.RemovedTables)
	}
}

func TestClassify_Destructive_TableDropped(t *testing.T) {
	before := mustParse(t, `
		CREATE TABLE users (id INTEGER PRIMARY KEY) STRICT;
		CREATE TABLE sessions (id INTEGER PRIMARY KEY, token TEXT) STRICT;
	`)
	after := mustParse(t, `CREATE TABLE users (id INTEGER PRIMARY KEY) STRICT;`)

	c := Classify(Diff(before, after))
	if c.Verdict != Destructive {
		t.Fatalf("want Destructive, got %v", c.Verdict)
	}
	if len(c.RemovedTables) != 1 || c.RemovedTables[0] != "sessions" {
		t.Fatalf("want RemovedTables=[sessions], got %+v", c.RemovedTables)
	}
	if len(c.RemovedColumns) != 0 {
		t.Errorf("want a dropped table's columns not double-reported as RemovedColumns, got %+v", c.RemovedColumns)
	}
}

func TestClassify_Destructive_ColumnDropAndAddInSameTable(t *testing.T) {
	// A column drop plus an unrelated add in the same table can look like a
	// rename to a naive heuristic, but Phase 1 has no rename detection: a
	// genuinely dropped column must still gate as destructive regardless of
	// what else is added alongside it in the same diff.
	before := mustParse(t, `CREATE TABLE users (id INTEGER PRIMARY KEY, legacy_name TEXT) STRICT;`)
	after := mustParse(t, `CREATE TABLE users (id INTEGER PRIMARY KEY, display_name TEXT) STRICT;`)

	c := Classify(Diff(before, after))
	if c.Verdict != Destructive {
		t.Fatalf("want Destructive for drop+add treated as unrelated columns, got %v", c.Verdict)
	}
	if got := c.RemovedColumns["users"]; len(got) != 1 || got[0] != "legacy_name" {
		t.Fatalf("want RemovedColumns[users]=[legacy_name], got %+v", c.RemovedColumns)
	}
}

func TestClassify_Destructive_MixedWithSafeChangesInSameDiff(t *testing.T) {
	// A destructive change to one table must not be masked by unrelated
	// safe changes (new table, rebuild-only change) elsewhere in the same
	// diff, and the safe changes must still be reported accurately.
	before := mustParse(t, `
		CREATE TABLE users (id INTEGER PRIMARY KEY, ssn TEXT, age TEXT) STRICT;
		CREATE TABLE sessions (id INTEGER PRIMARY KEY, token TEXT) STRICT;
	`)
	after := mustParse(t, `
		CREATE TABLE users (id INTEGER PRIMARY KEY, age INTEGER) STRICT;
		CREATE TABLE orders (id INTEGER PRIMARY KEY) STRICT;
	`)

	d := Diff(before, after)
	c := Classify(d)

	if c.Verdict != Destructive {
		t.Fatalf("want Destructive, got %v", c.Verdict)
	}
	if len(d.AddedTables) != 1 || d.AddedTables[0].Name != "orders" {
		t.Errorf("want the safe new table still reported, got %+v", d.AddedTables)
	}
	if len(c.RemovedTables) != 1 || c.RemovedTables[0] != "sessions" {
		t.Fatalf("want RemovedTables=[sessions], got %+v", c.RemovedTables)
	}
	if got := c.RemovedColumns["users"]; len(got) != 1 || got[0] != "ssn" {
		t.Fatalf("want RemovedColumns[users]=[ssn], got %+v", c.RemovedColumns)
	}
	users := findTableDiff(t, d, "users")
	if len(users.ChangedColumns) != 1 || users.ChangedColumns[0].Name != "age" {
		t.Errorf("want the safe age type-change still reported on users, got %+v", users)
	}
}

func TestClassify_Destructive_MultipleTablesAndColumnsDropped(t *testing.T) {
	before := mustParse(t, `
		CREATE TABLE users (id INTEGER PRIMARY KEY, ssn TEXT, notes TEXT) STRICT;
		CREATE TABLE sessions (id INTEGER PRIMARY KEY) STRICT;
		CREATE TABLE audit_log (id INTEGER PRIMARY KEY) STRICT;
	`)
	after := mustParse(t, `CREATE TABLE users (id INTEGER PRIMARY KEY) STRICT;`)

	c := Classify(Diff(before, after))
	if c.Verdict != Destructive {
		t.Fatalf("want Destructive, got %v", c.Verdict)
	}
	if len(c.RemovedTables) != 2 || c.RemovedTables[0] != "audit_log" || c.RemovedTables[1] != "sessions" {
		t.Fatalf("want RemovedTables=[audit_log sessions] (sorted), got %+v", c.RemovedTables)
	}
	if got := c.RemovedColumns["users"]; len(got) != 2 || got[0] != "notes" || got[1] != "ssn" {
		t.Fatalf("want RemovedColumns[users]=[notes ssn] (sorted), got %+v", c.RemovedColumns)
	}
}

func TestClassify_Destructive_TableNameResemblingSQLiteInternal(t *testing.T) {
	// "sqlite3_stats" and "sqliteusers" both match a naive LIKE
	// 'sqlite_%' filter (LIKE's "_" wildcard matches any character), which
	// would silently hide them from Parse and turn their removal into an
	// empty, falsely Safe diff.
	before := mustParse(t, `
		CREATE TABLE users (id INTEGER PRIMARY KEY) STRICT;
		CREATE TABLE sqlite3_stats (id INTEGER PRIMARY KEY) STRICT;
		CREATE TABLE sqliteusers (id INTEGER PRIMARY KEY, note TEXT) STRICT;
	`)
	after := mustParse(t, `
		CREATE TABLE users (id INTEGER PRIMARY KEY) STRICT;
		CREATE TABLE sqliteusers (id INTEGER PRIMARY KEY) STRICT;
	`)

	c := Classify(Diff(before, after))
	if c.Verdict != Destructive {
		t.Fatalf("want Destructive, got %v", c.Verdict)
	}
	if len(c.RemovedTables) != 1 || c.RemovedTables[0] != "sqlite3_stats" {
		t.Fatalf("want RemovedTables=[sqlite3_stats], got %+v", c.RemovedTables)
	}
	if got := c.RemovedColumns["sqliteusers"]; len(got) != 1 || got[0] != "note" {
		t.Fatalf("want RemovedColumns[sqliteusers]=[note], got %+v", c.RemovedColumns)
	}
}

func TestClassify_Verdict_String(t *testing.T) {
	if Safe.String() != "safe" {
		t.Errorf("want Safe.String() = safe, got %q", Safe.String())
	}
	if Destructive.String() != "destructive" {
		t.Errorf("want Destructive.String() = destructive, got %q", Destructive.String())
	}
}
