// generate.go implements the `generate` subcommand: it replays the
// existing migration journal, diffs the result against schema.sql,
// resolves rename ambiguity, routes each change to internal/sqldefwrap or
// internal/rebuild, gates destructive changes behind --allow-destructive,
// verifies the candidate via a replay-based drift check, and writes the
// migration file.
package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/mdg-labs/sqlite-migrate/internal/rebuild"
	"github.com/mdg-labs/sqlite-migrate/internal/rename"
	"github.com/mdg-labs/sqlite-migrate/internal/schemadiff"
	"github.com/mdg-labs/sqlite-migrate/internal/sqldefwrap"
)

// timestampLayout is the sortable version identifier every migration file
// name starts with (spec doc, "Concurrent schema authors" decision).
const timestampLayout = "20060102150405"

// generateOptions holds generate's parsed flags and everything else it
// needs to run without touching the process environment directly, so the
// whole command is testable without a real terminal or clock.
type generateOptions struct {
	schemaPath       string
	migrationsDir    string
	message          string
	allowDestructive bool
	assumeRenames    bool
	assumeNoRenames  bool
	now              func() time.Time
}

// RunGenerate is the CLI entry point for `sqlite-migrate generate`: it
// parses args, runs generate, and reports the outcome to stdout/stderr,
// returning the process exit code.
func RunGenerate(args []string, stdin io.Reader, stdout, stderr io.Writer, now func() time.Time) int {
	fs := flag.NewFlagSet("generate", flag.ContinueOnError)
	fs.SetOutput(stderr)
	opts := generateOptions{now: now}
	fs.StringVar(&opts.schemaPath, "schema", "schema.sql", "path to the schema.sql source of truth")
	fs.StringVar(&opts.migrationsDir, "dir", "migrations", "directory holding generated migration files")
	fs.StringVar(&opts.message, "m", "", "optional slug message for the generated migration file name")
	fs.StringVar(&opts.message, "message", "", "optional slug message for the generated migration file name")
	fs.BoolVar(&opts.allowDestructive, "allow-destructive", false, "generate a migration even if it would drop a table or column")
	fs.BoolVar(&opts.assumeRenames, "assume-renames", false, "treat every ambiguous drop+add pair as a rename without prompting")
	fs.BoolVar(&opts.assumeNoRenames, "assume-no-renames", false, "treat every ambiguous drop+add pair as a genuine drop+add without prompting")
	if err := fs.Parse(args); err != nil {
		return 2
	}

	result, err := generate(context.Background(), opts, stdin, stdout)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "generate: %v\n", err)
		return 1
	}
	if result.written {
		_, _ = fmt.Fprintf(stdout, "wrote %s\n", result.path)
	} else {
		_, _ = fmt.Fprintln(stdout, "no changes detected")
	}
	return 0
}

type generateResult struct {
	written bool
	path    string
}

// generate runs the full generate pipeline described at the top of this
// file and returns whether a migration file was written and, if so, its
// path. An error means generate refused to write anything.
func generate(ctx context.Context, opts generateOptions, stdin io.Reader, stdout io.Writer) (generateResult, error) {
	schemaBytes, err := os.ReadFile(opts.schemaPath)
	if err != nil {
		return generateResult{}, fmt.Errorf("read %s: %w", opts.schemaPath, err)
	}
	desiredDDL := string(schemaBytes)

	desiredSchema, err := schemadiff.Parse(ctx, desiredDDL)
	if err != nil {
		return generateResult{}, err
	}

	journalDDL, err := readJournal(opts.migrationsDir)
	if err != nil {
		return generateResult{}, err
	}

	currentSchema, err := schemadiff.Parse(ctx, journalDDL)
	if err != nil {
		return generateResult{}, fmt.Errorf("replay existing migrations: %w", err)
	}

	rawDiff := schemadiff.Diff(currentSchema, desiredSchema)
	if rawDiff.Empty() {
		return generateResult{}, nil
	}

	resolutions, err := rename.Resolve(ctx, rawDiff, stdin, stdout, rename.Flags{
		AssumeRenames:   opts.assumeRenames,
		AssumeNoRenames: opts.assumeNoRenames,
	})
	if err != nil {
		return generateResult{}, err
	}

	renameSection := buildRenameStatements(resolutions)
	currentDDLResolved := journalDDL
	if renameSection != "" {
		currentDDLResolved = journalDDL + "\n" + renameSection
	}

	currentSchemaResolved, err := schemadiff.Parse(ctx, currentDDLResolved)
	if err != nil {
		return generateResult{}, fmt.Errorf("replay resolved renames: %w", err)
	}

	diff := schemadiff.Diff(currentSchemaResolved, desiredSchema)
	if diff.Empty() && renameSection == "" {
		return generateResult{}, nil
	}

	classification := schemadiff.Classify(diff)
	if classification.Verdict == schemadiff.Destructive && !opts.allowDestructive {
		return generateResult{}, destructiveError(classification)
	}

	body, err := buildMigrationBody(ctx, renameSection, diff, currentSchemaResolved, desiredSchema, currentDDLResolved, desiredDDL)
	if err != nil {
		return generateResult{}, err
	}

	if err := verifyCandidate(ctx, journalDDL, body, desiredSchema); err != nil {
		return generateResult{}, err
	}

	path, err := writeMigrationFile(opts, body, diff)
	if err != nil {
		return generateResult{}, err
	}

	return generateResult{written: true, path: path}, nil
}

