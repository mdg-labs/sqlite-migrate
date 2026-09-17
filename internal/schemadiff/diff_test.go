package schemadiff

// Scenario numbers in test names refer to the Testing & Verification
// Strategy scenario matrix in the spec doc.

import "testing"

func findTableDiff(t *testing.T, d *SchemaDiff, name string) TableDiff {
	t.Helper()
	for _, td := range d.ChangedTables {
		if td.Name == name {
			return td
		}
	}
	t.Fatalf("no TableDiff for %q in %+v", name, d.ChangedTables)
	return TableDiff{}
}

func TestDiff_01_NewTable(t *testing.T) {
	before := mustParse(t, `CREATE TABLE users (id INTEGER PRIMARY KEY) STRICT;`)
	after := mustParse(t, `
		CREATE TABLE users (id INTEGER PRIMARY KEY) STRICT;
		CREATE TABLE orders (id INTEGER PRIMARY KEY) STRICT;
	`)

	d := Diff(before, after)
	if len(d.AddedTables) != 1 || d.AddedTables[0].Name != "orders" {
		t.Fatalf("want AddedTables=[orders], got %+v", d.AddedTables)
	}
	if len(d.RemovedTables) != 0 || len(d.ChangedTables) != 0 {
		t.Fatalf("want no removed/changed tables, got %+v", d)
	}
}

func TestDiff_02_NewColumnNoConstraints(t *testing.T) {
	before := mustParse(t, `CREATE TABLE users (id INTEGER PRIMARY KEY) STRICT;`)
	after := mustParse(t, `CREATE TABLE users (id INTEGER PRIMARY KEY, nickname TEXT) STRICT;`)

	td := findTableDiff(t, Diff(before, after), "users")
	if len(td.AddedColumns) != 1 || td.AddedColumns[0].Name != "nickname" {
		t.Fatalf("want AddedColumns=[nickname], got %+v", td.AddedColumns)
	}
	if td.AddedColumns[0].NotNull || td.AddedColumns[0].HasDefault {
		t.Errorf("want nickname to have no constraints, got %+v", td.AddedColumns[0])
	}
}

func TestDiff_03_NewColumnNotNullDefault(t *testing.T) {
	before := mustParse(t, `CREATE TABLE users (id INTEGER PRIMARY KEY) STRICT;`)
	after := mustParse(t, `CREATE TABLE users (id INTEGER PRIMARY KEY, plan TEXT NOT NULL DEFAULT 'free') STRICT;`)

	td := findTableDiff(t, Diff(before, after), "users")
	if len(td.AddedColumns) != 1 {
		t.Fatalf("want 1 added column, got %+v", td.AddedColumns)
	}
	col := td.AddedColumns[0]
	if !col.NotNull || !col.HasDefault || col.DefaultValue != "'free'" {
		t.Errorf("want plan NOT NULL DEFAULT 'free', got %+v", col)
	}
}

func TestDiff_04_NewColumnWithReferences(t *testing.T) {
	before := mustParse(t, `
		CREATE TABLE users (id INTEGER PRIMARY KEY) STRICT;
		CREATE TABLE orders (id INTEGER PRIMARY KEY) STRICT;
	`)
	after := mustParse(t, `
		CREATE TABLE users (id INTEGER PRIMARY KEY) STRICT;
		CREATE TABLE orders (id INTEGER PRIMARY KEY, user_id INTEGER REFERENCES users(id)) STRICT;
	`)

	td := findTableDiff(t, Diff(before, after), "orders")
	if len(td.AddedColumns) != 1 || td.AddedColumns[0].Name != "user_id" {
		t.Fatalf("want AddedColumns=[user_id], got %+v", td.AddedColumns)
	}
	if len(td.AddedForeignKeys) != 1 || td.AddedForeignKeys[0].Table != "users" {
		t.Fatalf("want AddedForeignKeys pointing at users, got %+v", td.AddedForeignKeys)
	}
}

func TestDiff_06_NewIndex(t *testing.T) {
	before := mustParse(t, `CREATE TABLE users (id INTEGER PRIMARY KEY, email TEXT NOT NULL) STRICT;`)
	after := mustParse(t, `
		CREATE TABLE users (id INTEGER PRIMARY KEY, email TEXT NOT NULL) STRICT;
		CREATE INDEX idx_users_email ON users(email);
	`)

	td := findTableDiff(t, Diff(before, after), "users")
	if len(td.AddedIndexes) != 1 || td.AddedIndexes[0].Name != "idx_users_email" {
		t.Fatalf("want AddedIndexes=[idx_users_email], got %+v", td.AddedIndexes)
	}
	if len(td.AddedColumns) != 0 || len(td.RemovedColumns) != 0 {
		t.Errorf("want no column changes, got %+v", td)
	}
}

