// runtime.go holds the pieces the apply/check/verify/status subcommands
// share: loading the migration journal from disk, and reading a target
// database's bookkeeping table directly (Runner itself only exposes a full
// transactional Apply, never a read-only query) to tell which migrations
// it has already recorded as applied.
package main

import (
	"context"
	"database/sql"
	"fmt"
	"os"

	sqlitemigrate "github.com/mdg-labs/sqlite-migrate"

	_ "modernc.org/sqlite"
)

// bookkeepingTable is Runner's bookkeeping table name (see runner.go).
const bookkeepingTable = "schema_migrations"

// appliedMigration is one row of the bookkeeping table.
type appliedMigration struct {
	Checksum  string
	AppliedAt string
}

// loadMigrations loads every migration file in dir, sorted by version. A
// missing directory is a cold repo, not an error: there are no migrations
// yet.
func loadMigrations(ctx context.Context, dir string) ([]sqlitemigrate.Migration, error) {
	if _, err := os.Stat(dir); err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("stat migrations dir %s: %w", dir, err)
	}
	migrations, err := sqlitemigrate.LoadDir(ctx, os.DirFS(dir), ".")
	if err != nil {
		return nil, fmt.Errorf("load migrations from %s: %w", dir, err)
	}
	return migrations, nil
}

// openDB opens dbPath through the same pure-Go driver the runtime package
// uses, pinned to a single connection.
func openDB(dbPath string) (*sql.DB, error) {
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		return nil, fmt.Errorf("open database %s: %w", dbPath, err)
	}
	db.SetMaxOpenConns(1)
	return db, nil
}

// readApplied reads dbPath's bookkeeping table. A database file that
// doesn't exist yet, or one that hasn't been migrated yet, both report no
// applied migrations — without ever opening a connection when the file is
// missing, so inspecting a not-yet-created target never creates it as a
// side effect.
func readApplied(ctx context.Context, dbPath string) (map[string]appliedMigration, error) {
	if _, err := os.Stat(dbPath); err != nil {
		if os.IsNotExist(err) {
			return map[string]appliedMigration{}, nil
		}
		return nil, fmt.Errorf("stat database %s: %w", dbPath, err)
	}

	db, err := openDB(dbPath)
	if err != nil {
		return nil, err
	}
	defer func() { _ = db.Close() }()

	var tableCount int
	if err := db.QueryRowContext(ctx,
		`SELECT count(*) FROM sqlite_master WHERE type = 'table' AND name = ?`, bookkeepingTable,
	).Scan(&tableCount); err != nil {
		return nil, fmt.Errorf("check bookkeeping table: %w", err)
	}
	if tableCount == 0 {
		return map[string]appliedMigration{}, nil
	}

	rows, err := db.QueryContext(ctx, `SELECT version, checksum, applied_at FROM `+bookkeepingTable)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", bookkeepingTable, err)
	}
	defer func() { _ = rows.Close() }()

	applied := make(map[string]appliedMigration)
	for rows.Next() {
		var version string
		var a appliedMigration
		if err := rows.Scan(&version, &a.Checksum, &a.AppliedAt); err != nil {
			return nil, fmt.Errorf("scan %s row: %w", bookkeepingTable, err)
		}
		applied[version] = a
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read %s: %w", bookkeepingTable, err)
	}
	return applied, nil
}

// pendingMigrations reports which of migrations aren't yet recorded in
// applied, enforcing the same immutability checks Runner.Apply itself
// performs before running anything: a recorded migration whose checksum no
// longer matches its file, or one with no matching file at all, is refused
// rather than silently skipped, so a dry run never predicts success for a
// run that would actually be rejected.
func pendingMigrations(migrations []sqlitemigrate.Migration, applied map[string]appliedMigration) ([]sqlitemigrate.Migration, error) {
	var pending []sqlitemigrate.Migration
	seen := make(map[string]bool, len(migrations))
	for _, m := range migrations {
		seen[m.Version] = true
		rec, ok := applied[m.Version]
		if !ok {
			pending = append(pending, m)
			continue
		}
		if rec.Checksum != m.Checksum {
			return nil, &sqlitemigrate.ChecksumMismatchError{Version: m.Version, Want: rec.Checksum, Got: m.Checksum}
		}
	}
	for version := range applied {
		if !seen[version] {
			return nil, &sqlitemigrate.MissingMigrationError{Version: version}
		}
	}
	return pending, nil
}
