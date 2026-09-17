package schemadiff

import (
	"context"
	"errors"
	"strings"
	"testing"
)

func TestParse_TableColumnsForeignKeysIndexes(t *testing.T) {
	schema := mustParse(t, `
		CREATE TABLE users (
			id INTEGER PRIMARY KEY,
			email TEXT NOT NULL,
			nickname TEXT
		) STRICT;
		CREATE TABLE orders (
			id INTEGER PRIMARY KEY,
			user_id INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
			total REAL NOT NULL DEFAULT 0
		) STRICT;
		CREATE UNIQUE INDEX idx_users_email ON users(email);
		CREATE INDEX idx_orders_user_id ON orders(user_id);
	`)

	if len(schema.Tables) != 2 {
		t.Fatalf("want 2 tables, got %d", len(schema.Tables))
	}

	users, ok := schema.Tables["users"]
	if !ok {
		t.Fatal("missing users table")
	}
	if !users.Strict {
		t.Error("want users.Strict = true")
	}
	if got := diffColumnNames(users.Columns); len(got) != 3 {
		t.Errorf("want 3 columns on users, got %v", got)
	}
	emailCol, ok := users.Column("email")
	if !ok || !emailCol.NotNull {
		t.Errorf("want users.email NOT NULL, got %+v (found=%v)", emailCol, ok)
	}
	if len(users.Indexes) != 1 || users.Indexes[0].Name != "idx_users_email" || !users.Indexes[0].Unique {
		t.Errorf("want one unique index idx_users_email on users, got %+v", users.Indexes)
	}
	if got := users.Indexes[0].Columns; len(got) != 1 || got[0] != "email" {
		t.Errorf("want idx_users_email to cover [email], got %v", got)
	}

	orders, ok := schema.Tables["orders"]
	if !ok {
		t.Fatal("missing orders table")
	}
	if len(orders.ForeignKeys) != 1 {
		t.Fatalf("want 1 foreign key on orders, got %d", len(orders.ForeignKeys))
	}
	fk := orders.ForeignKeys[0]
	if fk.Table != "users" || fk.From != "user_id" || fk.To != "id" || fk.OnDelete != "CASCADE" {
		t.Errorf("unexpected foreign key: %+v", fk)
	}
	totalCol, ok := orders.Column("total")
	if !ok || !totalCol.HasDefault || totalCol.DefaultValue != "0" {
		t.Errorf("want orders.total to have default 0, got %+v (found=%v)", totalCol, ok)
	}
}

func TestParse_RejectsNonStrictTable(t *testing.T) {
	_, err := Parse(context.Background(), `CREATE TABLE widgets (id INTEGER PRIMARY KEY, name TEXT);`)
	if err == nil {
		t.Fatal("want error for non-STRICT table, got nil")
	}
	var notStrict *NotStrictError
	if !errors.As(err, &notStrict) {
		t.Fatalf("want *NotStrictError, got %T: %v", err, err)
	}
	if notStrict.Table != "widgets" {
		t.Errorf("want NotStrictError.Table = widgets, got %q", notStrict.Table)
	}
}

func TestParse_InvalidSQL(t *testing.T) {
	_, err := Parse(context.Background(), `CREATE TABLE ( this is not valid SQL`)
	if err == nil {
		t.Fatal("want error for invalid SQL, got nil")
	}
}

// TestParse_RejectsSchemaContainingNULByte reproduces a bug found by Phase
// 8 fuzzing: modernc.org/sqlite silently stops executing a query string at
// its first NUL byte and reports no error at all (verified directly), so
// without this guard a schema.sql containing one would have every table
// declared after it vanish from the parsed Schema without a trace.
func TestParse_RejectsSchemaContainingNULByte(t *testing.T) {
	_, err := Parse(context.Background(), "CREATE TABLE a (id INTEGER) STRICT;\x00CREATE TABLE b (id INTEGER) STRICT;")
	if err == nil {
		t.Fatal("want error for a schema.sql containing a NUL byte, got nil")
	}
}

func TestParse_WithoutRowID(t *testing.T) {
	schema := mustParse(t, `CREATE TABLE kv (k TEXT PRIMARY KEY, v TEXT NOT NULL) STRICT, WITHOUT ROWID;`)
	kv, ok := schema.Tables["kv"]
	if !ok {
		t.Fatal("missing kv table")
	}
	if !kv.WithoutRowID {
		t.Error("want kv.WithoutRowID = true")
	}
}

