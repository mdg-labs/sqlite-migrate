package rebuild

import (
	"context"
	"testing"

	"github.com/mdg-labs/sqlite-migrate/internal/schemadiff"
)

func TestQuoteIdent(t *testing.T) {
	cases := map[string]string{
		"users":      `"users"`,
		`weird"name`: `"weird""name"`,
		"order":      `"order"`, // a SQL keyword must still be quotable as an identifier
	}
	for in, want := range cases {
		if got := quoteIdent(in); got != want {
			t.Errorf("quoteIdent(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestRenameCreateTableSQL(t *testing.T) {
	cases := []struct {
		name  string
		sql   string
		newer string
		want  string
	}{
		{
			name:  "bare identifier",
			sql:   "CREATE TABLE users (id INTEGER PRIMARY KEY) STRICT",
			newer: "users_new",
			want:  `CREATE TABLE "users_new" (id INTEGER PRIMARY KEY) STRICT`,
		},
		{
			name:  "quoted identifier",
			sql:   `CREATE TABLE "users" (id INTEGER PRIMARY KEY) STRICT`,
			newer: "users_new",
			want:  `CREATE TABLE "users_new" (id INTEGER PRIMARY KEY) STRICT`,
		},
		{
			name:  "if not exists",
			sql:   `CREATE TABLE IF NOT EXISTS users (id INTEGER PRIMARY KEY) STRICT`,
			newer: "users_new",
			want:  `CREATE TABLE IF NOT EXISTS "users_new" (id INTEGER PRIMARY KEY) STRICT`,
		},
		{
			name:  "lower-case keywords",
			sql:   "create table users (id integer primary key) strict",
			newer: "users_new",
			want:  `create table "users_new" (id integer primary key) strict`,
		},
		{
			name:  "non-ASCII bare identifier",
			sql:   "CREATE TABLE bücher (id INTEGER PRIMARY KEY) STRICT",
			newer: "bücher_new",
			want:  `CREATE TABLE "bücher_new" (id INTEGER PRIMARY KEY) STRICT`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := renameCreateTableSQL(tc.sql, tc.newer)
			if err != nil {
				t.Fatalf("renameCreateTableSQL: %v", err)
			}
			if got != tc.want {
				t.Errorf("got %q, want %q", got, tc.want)
			}
		})
	}
}

func TestReferencesTable(t *testing.T) {
	cases := []struct {
		name string
		sql  string
		want bool
	}{
		{"bare match", "SELECT id FROM items", true},
		{"quoted match", `SELECT id FROM "items"`, true},
		{"prefix false positive", "SELECT id FROM items_audit", false},
		{"no match", "SELECT id FROM other", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := referencesTable(tc.sql, "items"); got != tc.want {
				t.Errorf("referencesTable(%q, items) = %v, want %v", tc.sql, got, tc.want)
			}
		})
	}
}

func TestReferencesTable_EmbeddedQuotes(t *testing.T) {
	cases := []struct {
		name  string
		sql   string
		table string
	}{
		{"double quote in double-quoted name", `SELECT * FROM "a""b"`, `a"b`},
		{"double quote in backticked name", "SELECT * FROM `a\"b`", `a"b`},
		{"double quote in bracketed name", `SELECT * FROM [a"b]`, `a"b`},
		{"backtick in backticked name", "SELECT * FROM `a``b`", "a`b"},
		{"backtick in double-quoted name", "SELECT * FROM \"a`b\"", "a`b"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if !referencesTable(tc.sql, tc.table) {
				t.Errorf("referencesTable(%q, %q) = false, want true", tc.sql, tc.table)
			}
		})
	}
}

func TestOrderByDependency_ParentBeforeChild(t *testing.T) {
	before := mustParse(t, `
		CREATE TABLE users (id INTEGER PRIMARY KEY, age TEXT) STRICT;
		CREATE TABLE orders (id INTEGER PRIMARY KEY, user_id INTEGER REFERENCES users(id), note TEXT) STRICT;
	`)
	after := mustParse(t, `
		CREATE TABLE users (id INTEGER PRIMARY KEY, age INTEGER) STRICT;
		CREATE TABLE orders (id INTEGER PRIMARY KEY, user_id INTEGER REFERENCES users(id), note INTEGER) STRICT;
	`)
	d := schemadiff.Diff(before, after)
	if len(d.ChangedTables) != 2 {
		t.Fatalf("want both tables changed, got %+v", d.ChangedTables)
	}

	ordered := orderByDependency(d.ChangedTables)
	if len(ordered) != 2 {
		t.Fatalf("want 2 ordered diffs, got %d", len(ordered))
	}
	if ordered[0].Name != "users" || ordered[1].Name != "orders" {
		t.Fatalf("want users (parent) before orders (child), got %s then %s", ordered[0].Name, ordered[1].Name)
	}
}

