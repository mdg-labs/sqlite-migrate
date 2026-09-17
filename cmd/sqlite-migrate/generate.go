// generate.go implements the `generate` subcommand: it replays the
// existing migration journal, diffs the result against schema.sql,
// resolves rename ambiguity, routes each change to internal/sqldefwrap or
// internal/rebuild, gates destructive changes behind --allow-destructive,
// verifies the candidate via a replay-based drift check, and writes the
// migration file.
package main

import (
	"bufio"
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
	"github.com/mdg-labs/sqlite-migrate/internal/sqlident"
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
	if opts.assumeRenames && opts.assumeNoRenames {
		return generateResult{}, rename.ErrConflictingAssumeFlags
	}

	schemaBytes, err := os.ReadFile(opts.schemaPath)
	if err != nil {
		return generateResult{}, fmt.Errorf("read %s: %w", opts.schemaPath, err)
	}
	desiredDDL := string(schemaBytes)

	desiredSchema, err := schemadiff.Parse(ctx, desiredDDL)
	if err != nil {
		return generateResult{}, fmt.Errorf("parse %s: %w", opts.schemaPath, err)
	}

	journalDDL, err := readJournal(ctx, opts.migrationsDir)
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

	renameFlags := rename.Flags{
		AssumeRenames:   opts.assumeRenames,
		AssumeNoRenames: opts.assumeNoRenames,
	}
	// Shared across every Confirm prompt in this run, including the second
	// rename.Detect pass below: wrapping stdin in a fresh bufio.Reader per
	// call would discard whatever bytes an earlier call already buffered
	// but not consumed (see rename.ResolveCandidates).
	renameIn := bufio.NewReader(stdin)

	resolutions, err := rename.ResolveCandidates(ctx, rename.Detect(rawDiff), renameIn, stdout, renameFlags)
	if err != nil {
		return generateResult{}, err
	}

	renamedTables := make(map[string]bool)
	for _, r := range resolutions {
		if r.Confirmed && r.Candidate.Kind == rename.TableRename {
			renamedTables[r.Candidate.To] = true
		}
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

	// A table rename can reveal a column rename inside the same table that
	// rawDiff had no way to see: at that point the column lived inside the
	// whole dropped/added table pair, never in a ChangedTables entry
	// detectColumnRenames could run against. Re-resolve just the renamed
	// tables' now-visible column diffs so a compound table+column rename
	// made in one schema.sql edit isn't misclassified as a destructive
	// drop+add.
	if len(renamedTables) > 0 {
		var revealed []schemadiff.TableDiff
		for _, td := range diff.ChangedTables {
			if renamedTables[td.Name] {
				revealed = append(revealed, td)
			}
		}
		if len(revealed) > 0 {
			colCandidates := rename.Detect(&schemadiff.SchemaDiff{ChangedTables: revealed})
			colResolutions, err := rename.ResolveCandidates(ctx, colCandidates, renameIn, stdout, renameFlags)
			if err != nil {
				return generateResult{}, err
			}
			if colRenameSection := buildRenameStatements(colResolutions); colRenameSection != "" {
				renameSection = strings.TrimSpace(renameSection + "\n\n" + colRenameSection)
				currentDDLResolved += "\n" + colRenameSection
				currentSchemaResolved, err = schemadiff.Parse(ctx, currentDDLResolved)
				if err != nil {
					return generateResult{}, fmt.Errorf("replay resolved renames: %w", err)
				}
				diff = schemadiff.Diff(currentSchemaResolved, desiredSchema)
			}
		}
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

	path, err := writeMigrationFile(ctx, opts, body, diff)
	if err != nil {
		return generateResult{}, err
	}

	return generateResult{written: true, path: path}, nil
}

// readJournal reconstructs the current schema's DDL by concatenating every
// existing migration file's SQL body in filename order. A missing
// migrations directory is a cold repo, not an error: the journal is empty.
func readJournal(ctx context.Context, dir string) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}

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
		if err := ctx.Err(); err != nil {
			return "", err
		}
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
		full, err := needsRebuild(ctx, td)
		if err != nil {
			return "", err
		}
		if full {
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
// inline constraint changed), or a CREATE TABLE text change that isn't
// fully explained by the added columns themselves (a CHECK/COLLATE/
// GENERATED change PRAGMA introspection can't see any other way — see
// schemadiff.TableDiff.SQLChanged and additiveChangeReproducesAfter).
func needsRebuild(ctx context.Context, td schemadiff.TableDiff) (bool, error) {
	if len(td.RemovedColumns) > 0 || len(td.ChangedColumns) > 0 || len(td.RemovedForeignKeys) > 0 {
		return true, nil
	}
	for _, fk := range td.AddedForeignKeys {
		if !isAddedColumn(td, fk.From) {
			return true, nil
		}
	}
	for _, idx := range td.AddedIndexes {
		if idx.Origin != "c" {
			return true, nil
		}
	}
	for _, idx := range td.RemovedIndexes {
		if idx.Origin != "c" {
			return true, nil
		}
	}
	if !td.SQLChanged {
		return false, nil
	}
	if len(td.AddedColumns) == 0 {
		return true, nil
	}
	ok, err := additiveChangeReproducesAfter(ctx, td)
	if err != nil {
		return false, err
	}
	return !ok, nil
}

// additiveChangeReproducesAfter reports whether generating and applying
// sqldef's additive statements for td's own before/after CREATE TABLE text
// alone reproduces the desired table exactly. This is the only reliable
// way to tell a table whose CREATE TABLE text changed purely because of
// its added columns apart from one that picked up an untracked
// CHECK/COLLATE/GENERATED change in the same schema.sql edit: neither case
// is distinguishable from schemadiff.TableDiff's structured fields alone
// (see SQLChanged), so this replays the candidate additive-only change
// into a scratch database and compares the result structurally, the same
// technique verifyCandidate uses for the whole migration.
func additiveChangeReproducesAfter(ctx context.Context, td schemadiff.TableDiff) (bool, error) {
	ddls, err := sqldefwrap.New().Diff(td.After.SQL, td.Before.SQL)
	if err != nil {
		return false, fmt.Errorf("sqldefwrap: %w", err)
	}
	stmts := make([]string, len(ddls))
	for i, ddl := range ddls {
		stmts[i] = ddl + ";"
	}

	beforeSQL := strings.TrimRight(td.Before.SQL, "; \t\n") + ";"
	replayed, err := schemadiff.Parse(ctx, beforeSQL+"\n"+strings.Join(stmts, "\n"))
	if err != nil {
		return false, nil
	}
	got, ok := replayed.Tables[td.Name]
	if !ok {
		return false, nil
	}

	gotSchema := &schemadiff.Schema{Tables: map[string]*schemadiff.Table{td.Name: got}}
	wantSchema := &schemadiff.Schema{Tables: map[string]*schemadiff.Table{td.Name: td.After}}
	return schemadiff.Diff(gotSchema, wantSchema).Empty(), nil
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
// COLUMN correctly. It scopes the call to just this table and any
// already-existing table its new columns reference, rather than the whole
// schema, so sqldef never sees (and can't misjudge) any other table's
// unrelated changes. A foreign key's target table that doesn't exist yet
// in current is itself a brand-new table, already handled in full by
// buildMigrationBody's own AddedTables loop; scoping it in here too would
// make sqldef see it as newly created and emit its CREATE TABLE a second
// time.
func addColumnStatements(td schemadiff.TableDiff, current, desired *schemadiff.Schema) ([]string, error) {
	names := map[string]bool{asciiLower(td.Name): true}
	for _, fk := range td.AddedForeignKeys {
		if findTable(current, fk.Table) != nil {
			names[asciiLower(fk.Table)] = true
		}
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
	target := asciiLower(name)
	for _, t := range s.Tables {
		if asciiLower(t.Name) == target {
			return t
		}
	}
	return nil
}

// asciiLower folds ASCII letters to lower case, matching SQLite's own
// case-insensitive identifier comparison (which never applies Unicode
// case-folding rules) and internal/schemadiff's identity key — unlike
// strings.ToLower/EqualFold, which fold Unicode case too and so can treat
// two identifiers as equal (or distinct) differently than SQLite itself
// would.
func asciiLower(s string) string {
	b := []byte(s)
	changed := false
	for i, c := range b {
		if c >= 'A' && c <= 'Z' {
			b[i] = c + ('a' - 'A')
			changed = true
		}
	}
	if !changed {
		return s
	}
	return string(b)
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

// quoteIdent double-quote-wraps a SQL identifier; see internal/sqlident.QuoteIdent.
func quoteIdent(name string) string {
	return sqlident.QuoteIdent(name)
}

// writeMigrationFile picks a unique timestamp, computes the migration's
// checksum, and writes "<timestamp>_<slug>.sql" to the migrations
// directory. The checksum is recorded as a leading SQL comment (harmless
// to replay) rather than a sidecar file, so a later `check`/`apply` can
// verify it without any extra bookkeeping file to keep in sync.
func writeMigrationFile(ctx context.Context, opts generateOptions, body string, diff *schemadiff.SchemaDiff) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if err := os.MkdirAll(opts.migrationsDir, 0o755); err != nil {
		return "", fmt.Errorf("create migrations dir %s: %w", opts.migrationsDir, err)
	}

	ts, err := nextTimestamp(ctx, opts.migrationsDir, opts.now())
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
func nextTimestamp(ctx context.Context, dir string, t time.Time) (string, error) {
	t = t.UTC()
	for {
		if err := ctx.Err(); err != nil {
			return "", err
		}
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