func TestParse_ForeignKeyWithoutExplicitParentColumn(t *testing.T) {
	schema := mustParse(t, `
		CREATE TABLE users (id INTEGER PRIMARY KEY, email TEXT) STRICT;
		CREATE TABLE orders (id INTEGER PRIMARY KEY, user_id INTEGER REFERENCES users) STRICT;
	`)

	orders, ok := schema.Tables["orders"]
	if !ok {
		t.Fatal("missing orders table")
	}
	if len(orders.ForeignKeys) != 1 {
		t.Fatalf("want 1 foreign key on orders, got %+v", orders.ForeignKeys)
	}
	fk := orders.ForeignKeys[0]
	if fk.Table != "users" || fk.From != "user_id" || fk.To != "id" {
		t.Errorf("want a FK without an explicit parent column list to resolve to the parent's primary key, got %+v", fk)
	}
}

func TestParse_ForeignKeyWithoutExplicitParentColumn_CompositePrimaryKey(t *testing.T) {
	schema := mustParse(t, `
		CREATE TABLE p (a TEXT NOT NULL, b TEXT NOT NULL, PRIMARY KEY (a, b)) STRICT;
		CREATE TABLE c (x TEXT NOT NULL, y TEXT NOT NULL, FOREIGN KEY (x, y) REFERENCES p) STRICT;
	`)

	child, ok := schema.Tables["c"]
	if !ok {
		t.Fatal("missing c table")
	}
	if len(child.ForeignKeys) != 2 {
		t.Fatalf("want 2 foreign key columns on c, got %+v", child.ForeignKeys)
	}

	byFrom := map[string]ForeignKey{}
	for _, fk := range child.ForeignKeys {
		byFrom[fk.From] = fk
	}
	if got := byFrom["x"].To; got != "a" {
		t.Errorf("want x to resolve to parent column a, got %q", got)
	}
	if got := byFrom["y"].To; got != "b" {
		t.Errorf("want y to resolve to parent column b, got %q", got)
	}
}

func TestParse_ExpressionIndex(t *testing.T) {
	schema := mustParse(t, `
		CREATE TABLE users (id INTEGER PRIMARY KEY, email TEXT) STRICT;
		CREATE INDEX idx_users_lower_email ON users(lower(email));
	`)

	users, ok := schema.Tables["users"]
	if !ok {
		t.Fatal("missing users table")
	}
	if len(users.Indexes) != 1 {
		t.Fatalf("want 1 index on users, got %+v", users.Indexes)
	}
	idx := users.Indexes[0]
	if idx.Name != "idx_users_lower_email" {
		t.Errorf("want idx_users_lower_email, got %+v", idx)
	}
	if len(idx.Columns) != 1 {
		t.Errorf("want a single placeholder column entry for the indexed expression, got %+v", idx.Columns)
	}
	if !strings.Contains(idx.SQL, "lower(email)") {
		t.Errorf("want Index.SQL to carry the indexed expression, got %q", idx.SQL)
	}
}

func TestParse_VirtualTableFTS5ExemptFromSTRICT(t *testing.T) {
	schema := mustParse(t, `
		CREATE TABLE users (id INTEGER PRIMARY KEY) STRICT;
		CREATE VIRTUAL TABLE docs USING fts5(body);
	`)

	docs, ok := schema.Tables["docs"]
	if !ok {
		t.Fatal("missing docs table")
	}
	if !docs.Virtual {
		t.Error("want docs.Virtual = true")
	}
	if len(docs.ForeignKeys) != 0 || len(docs.Indexes) != 0 {
		t.Errorf("want a virtual table to carry no ForeignKeys/Indexes, got %+v", docs)
	}
	if got := diffColumnNames(docs.Columns); len(got) != 1 || got[0] != "body" {
		t.Errorf("want a virtual table's Columns populated from pragma_table_info, got %v", got)
	}
}

func TestParse_VirtualTableFTS5ShadowTablesAreHidden(t *testing.T) {
	schema := mustParse(t, `CREATE VIRTUAL TABLE docs USING fts5(body);`)

	for _, shadow := range []string{"docs_data", "docs_idx", "docs_content", "docs_docsize", "docs_config"} {
		if _, ok := schema.Tables[shadow]; ok {
			t.Errorf("want shadow table %q hidden from the parsed schema, got it present", shadow)
		}
	}
	if len(schema.Tables) != 1 {
		t.Fatalf("want exactly 1 table (docs), got %v", schema.SortedTableNames())
	}
}

func TestParse_TableNameResemblingContentlessFTS5ShadowTableIsNotHidden(t *testing.T) {
	// content='' puts fts5 in contentless mode, where the module never
	// creates a <table>_content table of its own; an ordinary table that
	// happens to have that name is real and must stay in the schema.
	schema := mustParse(t, `
		CREATE VIRTUAL TABLE docs USING fts5(body, content='');
		CREATE TABLE docs_content (id INTEGER PRIMARY KEY, v TEXT) STRICT;
	`)

	if _, ok := schema.Tables["docs_content"]; !ok {
		t.Fatalf("want docs_content present despite its shadow-matching name, got tables %v", schema.SortedTableNames())
	}
	if len(schema.Tables) != 2 {
		t.Fatalf("want exactly 2 tables (docs, docs_content), got %v", schema.SortedTableNames())
	}
}