func TestOrderByDependency_CycleDoesNotHang(t *testing.T) {
	before := mustParse(t, `
		CREATE TABLE a (id INTEGER PRIMARY KEY, b_id INTEGER REFERENCES b(id), tag TEXT) STRICT;
		CREATE TABLE b (id INTEGER PRIMARY KEY, a_id INTEGER REFERENCES a(id), label TEXT) STRICT;
	`)
	after := mustParse(t, `
		CREATE TABLE a (id INTEGER PRIMARY KEY, b_id INTEGER REFERENCES b(id), tag INTEGER) STRICT;
		CREATE TABLE b (id INTEGER PRIMARY KEY, a_id INTEGER REFERENCES a(id), label INTEGER) STRICT;
	`)
	d := schemadiff.Diff(before, after)

	ordered := orderByDependency(d.ChangedTables)
	if len(ordered) != 2 {
		t.Fatalf("want both cyclic tables ordered exactly once each, got %+v", ordered)
	}
	seen := map[string]bool{}
	for _, td := range ordered {
		seen[td.Name] = true
	}
	if !seen["a"] || !seen["b"] {
		t.Fatalf("want both a and b present, got %+v", ordered)
	}
}

func TestLoadCatalog(t *testing.T) {
	schemaSQL := `
		CREATE TABLE items (id INTEGER PRIMARY KEY, price INTEGER) STRICT;
		CREATE TABLE items_audit (id INTEGER PRIMARY KEY, item_id INTEGER, changed_at TEXT) STRICT;
		CREATE TRIGGER trg_items_updated AFTER UPDATE ON items
		BEGIN
			INSERT INTO items_audit (item_id, changed_at) VALUES (NEW.id, 'now');
		END;
		CREATE VIEW items_view AS SELECT id, price FROM items;
	`
	cat, err := loadCatalog(context.Background(), schemaSQL, nil)
	if err != nil {
		t.Fatalf("loadCatalog: %v", err)
	}
	if len(cat.triggers) != 1 || cat.triggers[0].name != "trg_items_updated" || cat.triggers[0].tblName != "items" {
		t.Fatalf("want one trigger on items, got %+v", cat.triggers)
	}
	if len(cat.views) != 1 || cat.views[0].name != "items_view" {
		t.Fatalf("want one view, got %+v", cat.views)
	}
}

// TestHasAutoincrement also covers the finding that hasAutoincrement must
// match AUTOINCREMENT as a real keyword token, never a mention of the word
// inside a `--`/`/* */` comment, a `'...'` string literal (e.g. a CHECK
// constraint's allowed values), or a quoted identifier — all of which
// sqlite_master.sql preserves verbatim alongside the real DDL.
func TestHasAutoincrement(t *testing.T) {
	cases := map[string]bool{
		"CREATE TABLE t (id INTEGER PRIMARY KEY AUTOINCREMENT) STRICT": true,
		"create table t (id integer primary key autoincrement) strict": true,
		"CREATE TABLE t (id INTEGER PRIMARY KEY) STRICT":               false,
		"CREATE TABLE t (id INTEGER PRIMARY KEY, note TEXT) STRICT":    false,
		"CREATE TABLE t (\n" +
			"  id INTEGER PRIMARY KEY, -- deliberately not AUTOINCREMENT\n" +
			"  v TEXT\n" +
			") STRICT;": false,
		"CREATE TABLE t (id INTEGER PRIMARY KEY /* not AUTOINCREMENT */, v TEXT) STRICT;":                               false,
		"CREATE TABLE t (id INTEGER PRIMARY KEY, mode TEXT CHECK (mode IN ('autoincrement','manual')), v TEXT) STRICT;": false,
		`CREATE TABLE t (id INTEGER PRIMARY KEY, "autoincrement" TEXT) STRICT;`:                                         false,
		"CREATE TABLE t (id INTEGER PRIMARY KEY, `autoincrement` TEXT) STRICT;":                                         false,
		"CREATE TABLE t (id INTEGER PRIMARY KEY, [autoincrement] TEXT) STRICT;":                                         false,
		"CREATE TABLE t (id INTEGER PRIMARY KEY, autoincrementish TEXT) STRICT;":                                        false,
	}
	for sql, want := range cases {
		if got := hasAutoincrement(sql); got != want {
			t.Errorf("hasAutoincrement(%q) = %v, want %v", sql, got, want)
		}
	}
}

