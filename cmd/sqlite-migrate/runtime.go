// runtime.go holds the pieces the apply/check/verify/status subcommands
// share: loading the migration journal from disk, and opening direct,
// read-only connections to a target database for commands that don't run a
// migration (status, verify).
package main

import (
	"context"
	"database/sql"
	"fmt"
	"os"

	sqlitemigrate "github.com/mdg-labs/sqlite-migrate"

	_ "modernc.org/sqlite"
)

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
// uses, pinned to a single connection, with the same busy_timeout Runner
// itself sets — so a status/verify run waits out a brief lock held by
// another process (e.g. a concurrent apply) instead of failing immediately
// with "database is locked".
func openDB(dbPath string) (*sql.DB, error) {
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		return nil, fmt.Errorf("open database %s: %w", dbPath, err)
	}
	db.SetMaxOpenConns(1)
	if _, err := db.ExecContext(context.Background(), fmt.Sprintf("PRAGMA busy_timeout = %d", sqlitemigrate.BusyTimeoutMillis)); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("set busy_timeout on %s: %w", dbPath, err)
	}
	return db, nil
}

// readApplied reports dbPath's applied migrations through Runner.Applied,
// so the CLI never needs its own copy of the bookkeeping table's name or
// schema.
func readApplied(ctx context.Context, dbPath string) ([]sqlitemigrate.AppliedMigration, error) {
	runner := &sqlitemigrate.Runner{DBPath: dbPath}
	return runner.Applied(ctx)
}