func TestParse_TableNameResemblingContentlessFTS5ShadowTable_RejectsNonStrict(t *testing.T) {
	_, err := Parse(context.Background(), `
		CREATE VIRTUAL TABLE docs USING fts5(body, content='');
		CREATE TABLE docs_content (id INTEGER PRIMARY KEY, v TEXT);
	`)
	var notStrict *NotStrictError
	if !errors.As(err, &notStrict) {
		t.Fatalf("want *NotStrictError for the real, non-STRICT docs_content table, got %v", err)
	}
	if notStrict.Table != "docs_content" {
		t.Errorf("want NotStrictError.Table = docs_content, got %q", notStrict.Table)
	}
}

func TestParse_ExternalContentFTS5TableIsNotHidden(t *testing.T) {
	// In external-content mode the module never creates its own
	// <table>_content table either — it reads directly from the named
	// table, which is exactly the shadow-matching name here.
	schema := mustParse(t, `
		CREATE TABLE docs_content (id INTEGER PRIMARY KEY, body TEXT) STRICT;
		CREATE VIRTUAL TABLE docs USING fts5(body, content='docs_content', content_rowid='id');
	`)

	if _, ok := schema.Tables["docs_content"]; !ok {
		t.Fatalf("want the external-content table docs_content present, got tables %v", schema.SortedTableNames())
	}
}

func TestParse_TableNameResemblingDocsizeZeroFTS5ShadowTableIsNotHidden(t *testing.T) {
	// columnsize=0 means the module never creates a <table>_docsize table.
	schema := mustParse(t, `
		CREATE VIRTUAL TABLE docs USING fts5(body, columnsize=0);
		CREATE TABLE docs_docsize (id INTEGER PRIMARY KEY) STRICT;
	`)

	if _, ok := schema.Tables["docs_docsize"]; !ok {
		t.Fatalf("want docs_docsize present despite its shadow-matching name, got tables %v", schema.SortedTableNames())
	}
}

func TestDiff_Destructive_DroppingTableWithFTS5ShadowMatchingNameIsDestructive(t *testing.T) {
	before := mustParse(t, `
		CREATE VIRTUAL TABLE docs USING fts5(body, content='');
		CREATE TABLE docs_content (id INTEGER PRIMARY KEY, v TEXT) STRICT;
	`)
	after := mustParse(t, `CREATE VIRTUAL TABLE docs USING fts5(body, content='');`)

	d := Diff(before, after)
	if len(d.RemovedTables) != 1 || d.RemovedTables[0].Name != "docs_content" {
		t.Fatalf("want RemovedTables=[docs_content], got %+v", d.RemovedTables)
	}
	if c := Classify(d); c.Verdict != Destructive {
		t.Fatalf("want dropping a real table with a shadow-matching name to classify Destructive, got %v (%+v)", c.Verdict, c)
	}
}

func TestDiff_Destructive_DroppingColumnFromTableWithFTS5ShadowMatchingNameIsDestructive(t *testing.T) {
	before := mustParse(t, `
		CREATE VIRTUAL TABLE docs USING fts5(body, content='');
		CREATE TABLE docs_content (id INTEGER PRIMARY KEY, v TEXT) STRICT;
	`)
	after := mustParse(t, `
		CREATE VIRTUAL TABLE docs USING fts5(body, content='');
		CREATE TABLE docs_content (id INTEGER PRIMARY KEY) STRICT;
	`)

	c := Classify(Diff(before, after))
	if c.Verdict != Destructive {
		t.Fatalf("want dropping a column from a table with a shadow-matching name to classify Destructive, got %v (%+v)", c.Verdict, c)
	}
	if got := c.RemovedColumns["docs_content"]; len(got) != 1 || got[0] != "v" {
		t.Fatalf("want RemovedColumns[docs_content]=[v], got %+v", c.RemovedColumns)
	}
}

func TestDiff_Destructive_FTS5ColumnDropped(t *testing.T) {
	before := mustParse(t, `CREATE VIRTUAL TABLE docs USING fts5(body, title);`)
	after := mustParse(t, `CREATE VIRTUAL TABLE docs USING fts5(body);`)

	c := Classify(Diff(before, after))
	if c.Verdict != Destructive {
		t.Fatalf("want dropping an fts5 column to classify Destructive, got %v (%+v)", c.Verdict, c)
	}
	if got := c.RemovedColumns["docs"]; len(got) != 1 || got[0] != "title" {
		t.Fatalf("want RemovedColumns[docs]=[title], got %+v", c.RemovedColumns)
	}
}

