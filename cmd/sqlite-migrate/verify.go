// verify.go implements the `verify` subcommand: a ground-truth check
// against a real target database, independent of its bookkeeping table —
// it replays the migration journal into a temporary database and compares
// the result structurally against the target database's actual schema,
// reporting any drift.
package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"

	sqlitemigrate "github.com/mdg-labs/sqlite-migrate"
)

// RunVerify is the CLI entry point for `sqlite-migrate verify`.
func RunVerify(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("verify", flag.ContinueOnError)
	fs.SetOutput(stderr)
	var dbPath, dir string
	fs.StringVar(&dbPath, "db", "", "path to the target SQLite database file")
	fs.StringVar(&dir, "dir", "migrations", "directory holding generated migration files")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if dbPath == "" {
		_, _ = fmt.Fprintln(stderr, "verify: -db is required")
		return 2
	}

	ctx := context.Background()
	migrations, err := loadMigrations(ctx, dir)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "verify: %v\n", err)
		return 1
	}

	if _, err := os.Stat(dbPath); err != nil {
		_, _ = fmt.Fprintf(stderr, "verify: database %s: %v\n", dbPath, err)
		return 1
	}

	db, err := openDB(dbPath)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "verify: %v\n", err)
		return 1
	}
	defer func() { _ = db.Close() }()

	actual, err := sqlitemigrate.CaptureSchema(ctx, db)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "verify: %v\n", err)
		return 1
	}

	report, err := sqlitemigrate.CheckDrift(ctx, migrations, actual)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "verify: %v\n", err)
		return 1
	}

	if report.Empty() {
		_, _ = fmt.Fprintln(stdout, "ok: database matches the migration journal")
		return 0
	}

	_, _ = fmt.Fprintln(stdout, "drift detected:")
	for _, o := range report.OnlyInExpected {
		_, _ = fmt.Fprintf(stdout, "  in database, not produced by the migration journal: %s %s\n", o.Type, o.Name)
	}
	for _, o := range report.OnlyInActual {
		_, _ = fmt.Fprintf(stdout, "  produced by the migration journal, not in database: %s %s\n", o.Type, o.Name)
	}
	return 1
}