// readJournal reconstructs the current schema's DDL by concatenating every
// existing migration file's SQL body in filename order. A missing
// migrations directory is a cold repo, not an error: the journal is empty.
func readJournal(dir string) (string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return "", nil
		}
		return "", fmt.Errorf("read migrations dir %s: %w", dir, err)
	}

	var names []string
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".sql") {
			continue
		}
		names = append(names, e.Name())
	}
	sort.Strings(names)

	var parts []string
	for _, name := range names {
		b, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			return "", fmt.Errorf("read migration %s: %w", name, err)
		}
		parts = append(parts, string(b))
	}
	return strings.Join(parts, "\n"), nil
}

func buildRenameStatements(resolutions []rename.Resolution) string {
	var stmts []string
	for _, r := range resolutions {
		if !r.Confirmed {
			continue
		}
		switch r.Candidate.Kind {
		case rename.TableRename:
			stmts = append(stmts, fmt.Sprintf("ALTER TABLE %s RENAME TO %s;", quoteIdent(r.Candidate.From), quoteIdent(r.Candidate.To)))
		case rename.ColumnRename:
			stmts = append(stmts, fmt.Sprintf("ALTER TABLE %s RENAME COLUMN %s TO %s;", quoteIdent(r.Candidate.Table), quoteIdent(r.Candidate.From), quoteIdent(r.Candidate.To)))
		}
	}
	return strings.Join(stmts, "\n\n")
}

func destructiveError(c schemadiff.Classification) error {
	var b strings.Builder
	b.WriteString("refusing to generate a destructive migration:\n")
	for _, t := range c.RemovedTables {
		fmt.Fprintf(&b, "  table dropped: %s\n", t)
	}
	tables := make([]string, 0, len(c.RemovedColumns))
	for t := range c.RemovedColumns {
		tables = append(tables, t)
	}
	sort.Strings(tables)
	for _, t := range tables {
		fmt.Fprintf(&b, "  %s: column(s) dropped: %s\n", t, strings.Join(c.RemovedColumns[t], ", "))
	}
	b.WriteString("rerun with --allow-destructive to generate this migration anyway")
	return errors.New(b.String())
}