func TestQuoteLiteral(t *testing.T) {
	cases := map[string]string{
		"t":                    "'t'",
		"weird'name":           "'weird''name'",
		"t_sqlite_migrate_new": "'t_sqlite_migrate_new'",
	}
	for in, want := range cases {
		if got := quoteLiteral(in); got != want {
			t.Errorf("quoteLiteral(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestAffectedObjects_TriggerNotAttachedToRebuiltTable covers finding (a):
// a trigger attached to a table that isn't itself rebuilt, but whose body
// references a table that is, must still be swept into the affected set.
func TestAffectedObjects_TriggerNotAttachedToRebuiltTable(t *testing.T) {
	schemaSQL := `
		CREATE TABLE users (id INTEGER PRIMARY KEY, age INTEGER) STRICT;
		CREATE TABLE log (id INTEGER PRIMARY KEY, msg TEXT) STRICT;
		CREATE TRIGGER t AFTER INSERT ON log
		BEGIN
			UPDATE users SET age = age + 1 WHERE id = NEW.id;
		END;
	`
	cat, err := loadCatalog(context.Background(), schemaSQL, nil)
	if err != nil {
		t.Fatalf("loadCatalog: %v", err)
	}
	aff := affectedObjects([]string{"users"}, cat)
	if len(aff.triggers) != 1 || aff.triggers[0].name != "t" {
		t.Fatalf("want trigger t swept in despite being attached to log, got %+v", aff.triggers)
	}
}

// TestAffectedObjects_ViewChain covers finding (b): a view referencing
// another view that references a rebuilt table must be swept in
// transitively, not just the view attached directly.
func TestAffectedObjects_ViewChain(t *testing.T) {
	schemaSQL := `
		CREATE TABLE items (id INTEGER PRIMARY KEY, price INTEGER) STRICT;
		CREATE VIEW v1 AS SELECT id, price FROM items;
		CREATE VIEW v2 AS SELECT id FROM v1;
	`
	cat, err := loadCatalog(context.Background(), schemaSQL, nil)
	if err != nil {
		t.Fatalf("loadCatalog: %v", err)
	}
	aff := affectedObjects([]string{"items"}, cat)
	if len(aff.views) != 2 {
		t.Fatalf("want both v1 and v2 swept in, got %+v", aff.views)
	}
	ordered := topoSortViews(aff.views)
	if ordered[0].name != "v1" || ordered[1].name != "v2" {
		t.Fatalf("want v1 recreated before v2, got %s then %s", ordered[0].name, ordered[1].name)
	}
}

func TestHasRowidAlias(t *testing.T) {
	cases := []struct {
		ddl  string
		want bool
	}{
		{`CREATE TABLE t (id INTEGER PRIMARY KEY, x TEXT) STRICT;`, true},
		{`CREATE TABLE t (id integer primary key asc, x TEXT) STRICT;`, true},
		{`CREATE TABLE t (id INTEGER PRIMARY KEY DESC, x TEXT) STRICT;`, false},
		{`CREATE TABLE t (id INT PRIMARY KEY, x TEXT) STRICT;`, false},
		{`CREATE TABLE t (id TEXT PRIMARY KEY, x TEXT) STRICT;`, false},
		{`CREATE TABLE t (a INTEGER, b INTEGER, PRIMARY KEY (a, b)) STRICT;`, false},
		{`CREATE TABLE t (x TEXT) STRICT;`, false},
		{`CREATE TABLE t (id INTEGER PRIMARY KEY, x TEXT) STRICT, WITHOUT ROWID;`, false},
	}
	for _, tc := range cases {
		s, err := schemadiff.Parse(context.Background(), tc.ddl)
		if err != nil {
			t.Fatalf("Parse(%q): %v", tc.ddl, err)
		}
		if got := hasRowidAlias(s.Tables["t"]); got != tc.want {
			t.Errorf("hasRowidAlias(%q) = %v, want %v", tc.ddl, got, tc.want)
		}
	}
}
