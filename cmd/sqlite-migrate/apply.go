// apply.go implements the `apply` subcommand: run every migration not yet
// recorded as applied against a target database, through the public
// runtime's Runner.Apply — one transaction, an automatic VACUUM INTO
// backup first, foreign_key_check/integrity_check before commit. Dry-run
// is the default: apply only prints what would run until --yes is passed.
package main

import (
	"context"
	"flag"
	"fmt"
	"io"

	sqlitemigrate "github.com/mdg-labs/sqlite-migrate"
)

// RunApply is the CLI entry point for `sqlite-migrate apply`.
func RunApply(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("apply", flag.ContinueOnError)
	fs.SetOutput(stderr)
	var dbPath, dir string
	var yes bool
	var retainSnapshots int
	fs.StringVar(&dbPath, "db", "", "path to the target SQLite database file")
	fs.StringVar(&dir, "dir", "migrations", "directory holding generated migration files")
	fs.BoolVar(&yes, "yes", false, "apply pending migrations; without this flag, apply only prints what would run")
	fs.IntVar(&retainSnapshots, "retain-snapshots", 0, "snapshot files to keep per database after a successful apply; 0 keeps every snapshot")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if dbPath == "" {
		_, _ = fmt.Fprintln(stderr, "apply: -db is required")
		return 2
	}

	ctx := context.Background()
	migrations, err := loadMigrations(ctx, dir)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "apply: %v\n", err)
		return 1
	}

	if !yes {
		applied, err := readApplied(ctx, dbPath)
		if err != nil {
			_, _ = fmt.Fprintf(stderr, "apply: %v\n", err)
			return 1
		}
		pending, err := sqlitemigrate.PendingMigrations(migrations, applied)
		if err != nil {
			_, _ = fmt.Fprintf(stderr, "apply: %v\n", err)
			return 1
		}
		printApplyPlan(stdout, pending)
		return 0
	}

	runner := &sqlitemigrate.Runner{DBPath: dbPath, RetainSnapshots: retainSnapshots}
	applied, err := runner.Apply(ctx, migrations)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "apply: %v\n", err)
		return 1
	}
	if len(applied) == 0 {
		_, _ = fmt.Fprintln(stdout, "no pending migrations")
		return 0
	}
	_, _ = fmt.Fprintf(stdout, "applied %d migration(s):\n", len(applied))
	for _, m := range applied {
		_, _ = fmt.Fprintf(stdout, "  %s_%s\n", m.Version, m.Slug)
	}
	return 0
}

func printApplyPlan(stdout io.Writer, pending []sqlitemigrate.Migration) {
	if len(pending) == 0 {
		_, _ = fmt.Fprintln(stdout, "no pending migrations")
		return
	}
	_, _ = fmt.Fprintf(stdout, "dry run: %d pending migration(s) would be applied:\n", len(pending))
	for _, m := range pending {
		_, _ = fmt.Fprintf(stdout, "  %s_%s\n", m.Version, m.Slug)
	}
	_, _ = fmt.Fprintln(stdout, "rerun with --yes to apply")
}