func TestDiff_07_ColumnTypeChanged(t *testing.T) {
	before := mustParse(t, `CREATE TABLE users (id INTEGER PRIMARY KEY, age TEXT) STRICT;`)
	after := mustParse(t, `CREATE TABLE users (id INTEGER PRIMARY KEY, age INTEGER) STRICT;`)

	td := findTableDiff(t, Diff(before, after), "users")
	if len(td.ChangedColumns) != 1 || td.ChangedColumns[0].Name != "age" {
		t.Fatalf("want ChangedColumns=[age], got %+v", td.ChangedColumns)
	}
	cd := td.ChangedColumns[0]
	if cd.Before.Type != "TEXT" || cd.After.Type != "INTEGER" {
		t.Errorf("want TEXT -> INTEGER, got %+v", cd)
	}
	if len(td.AddedColumns) != 0 || len(td.RemovedColumns) != 0 {
		t.Errorf("want no columns added/removed, got %+v", td)
	}
}

func TestDiff_08_CheckConstraintAdded(t *testing.T) {
	before := mustParse(t, `CREATE TABLE users (id INTEGER PRIMARY KEY, age INTEGER) STRICT;`)
	after := mustParse(t, `CREATE TABLE users (id INTEGER PRIMARY KEY, age INTEGER CHECK (age >= 0)) STRICT;`)

	td := findTableDiff(t, Diff(before, after), "users")
	if !td.SQLChanged {
		t.Fatal("want SQLChanged=true for an added CHECK constraint")
	}
	if len(td.AddedColumns) != 0 || len(td.RemovedColumns) != 0 || len(td.ChangedColumns) != 0 {
		t.Errorf("want no column-level diff for a CHECK addition, got %+v", td)
	}
}

func TestDiff_09_UniqueConstraintAdded(t *testing.T) {
	before := mustParse(t, `CREATE TABLE users (id INTEGER PRIMARY KEY, email TEXT NOT NULL) STRICT;`)
	after := mustParse(t, `CREATE TABLE users (id INTEGER PRIMARY KEY, email TEXT NOT NULL UNIQUE) STRICT;`)

	td := findTableDiff(t, Diff(before, after), "users")
	if len(td.AddedIndexes) != 1 || td.AddedIndexes[0].Origin != "u" || !td.AddedIndexes[0].Unique {
		t.Fatalf("want a unique-constraint-origin index added, got %+v", td.AddedIndexes)
	}
}

func TestDiff_10_ForeignKeyActionChanged(t *testing.T) {
	before := mustParse(t, `
		CREATE TABLE users (id INTEGER PRIMARY KEY) STRICT;
		CREATE TABLE orders (id INTEGER PRIMARY KEY, user_id INTEGER REFERENCES users(id) ON DELETE NO ACTION) STRICT;
	`)
	after := mustParse(t, `
		CREATE TABLE users (id INTEGER PRIMARY KEY) STRICT;
		CREATE TABLE orders (id INTEGER PRIMARY KEY, user_id INTEGER REFERENCES users(id) ON DELETE CASCADE) STRICT;
	`)

	td := findTableDiff(t, Diff(before, after), "orders")
	if len(td.RemovedForeignKeys) != 1 || len(td.AddedForeignKeys) != 1 {
		t.Fatalf("want one FK removed and one added (action changed), got +%v -%v", td.AddedForeignKeys, td.RemovedForeignKeys)
	}
	if td.RemovedForeignKeys[0].OnDelete != "NO ACTION" || td.AddedForeignKeys[0].OnDelete != "CASCADE" {
		t.Errorf("unexpected FK action change: %+v", td)
	}
}

func TestDiff_12_ColumnDropped(t *testing.T) {
	before := mustParse(t, `CREATE TABLE users (id INTEGER PRIMARY KEY, nickname TEXT) STRICT;`)
	after := mustParse(t, `CREATE TABLE users (id INTEGER PRIMARY KEY) STRICT;`)

	td := findTableDiff(t, Diff(before, after), "users")
	if len(td.RemovedColumns) != 1 || td.RemovedColumns[0].Name != "nickname" {
		t.Fatalf("want RemovedColumns=[nickname], got %+v", td.RemovedColumns)
	}
}

func TestDiff_13_TableDropped(t *testing.T) {
	before := mustParse(t, `
		CREATE TABLE users (id INTEGER PRIMARY KEY) STRICT;
		CREATE TABLE sessions (id INTEGER PRIMARY KEY) STRICT;
	`)
	after := mustParse(t, `CREATE TABLE users (id INTEGER PRIMARY KEY) STRICT;`)

	d := Diff(before, after)
	if len(d.RemovedTables) != 1 || d.RemovedTables[0].Name != "sessions" {
		t.Fatalf("want RemovedTables=[sessions], got %+v", d.RemovedTables)
	}
}