// buildMigrationBody routes every entry in diff to the tool that can
// generate it — internal/rebuild for anything a plain ALTER TABLE can't
// express, internal/sqldefwrap for the additive column changes it already
// gets right, and this package directly for the cases neither needs
// (new/removed tables, explicit index changes, table/column drops) — and
// assembles the result in a fixed, deterministic order.
func buildMigrationBody(ctx context.Context, renameSection string, diff *schemadiff.SchemaDiff, current, desired *schemadiff.Schema, currentDDL, desiredDDL string) (string, error) {
	var sqldefStmts []string
	var directStmts []string
	var rebuildDiffs []schemadiff.TableDiff

	for _, td := range diff.ChangedTables {
		if needsRebuild(td) {
			rebuildDiffs = append(rebuildDiffs, td)
			continue
		}
		if len(td.AddedColumns) > 0 {
			stmts, err := addColumnStatements(td, current, desired)
			if err != nil {
				return "", err
			}
			sqldefStmts = append(sqldefStmts, stmts...)
		}
		for _, idx := range td.AddedIndexes {
			if idx.Origin == "c" && idx.SQL != "" {
				directStmts = append(directStmts, idx.SQL+";")
			}
		}
		for _, idx := range td.RemovedIndexes {
			if idx.Origin == "c" {
				directStmts = append(directStmts, fmt.Sprintf("DROP INDEX %s;", quoteIdent(idx.Name)))
			}
		}
	}

	for _, t := range diff.AddedTables {
		directStmts = append(directStmts, t.SQL+";")
		for _, idx := range t.Indexes {
			if idx.Origin == "c" && idx.SQL != "" {
				directStmts = append(directStmts, idx.SQL+";")
			}
		}
	}
	for _, t := range diff.RemovedTables {
		directStmts = append(directStmts, fmt.Sprintf("DROP TABLE %s;", quoteIdent(t.Name)))
	}

	rebuildSQL := ""
	if len(rebuildDiffs) > 0 {
		sql, err := rebuild.Generate(ctx, currentDDL, desiredDDL, rebuildDiffs)
		if err != nil {
			return "", fmt.Errorf("rebuild: %w", err)
		}
		rebuildSQL = strings.TrimSuffix(sql, "\n")
	}

	sections := []string{
		renameSection,
		strings.Join(sqldefStmts, "\n\n"),
		strings.Join(directStmts, "\n\n"),
		rebuildSQL,
	}
	var nonEmpty []string
	for _, s := range sections {
		if s != "" {
			nonEmpty = append(nonEmpty, s)
		}
	}
	return strings.Join(nonEmpty, "\n\n") + "\n", nil
}

// needsRebuild decides whether a changed table requires the full 12-step
// rebuild rather than a direct ALTER TABLE: anything removed, any existing
// column's definition changed, any foreign key removed or added onto a
// column that already existed, any index change that isn't a plain
// explicit CREATE INDEX (an implied unique/primary-key index means an
// inline constraint changed), or a CREATE TABLE text change with no new
// column to explain it (a CHECK/COLLATE/GENERATED change PRAGMA
// introspection can't see any other way — see schemadiff.TableDiff.SQLChanged).
func needsRebuild(td schemadiff.TableDiff) bool {
	if len(td.RemovedColumns) > 0 || len(td.ChangedColumns) > 0 || len(td.RemovedForeignKeys) > 0 {
		return true
	}
	for _, fk := range td.AddedForeignKeys {
		if !isAddedColumn(td, fk.From) {
			return true
		}
	}
	for _, idx := range td.AddedIndexes {
		if idx.Origin != "c" {
			return true
		}
	}
	for _, idx := range td.RemovedIndexes {
		if idx.Origin != "c" {
			return true
		}
	}
	if td.SQLChanged && len(td.AddedColumns) == 0 {
		return true
	}
	return false
}

func isAddedColumn(td schemadiff.TableDiff, name string) bool {
	for _, c := range td.AddedColumns {
		if strings.EqualFold(c.Name, name) {
			return true
		}
	}
	return false
}

// addColumnStatements generates the ADD COLUMN statements for a table
// whose only changes are new columns, via internal/sqldefwrap — the one
// package that already handles folding a REFERENCES clause into ADD
// COLUMN correctly. It scopes the call to just this table and any table
// its new columns reference, rather than the whole schema, so sqldef never
// sees (and can't misjudge) any other table's unrelated changes.
func addColumnStatements(td schemadiff.TableDiff, current, desired *schemadiff.Schema) ([]string, error) {
	names := map[string]bool{strings.ToLower(td.Name): true}
	for _, fk := range td.AddedForeignKeys {
		names[strings.ToLower(fk.Table)] = true
	}

	sortedNames := make([]string, 0, len(names))
	for n := range names {
		sortedNames = append(sortedNames, n)
	}
	sort.Strings(sortedNames)

	var beforeParts, afterParts []string
	for _, n := range sortedNames {
		if t := findTable(current, n); t != nil {
			beforeParts = append(beforeParts, t.SQL+";")
		}
		if t := findTable(desired, n); t != nil {
			afterParts = append(afterParts, t.SQL+";")
		}
	}

	ddls, err := sqldefwrap.New().Diff(strings.Join(afterParts, "\n"), strings.Join(beforeParts, "\n"))
	if err != nil {
		return nil, fmt.Errorf("sqldefwrap: %w", err)
	}
	stmts := make([]string, len(ddls))
	for i, ddl := range ddls {
		stmts[i] = ddl + ";"
	}
	return stmts, nil
}

