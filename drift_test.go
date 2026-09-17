package sqlitemigrate

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"

	_ "modernc.org/sqlite"
)

func openFileDB(t *testing.T, path string) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("open %q: %v", path, err)
	}
	t.Cleanup(func() { _ = db.Close() })
	db.SetMaxOpenConns(1)
	return db
}

func TestCheckDrift_NoDriftAgainstRealDatabase(t *testing.T) {
	ctx := context.Background()
	migrations := []Migration{
		{Version: "1", Filename: "1.sql", SQL: `CREATE TABLE users (id INTEGER PRIMARY KEY, name TEXT NOT NULL) STRICT;`},
		{Version: "2", Filename: "2.sql", SQL: `CREATE INDEX idx_users_name ON users(name);`},
	}

	dbPath := filepath.Join(t.TempDir(), "app.db")
	db := openFileDB(t, dbPath)
	for _, m := range migrations {
		if _, err := db.ExecContext(ctx, m.SQL); err != nil {
			t.Fatalf("seed real database: %v", err)
		}
	}

	expected, err := CaptureSchema(ctx, db)
	if err != nil {
		t.Fatalf("CaptureSchema: %v", err)
	}

	report, err := CheckDrift(ctx, migrations, expected)
	if err != nil {
		t.Fatalf("CheckDrift: %v", err)
	}
	if !report.Empty() {
		t.Fatalf("CheckDrift reported drift for an identical database: %+v", report)
	}
}

func TestCheckDrift_NoDriftAfterRunnerApply(t *testing.T) {
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "app.db")
	r := newRunner(t, dbPath)

	migrations := []Migration{
		{Version: "20260101000000", Slug: "init", Filename: "20260101000000_init.sql", SQL: `CREATE TABLE users (id INTEGER PRIMARY KEY, name TEXT NOT NULL) STRICT;`},
		{Version: "20260102000000", Slug: "add_index", Filename: "20260102000000_add_index.sql", SQL: `CREATE INDEX idx_users_name ON users(name);`},
	}
	migrations[0].Checksum = Checksum(migrations[0].SQL)
	migrations[1].Checksum = Checksum(migrations[1].SQL)

	if _, err := r.Apply(ctx, migrations); err != nil {
		t.Fatalf("Apply: %v", err)
	}

	db := openFileDB(t, dbPath)
	expected, err := CaptureSchema(ctx, db)
	if err != nil {
		t.Fatalf("CaptureSchema: %v", err)
	}

	report, err := CheckDrift(ctx, migrations, expected)
	if err != nil {
		t.Fatalf("CheckDrift: %v", err)
	}
	if !report.Empty() {
		t.Fatalf("CheckDrift reported drift against a database Runner.Apply itself produced: %+v", report)
	}
}

func TestCheckDrift_DetectsManualDriftOnRealDatabase(t *testing.T) {
	ctx := context.Background()
	migrations := []Migration{
		{Version: "1", Filename: "1.sql", SQL: `CREATE TABLE users (id INTEGER PRIMARY KEY, name TEXT NOT NULL) STRICT;`},
	}

	dbPath := filepath.Join(t.TempDir(), "app.db")
	db := openFileDB(t, dbPath)
	for _, m := range migrations {
		if _, err := db.ExecContext(ctx, m.SQL); err != nil {
			t.Fatalf("seed real database: %v", err)
		}
	}
	// Simulate manual drift: a column added directly against the real
	// database, bypassing the migration journal entirely.
	if _, err := db.ExecContext(ctx, `ALTER TABLE users ADD COLUMN email TEXT`); err != nil {
		t.Fatalf("apply manual drift: %v", err)
	}

	expected, err := CaptureSchema(ctx, db)
	if err != nil {
		t.Fatalf("CaptureSchema: %v", err)
	}

	report, err := CheckDrift(ctx, migrations, expected)
	if err != nil {
		t.Fatalf("CheckDrift: %v", err)
	}
	if report.Empty() {
		t.Fatal("CheckDrift missed drift introduced directly against the real database")
	}
	if len(report.OnlyInExpected) != 1 || report.OnlyInExpected[0].Name != "users" {
		t.Fatalf("unexpected drift report: %+v", report)
	}
}

func TestReplaySchema_FailsOnBadSQL(t *testing.T) {
	migrations := []Migration{
		{Version: "1", Filename: "1.sql", SQL: `CREATE TABLE users (id INTEGER PRIMARY KEY) STRICT;`},
		{Version: "2", Filename: "2.sql", SQL: `NOT VALID SQL;`},
	}
	if _, err := ReplaySchema(context.Background(), migrations); err == nil {
		t.Fatal("ReplaySchema accepted an invalid migration")
	}
}
