// status.go implements the `status` subcommand: for each migration in the
// journal, reports whether a target database has it recorded as applied or
// still pending, reading the bookkeeping table directly rather than
// through Runner (which only exposes a full transactional Apply, not a
// read-only query).
package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"sort"
)

// RunStatus is the CLI entry point for `sqlite-migrate status`.
func RunStatus(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("status", flag.ContinueOnError)
	fs.SetOutput(stderr)
	var dbPath, dir string
	fs.StringVar(&dbPath, "db", "", "path to the target SQLite database file")
	fs.StringVar(&dir, "dir", "migrations", "directory holding generated migration files")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if dbPath == "" {
		_, _ = fmt.Fprintln(stderr, "status: -db is required")
		return 2
	}

	ctx := context.Background()
	migrations, err := loadMigrations(ctx, dir)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "status: %v\n", err)
		return 1
	}

	applied, err := readApplied(ctx, dbPath)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "status: %v\n", err)
		return 1
	}

	if len(migrations) == 0 && len(applied) == 0 {
		_, _ = fmt.Fprintln(stdout, "no migrations")
		return 0
	}

	seen := make(map[string]bool, len(migrations))
	for _, m := range migrations {
		seen[m.Version] = true
		rec, ok := applied[m.Version]
		switch {
		case !ok:
			_, _ = fmt.Fprintf(stdout, "%s_%s  pending\n", m.Version, m.Slug)
		case rec.Checksum != m.Checksum:
			_, _ = fmt.Fprintf(stdout, "%s_%s  applied, checksum mismatch (file was edited after being applied)\n", m.Version, m.Slug)
		default:
			_, _ = fmt.Fprintf(stdout, "%s_%s  applied at %s\n", m.Version, m.Slug, rec.AppliedAt)
		}
	}

	var orphaned []string
	for version := range applied {
		if !seen[version] {
			orphaned = append(orphaned, version)
		}
	}
	sort.Strings(orphaned)
	for _, version := range orphaned {
		_, _ = fmt.Fprintf(stdout, "%s  applied, migration file missing\n", version)
	}

	return 0
}