func findTable(s *schemadiff.Schema, name string) *schemadiff.Table {
	for _, t := range s.Tables {
		if strings.EqualFold(t.Name, name) {
			return t
		}
	}
	return nil
}

// verifyCandidate is the replay-based drift check Core Design Principle 6
// requires before any generated migration is written to disk: replay the
// journal plus the candidate migration into a fresh temporary database,
// then compare the result against schema.sql structurally. A candidate
// that fails to apply, or applies but doesn't reproduce schema.sql
// exactly, is refused here rather than discovered later.
func verifyCandidate(ctx context.Context, journalDDL, candidateBody string, desired *schemadiff.Schema) error {
	replayed, err := schemadiff.Parse(ctx, journalDDL+"\n"+candidateBody)
	if err != nil {
		return fmt.Errorf("candidate migration failed to apply during verification: %w", err)
	}
	drift := schemadiff.Diff(replayed, desired)
	if !drift.Empty() {
		return fmt.Errorf("candidate migration does not reproduce schema.sql exactly (drift check failed): %+v", drift)
	}
	return nil
}

var slugSanitizer = regexp.MustCompile(`[^a-z0-9]+`)

func slugify(s string) string {
	s = slugSanitizer.ReplaceAllString(strings.ToLower(strings.TrimSpace(s)), "_")
	s = strings.Trim(s, "_")
	if s == "" {
		return "schema_change"
	}
	return s
}

// deriveSlug picks the migration file's cosmetic slug: the -m message if
// one was given, otherwise a name derived from the single table the diff
// centers on, falling back to "schema_change" when several unrelated
// tables changed and no single name is representative (spec doc,
// "Migration file naming").
func deriveSlug(message string, diff *schemadiff.SchemaDiff) string {
	if message != "" {
		return slugify(message)
	}

	total := len(diff.AddedTables) + len(diff.RemovedTables) + len(diff.ChangedTables)
	if total != 1 {
		return "schema_change"
	}

	switch {
	case len(diff.AddedTables) == 1:
		return slugify("create_" + diff.AddedTables[0].Name)
	case len(diff.RemovedTables) == 1:
		return slugify("drop_" + diff.RemovedTables[0].Name)
	default:
		td := diff.ChangedTables[0]
		if len(td.AddedColumns) == 1 {
			return slugify("add_" + td.Name + "_" + td.AddedColumns[0].Name)
		}
		return slugify("update_" + td.Name)
	}
}

func quoteIdent(name string) string {
	return `"` + strings.ReplaceAll(name, `"`, `""`) + `"`
}

// writeMigrationFile picks a unique timestamp, computes the migration's
// checksum, and writes "<timestamp>_<slug>.sql" to the migrations
// directory. The checksum is recorded as a leading SQL comment (harmless
// to replay) rather than a sidecar file, so a later `check`/`apply` can
// verify it without any extra bookkeeping file to keep in sync.
func writeMigrationFile(opts generateOptions, body string, diff *schemadiff.SchemaDiff) (string, error) {
	if err := os.MkdirAll(opts.migrationsDir, 0o755); err != nil {
		return "", fmt.Errorf("create migrations dir %s: %w", opts.migrationsDir, err)
	}

	ts, err := nextTimestamp(opts.migrationsDir, opts.now())
	if err != nil {
		return "", err
	}
	slug := deriveSlug(opts.message, diff)
	filename := fmt.Sprintf("%s_%s.sql", ts, slug)
	path := filepath.Join(opts.migrationsDir, filename)

	sum := sha256.Sum256([]byte(body))
	content := fmt.Sprintf("-- sqlite-migrate: checksum %s\n\n%s", hex.EncodeToString(sum[:]), body)

	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		return "", fmt.Errorf("write migration file %s: %w", path, err)
	}
	return path, nil
}

// nextTimestamp formats t per timestampLayout, advancing by one second at
// a time if that timestamp is already used by an existing migration file —
// two generates within the same wall-clock second (e.g. under test with a
// fixed clock) must still get distinct, monotonically increasing versions.
func nextTimestamp(dir string, t time.Time) (string, error) {
	t = t.UTC()
	for {
		candidate := t.Format(timestampLayout)
		matches, err := filepath.Glob(filepath.Join(dir, candidate+"_*.sql"))
		if err != nil {
			return "", fmt.Errorf("check existing migration timestamps: %w", err)
		}
		if len(matches) == 0 {
			return candidate, nil
		}
		t = t.Add(time.Second)
	}
}
