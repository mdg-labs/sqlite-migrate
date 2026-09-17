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
