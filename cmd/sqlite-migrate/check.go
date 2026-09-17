// check.go implements the `check` subcommand: static, connection-free
// verification of the migration journal — every migration file's checksum
// header still matches its body, and replaying the whole journal still
// reproduces schema.sql exactly (the same replay-based drift check
// generate verifies a candidate against before writing it). It never opens
// a target database, so it's safe to run in CI without one.
package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"flag"
	"fmt"
	"io"
	"os"
	"regexp"
	"strings"

	"github.com/mdg-labs/sqlite-migrate/internal/schemadiff"

	sqlitemigrate "github.com/mdg-labs/sqlite-migrate"
)

// checksumHeaderPattern matches the leading comment writeMigrationFile
// (generate.go) puts on every generated migration file.
var checksumHeaderPattern = regexp.MustCompile(`^-- sqlite-migrate: checksum ([0-9a-f]{64})\n\n`)

// RunCheck is the CLI entry point for `sqlite-migrate check`.
func RunCheck(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("check", flag.ContinueOnError)
	fs.SetOutput(stderr)
	var schemaPath, dir string
	fs.StringVar(&schemaPath, "schema", "schema.sql", "path to the schema.sql source of truth")
	fs.StringVar(&dir, "dir", "migrations", "directory holding generated migration files")
	if err := fs.Parse(args); err != nil {
		return 2
	}

	ctx := context.Background()
	migrations, err := loadMigrations(ctx, dir)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "check: %v\n", err)
		return 1
	}

	for _, m := range migrations {
		if err := verifyChecksumHeader(m); err != nil {
			_, _ = fmt.Fprintf(stderr, "check: %v\n", err)
			return 1
		}
	}

	schemaBytes, err := os.ReadFile(schemaPath)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "check: read %s: %v\n", schemaPath, err)
		return 1
	}
	desired, err := schemadiff.Parse(ctx, string(schemaBytes))
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "check: parse %s: %v\n", schemaPath, err)
		return 1
	}

	parts := make([]string, len(migrations))
	for i, m := range migrations {
		parts[i] = m.SQL
	}
	replayed, err := schemadiff.Parse(ctx, strings.Join(parts, "\n"))
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "check: replay migration journal: %v\n", err)
		return 1
	}

	diff := schemadiff.Diff(replayed, desired)
	if !diff.Empty() {
		_, _ = fmt.Fprintf(stderr, "check: migration journal does not reproduce %s exactly (drift check failed): %+v\n", schemaPath, diff)
		return 1
	}

	_, _ = fmt.Fprintf(stdout, "ok: %d migration(s) verified\n", len(migrations))
	return 0
}

// verifyChecksumHeader recomputes a migration file's checksum over its
// body (everything after the leading checksum comment) and compares it
// against the checksum that comment records, catching a file edited after
// being generated.
func verifyChecksumHeader(m sqlitemigrate.Migration) error {
	match := checksumHeaderPattern.FindStringSubmatch(m.SQL)
	if match == nil {
		return fmt.Errorf("%s: missing checksum header", m.Filename)
	}
	body := m.SQL[len(match[0]):]
	sum := sha256.Sum256([]byte(body))
	got := hex.EncodeToString(sum[:])
	if got != match[1] {
		return fmt.Errorf("%s: checksum mismatch: header records %s, body hashes to %s (file was edited after being generated)", m.Filename, match[1], got)
	}
	return nil
}