func TestDiff_Safe_FTS5ColumnAdded(t *testing.T) {
	before := mustParse(t, `CREATE VIRTUAL TABLE docs USING fts5(body);`)
	after := mustParse(t, `CREATE VIRTUAL TABLE docs USING fts5(body, title);`)

	c := Classify(Diff(before, after))
	if c.Verdict != Safe {
		t.Fatalf("want adding an fts5 column to classify Safe, got %v (%+v)", c.Verdict, c)
	}
}

func TestDiff_Destructive_RTreeColumnDropped(t *testing.T) {
	before := mustParse(t, `CREATE VIRTUAL TABLE bbox USING rtree(id, minx, maxx, miny, maxy);`)
	after := mustParse(t, `CREATE VIRTUAL TABLE bbox USING rtree(id, minx, maxx);`)

	c := Classify(Diff(before, after))
	if c.Verdict != Destructive {
		t.Fatalf("want dropping rtree columns to classify Destructive, got %v (%+v)", c.Verdict, c)
	}
	if got := c.RemovedColumns["bbox"]; len(got) != 2 {
		t.Fatalf("want 2 RemovedColumns[bbox], got %+v", c.RemovedColumns)
	}
}

func TestDiff_Destructive_VirtualToOrdinaryTableColumnRemoved(t *testing.T) {
	before := mustParse(t, `CREATE VIRTUAL TABLE docs USING fts5(body, title);`)
	after := mustParse(t, `CREATE TABLE docs (body TEXT) STRICT;`)

	c := Classify(Diff(before, after))
	if c.Verdict != Destructive {
		t.Fatalf("want replacing a virtual table with an ordinary one that drops a column to classify Destructive, got %v (%+v)", c.Verdict, c)
	}
}

func TestDiff_VirtualToOrdinaryTableSameColumnsStillShowsChange(t *testing.T) {
	before := mustParse(t, `CREATE VIRTUAL TABLE docs USING fts5(body);`)
	after := mustParse(t, `CREATE TABLE docs (body TEXT) STRICT;`)

	td := findTableDiff(t, Diff(before, after), "docs")
	if td.Empty() {
		t.Fatalf("want a virtual-to-ordinary table change to be visible in the diff even when the column names match, got empty TableDiff")
	}
}

func TestDiff_VirtualTable_AddedRemovedChanged(t *testing.T) {
	withoutDocs := mustParse(t, `CREATE TABLE users (id INTEGER PRIMARY KEY) STRICT;`)
	withDocs := mustParse(t, `
		CREATE TABLE users (id INTEGER PRIMARY KEY) STRICT;
		CREATE VIRTUAL TABLE docs USING fts5(body);
	`)
	withChangedDocs := mustParse(t, `
		CREATE TABLE users (id INTEGER PRIMARY KEY) STRICT;
		CREATE VIRTUAL TABLE docs USING fts5(body, title);
	`)

	added := Diff(withoutDocs, withDocs)
	if len(added.AddedTables) != 1 || added.AddedTables[0].Name != "docs" {
		t.Fatalf("want AddedTables=[docs], got %+v", added.AddedTables)
	}

	removed := Diff(withDocs, withoutDocs)
	if len(removed.RemovedTables) != 1 || removed.RemovedTables[0].Name != "docs" {
		t.Fatalf("want RemovedTables=[docs], got %+v", removed.RemovedTables)
	}
	if c := Classify(removed); c.Verdict != Destructive {
		t.Fatalf("want dropping a virtual table to classify Destructive, got %v (%+v)", c.Verdict, c)
	}

	td := findTableDiff(t, Diff(withDocs, withChangedDocs), "docs")
	if !td.SQLChanged {
		t.Fatalf("want a changed fts5 column list to register as SQLChanged, got %+v", td)
	}
}

func TestParse_TableNamesResemblingSQLiteInternalTablesAreNotHidden(t *testing.T) {
	schema := mustParse(t, `
		CREATE TABLE users (id INTEGER PRIMARY KEY) STRICT;
		CREATE TABLE sqlite3_stats (id INTEGER PRIMARY KEY) STRICT;
		CREATE TABLE sqliteusers (id INTEGER PRIMARY KEY) STRICT;
	`)

	for _, name := range []string{"users", "sqlite3_stats", "sqliteusers"} {
		if _, ok := schema.Tables[name]; !ok {
			t.Errorf("want table %q present, got tables %v", name, schema.SortedTableNames())
		}
	}
	if len(schema.Tables) != 3 {
		t.Fatalf("want 3 tables, got %v", schema.SortedTableNames())
	}
}