func TestDiff_16_TwoUnrelatedChangesInOneDiff(t *testing.T) {
	before := mustParse(t, `
		CREATE TABLE users (id INTEGER PRIMARY KEY, age TEXT) STRICT;
		CREATE TABLE orders (id INTEGER PRIMARY KEY) STRICT;
	`)
	after := mustParse(t, `
		CREATE TABLE users (id INTEGER PRIMARY KEY, age INTEGER) STRICT;
		CREATE TABLE orders (id INTEGER PRIMARY KEY, note TEXT) STRICT;
	`)

	d := Diff(before, after)
	if len(d.ChangedTables) != 2 {
		t.Fatalf("want 2 changed tables, got %+v", d.ChangedTables)
	}
	users := findTableDiff(t, d, "users")
	if len(users.ChangedColumns) != 1 {
		t.Errorf("want users.age type change, got %+v", users)
	}
	orders := findTableDiff(t, d, "orders")
	if len(orders.AddedColumns) != 1 {
		t.Errorf("want orders.note added, got %+v", orders)
	}
}

func TestDiff_18_FKCycleBothTablesRebuild(t *testing.T) {
	before := mustParse(t, `
		CREATE TABLE a (id INTEGER PRIMARY KEY, b_id INTEGER REFERENCES b(id), tag TEXT) STRICT;
		CREATE TABLE b (id INTEGER PRIMARY KEY, a_id INTEGER REFERENCES a(id), label TEXT) STRICT;
	`)
	after := mustParse(t, `
		CREATE TABLE a (id INTEGER PRIMARY KEY, b_id INTEGER REFERENCES b(id), tag INTEGER) STRICT;
		CREATE TABLE b (id INTEGER PRIMARY KEY, a_id INTEGER REFERENCES a(id), label INTEGER) STRICT;
	`)

	d := Diff(before, after)
	if len(d.ChangedTables) != 2 {
		t.Fatalf("want both tables in the FK cycle to show as changed, got %+v", d.ChangedTables)
	}
	a := findTableDiff(t, d, "a")
	if len(a.ChangedColumns) != 1 || a.ChangedColumns[0].Name != "tag" {
		t.Errorf("want a.tag changed, got %+v", a)
	}
	b := findTableDiff(t, d, "b")
	if len(b.ChangedColumns) != 1 || b.ChangedColumns[0].Name != "label" {
		t.Errorf("want b.label changed, got %+v", b)
	}

	c := Classify(d)
	if c.Verdict != Safe {
		t.Fatalf("want an FK-cycle rebuild with no column/table loss to classify Safe, got %v (%+v)", c.Verdict, c)
	}
}

func TestDiff_IndexWhereClauseChanged(t *testing.T) {
	before := mustParse(t, `
		CREATE TABLE users (id INTEGER PRIMARY KEY, age INTEGER) STRICT;
		CREATE INDEX idx_users_age ON users(age) WHERE age > 0;
	`)
	after := mustParse(t, `
		CREATE TABLE users (id INTEGER PRIMARY KEY, age INTEGER) STRICT;
		CREATE INDEX idx_users_age ON users(age) WHERE age > 1;
	`)

	td := findTableDiff(t, Diff(before, after), "users")
	if len(td.AddedIndexes) != 1 || len(td.RemovedIndexes) != 1 {
		t.Fatalf("want the index with a changed WHERE clause to show as removed+added, got +%v -%v", td.AddedIndexes, td.RemovedIndexes)
	}
}

func TestDiff_IndexSortOrderChanged(t *testing.T) {
	before := mustParse(t, `
		CREATE TABLE users (id INTEGER PRIMARY KEY, age INTEGER) STRICT;
		CREATE INDEX idx_users_age ON users(age ASC);
	`)
	after := mustParse(t, `
		CREATE TABLE users (id INTEGER PRIMARY KEY, age INTEGER) STRICT;
		CREATE INDEX idx_users_age ON users(age DESC);
	`)

	td := findTableDiff(t, Diff(before, after), "users")
	if len(td.AddedIndexes) != 1 || len(td.RemovedIndexes) != 1 {
		t.Fatalf("want the index with a changed sort order to show as removed+added, got +%v -%v", td.AddedIndexes, td.RemovedIndexes)
	}
}

func TestDiff_NoChanges(t *testing.T) {
	ddl := `CREATE TABLE users (id INTEGER PRIMARY KEY, email TEXT NOT NULL) STRICT;`
	d := Diff(mustParse(t, ddl), mustParse(t, ddl))
	if !d.Empty() {
		t.Fatalf("want empty diff for identical schemas, got %+v", d)
	}
}
