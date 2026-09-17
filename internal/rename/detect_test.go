package rename

import (
	"context"
	"testing"

	"github.com/mdg-labs/sqlite-migrate/internal/schemadiff"
)

func mustParse(t *testing.T, ddl string) *schemadiff.Schema {
	t.Helper()
	s, err := schemadiff.Parse(context.Background(), ddl)
	if err != nil {
		t.Fatalf("Parse failed: %v\nDDL:\n%s", err, ddl)
	}
	return s
}

func diffOf(t *testing.T, before, after string) *schemadiff.SchemaDiff {
	t.Helper()
	return schemadiff.Diff(mustParse(t, before), mustParse(t, after))
}

// Scenario 14 (testdata matrix): a column renamed, matching type and
// structural position — the canonical true-rename case from the spec's
// worked example.
func TestDetect_ColumnRename_TrueRename(t *testing.T) {
	d := diffOf(t, `
		CREATE TABLE orders (
			id INTEGER PRIMARY KEY,
			customer_name TEXT NOT NULL,
			total INTEGER NOT NULL
		) STRICT;
	`, `
		CREATE TABLE orders (
			id INTEGER PRIMARY KEY,
			buyer_name TEXT NOT NULL,
			total INTEGER NOT NULL
		) STRICT;
	`)

	got := Detect(d)
	want := []Candidate{{Kind: ColumnRename, Table: "orders", From: "customer_name", To: "buyer_name"}}
	assertCandidates(t, got, want)
}

// Scenario 15: a whole table renamed, column structure unchanged.
func TestDetect_TableRename_TrueRename(t *testing.T) {
	d := diffOf(t, `
		CREATE TABLE orders (
			id INTEGER PRIMARY KEY,
			total INTEGER NOT NULL
		) STRICT;
	`, `
		CREATE TABLE purchases (
			id INTEGER PRIMARY KEY,
			total INTEGER NOT NULL
		) STRICT;
	`)

	got := Detect(d)
	want := []Candidate{{Kind: TableRename, From: "orders", To: "purchases"}}
	assertCandidates(t, got, want)
}

// A dropped column and an added column that happen to share a type, but
// are otherwise unrelated, is exactly the false-positive case the heuristic
// can't rule out on its own (per the spec's Rename Handling section) — it
// is still flagged as a candidate here; TestConfirm and TestResolve cover
// it being correctly resolved by the flag/prompt answer instead.
func TestDetect_ColumnRename_FalsePositiveStillFlagged(t *testing.T) {
	d := diffOf(t, `
		CREATE TABLE users (
			id INTEGER PRIMARY KEY,
			ssn TEXT
		) STRICT;
	`, `
		CREATE TABLE users (
			id INTEGER PRIMARY KEY,
			last_login TEXT
		) STRICT;
	`)

	got := Detect(d)
	want := []Candidate{{Kind: ColumnRename, Table: "users", From: "ssn", To: "last_login"}}
	assertCandidates(t, got, want)
}

// A dropped column and an added column with incompatible types is never a
// rename candidate — type compatibility is a hard requirement, not a
// scored signal.
func TestDetect_ColumnRename_TypeMismatchNotFlagged(t *testing.T) {
	d := diffOf(t, `
		CREATE TABLE users (
			id INTEGER PRIMARY KEY,
			customer_name TEXT
		) STRICT;
	`, `
		CREATE TABLE users (
			id INTEGER PRIMARY KEY,
			customer_count INTEGER
		) STRICT;
	`)

	got := Detect(d)
	assertCandidates(t, got, nil)
}

// Scenario 16: two unrelated changes in one diff (a new column on one
// table, a type change on another) touch no removed+added pair at all, so
// nothing should ever be flagged.
func TestDetect_NoRemovedPlusAdded_NoCandidates(t *testing.T) {
	d := diffOf(t, `
		CREATE TABLE users (id INTEGER PRIMARY KEY, age TEXT) STRICT;
	`, `
		CREATE TABLE users (id INTEGER PRIMARY KEY, age INTEGER, nickname TEXT) STRICT;
	`)

	got := Detect(d)
	assertCandidates(t, got, nil)
}

// Multiple renames within one table are paired by matching structural
// position (their rank among removed/added columns in original order),
// not by whichever names happen to look similar.
func TestDetect_ColumnRename_MultiplePairedByPosition(t *testing.T) {
	d := diffOf(t, `
		CREATE TABLE orders (
			id INTEGER PRIMARY KEY,
			customer_name TEXT NOT NULL,
			order_total INTEGER NOT NULL
		) STRICT;
	`, `
		CREATE TABLE orders (
			id INTEGER PRIMARY KEY,
			buyer_name TEXT NOT NULL,
			grand_total INTEGER NOT NULL
		) STRICT;
	`)

	got := Detect(d)
	want := []Candidate{
		{Kind: ColumnRename, Table: "orders", From: "customer_name", To: "buyer_name"},
		{Kind: ColumnRename, Table: "orders", From: "order_total", To: "grand_total"},
	}
	assertCandidates(t, got, want)
}

// A table whose column layout is entirely different from any dropped
// table, and whose name isn't a close match either, is a genuine new
// table plus a genuine dropped table — not a rename candidate.
func TestDetect_TableRename_UnrelatedNotFlagged(t *testing.T) {
	d := diffOf(t, `
		CREATE TABLE sessions (
			id INTEGER PRIMARY KEY,
			token TEXT NOT NULL
		) STRICT;
	`, `
		CREATE TABLE audit_log (
			id INTEGER PRIMARY KEY,
			actor TEXT NOT NULL,
			action TEXT NOT NULL,
			happened_at INTEGER NOT NULL
		) STRICT;
	`)

	got := Detect(d)
	assertCandidates(t, got, nil)
}

// A high name-similarity match rescues a candidate whose structural
// position doesn't line up (a column renamed and also relocated past a
// newly added column), the "name-similarity heuristic" leg of detection.
func TestDetect_ColumnRename_NameSimilarityOverridesPosition(t *testing.T) {
	d := diffOf(t, `
		CREATE TABLE orders (
			id INTEGER PRIMARY KEY,
			customer_name TEXT NOT NULL
		) STRICT;
	`, `
		CREATE TABLE orders (
			id INTEGER PRIMARY KEY,
			priority INTEGER,
			customer_full_name TEXT NOT NULL
		) STRICT;
	`)

	got := Detect(d)
	want := []Candidate{{Kind: ColumnRename, Table: "orders", From: "customer_name", To: "customer_full_name"}}
	assertCandidates(t, got, want)
}

func assertCandidates(t *testing.T, got, want []Candidate) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("Detect: got %d candidates %+v, want %d %+v", len(got), got, len(want), want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("Detect[%d]: got %+v, want %+v", i, got[i], want[i])
		}
	}
}
