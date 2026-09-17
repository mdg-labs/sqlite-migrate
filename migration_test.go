package sqlitemigrate

import (
	"context"
	"strings"
	"testing"
	"testing/fstest"
)

func TestLoad_ParsesFilenameConvention(t *testing.T) {
	ctx := context.Background()
	sql := "CREATE TABLE users (id INTEGER PRIMARY KEY) STRICT;\n"
	m, err := Load(ctx, "20260917143022_add_users.sql", strings.NewReader(sql))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if m.Version != "20260917143022" {
		t.Errorf("Version = %q, want 20260917143022", m.Version)
	}
	if m.Slug != "add_users" {
		t.Errorf("Slug = %q, want add_users", m.Slug)
	}
	if m.SQL != sql {
		t.Errorf("SQL = %q, want %q", m.SQL, sql)
	}
	if m.Checksum != Checksum(sql) {
		t.Errorf("Checksum = %q, want %q", m.Checksum, Checksum(sql))
	}
}

func TestLoad_RejectsBadFilenames(t *testing.T) {
	ctx := context.Background()
	cases := []string{
		"add_users.sql",
		"2026_add_users.sql",
		"20260917143022.sql",
		"20260917143022_add_users.txt",
	}
	for _, name := range cases {
		if _, err := Load(ctx, name, strings.NewReader("")); err == nil {
			t.Errorf("Load(%q) succeeded, want an error", name)
		}
	}
}

func TestLoadDir_SortsByVersion(t *testing.T) {
	ctx := context.Background()
	fsys := fstest.MapFS{
		"migrations/20260917143022_add_users.sql": &fstest.MapFile{Data: []byte("CREATE TABLE users (id INTEGER PRIMARY KEY) STRICT;")},
		"migrations/20260101000000_init.sql":      &fstest.MapFile{Data: []byte("CREATE TABLE t (id INTEGER PRIMARY KEY) STRICT;")},
		"migrations/README.md":                    &fstest.MapFile{Data: []byte("not a migration")},
	}

	migrations, err := LoadDir(ctx, fsys, "migrations")
	if err != nil {
		t.Fatalf("LoadDir: %v", err)
	}
	if len(migrations) != 2 {
		t.Fatalf("LoadDir returned %d migrations, want 2", len(migrations))
	}
	if migrations[0].Version != "20260101000000" || migrations[1].Version != "20260917143022" {
		t.Errorf("LoadDir did not sort by version: got %q, %q", migrations[0].Version, migrations[1].Version)
	}
}

func TestLoadDir_RejectsDuplicateVersions(t *testing.T) {
	ctx := context.Background()
	fsys := fstest.MapFS{
		"migrations/20260917143022_add_users.sql":  &fstest.MapFile{Data: []byte("CREATE TABLE users (id INTEGER PRIMARY KEY) STRICT;")},
		"migrations/20260917143022_add_orders.sql": &fstest.MapFile{Data: []byte("CREATE TABLE orders (id INTEGER PRIMARY KEY) STRICT;")},
	}

	if _, err := LoadDir(ctx, fsys, "migrations"); err == nil {
		t.Fatal("LoadDir accepted two migrations sharing the same version")
	}
}
