// Package rebuild generates the full 12-step SQL sequence SQLite's native
// ALTER TABLE can't express: create the new table, copy rows across with
// an explicit column mapping, drop the old table, rename the new one into
// place, and recreate everything that referenced the original.
package rebuild

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/mdg-labs/sqlite-migrate/internal/schemadiff"
	"github.com/mdg-labs/sqlite-migrate/internal/sqlident"
)

// Generate produces the SQL for rebuilding every table in diffs, given the
// full schema.sql texts the diff's "before" and "after" sides were parsed
// from (needed to recover the trigger and view definitions, and the
// generated columns, schemadiff.Schema doesn't carry — see preserve.go).
// The before side matters as much as the after side: a trigger or view the
// migration removes still exists in the database until it's dropped, and
// breaks the rebuilt table's RENAME just like one that survives. Every diff in diffs is assumed to already be a table
// the caller has decided needs a full rebuild; Generate itself makes no
// safe/destructive or rebuild-vs-plain-ALTER decision.
//
// Multi-table rebuild ordering follows the spec's decision: every affected
// table's create-and-copy step runs before any of their drops, ordered by
// FK dependency (parent tables first) so a row can always find its
// also-being-rebuilt referenced table during the copy step. A genuine FK
// cycle across rebuilt tables can't be fully ordered; it's resolved by the
// runtime's foreign_key_check-at-commit-only design (Runner.Apply, Phase
// 5), not by a topological order that can't exist for a cycle.
//
// Every trigger and view the output drops is also recreated from its
// after-schema definition if the after schema still defines it, so the
// output owns those objects outright: a caller combining it with other
// migration statements must not create or drop them again.
//
// The returned SQL contains only the rebuild DDL/DML statements — no
// PRAGMA or transaction wrapping, since Runner.Apply wraps every migration
// file in one transaction with foreign keys suspended, uniformly for every
// kind of migration.
func Generate(ctx context.Context, beforeSchemaSQL, afterSchemaSQL string, diffs []schemadiff.TableDiff) (string, error) {
	stmts, err := Statements(ctx, beforeSchemaSQL, afterSchemaSQL, diffs)
	if err != nil {
		return "", err
	}
	if len(stmts) == 0 {
		return "", nil
	}
	return strings.Join(stmts, "\n\n") + "\n", nil
}

// Statements is Generate's output as individual, already-terminated ("; "
// suffixed) SQL statements rather than one joined string — the form a
// caller needs to execute the migration statement-by-statement (as
// Runner.Apply does) instead of relying on the driver to split a
// multi-statement string itself.
func Statements(ctx context.Context, beforeSchemaSQL, afterSchemaSQL string, diffs []schemadiff.TableDiff) ([]string, error) {
	if len(diffs) == 0 {
		return nil, nil
	}

	ordered := orderByDependency(diffs)

	tableNames := make([]string, len(ordered))
	for i, td := range ordered {
		tableNames[i] = td.Name
	}

	beforeCat, err := loadCatalog(ctx, beforeSchemaSQL, tableNames)
	if err != nil {
		return nil, fmt.Errorf("rebuild: before schema: %w", err)
	}
	afterCat, err := loadCatalog(ctx, afterSchemaSQL, tableNames)
	if err != nil {
		return nil, fmt.Errorf("rebuild: after schema: %w", err)
	}

	prepared := make([]tableRebuild, len(ordered))
	for i, td := range ordered {
		key := asciiLower(td.Name)
		rb, err := rebuildTable(td, beforeCat.columns[key], afterCat.columns[key])
		if err != nil {
			return nil, fmt.Errorf("rebuild: table %q: %w", td.Name, err)
		}
		prepared[i] = rb
	}

	drop := mergeAffected(affectedObjects(tableNames, beforeCat), affectedObjects(tableNames, afterCat))
	recreate := stillDefined(drop, afterCat)

	var stmts []string
	// Every trigger or view that references a rebuilt table — or another
	// object already being dropped, transitively — must be dropped before
	// any rebuilt table's own CREATE and recreated only after every rebuilt
	// table's RENAME: SQLite's ALTER TABLE RENAME recompiles every other
	// trigger and view in the schema to track the rename, and fails if any
	// of them currently reference a table missing for the whole rebuild
	// window — see affectedObjects.
	stmts = append(stmts, dropAffectedStatements(drop)...)
	for _, rb := range prepared {
		stmts = append(stmts, rb.create, rb.copy)
		stmts = append(stmts, rb.seqCarry...)
	}
	for _, rb := range prepared {
		stmts = append(stmts, rb.drop, rb.rename)
	}
	for _, td := range ordered {
		stmts = append(stmts, indexStatements(td)...)
	}
	stmts = append(stmts, recreateAffectedStatements(recreate)...)

	return stmts, nil
}

type tableRebuild struct {
	create   string
	copy     string
	drop     string
	rename   string
	seqCarry []string
}

// newTableName is the temporary name the rebuilt table is created under
// before it's renamed into place; it must not collide with any table name
// already present in the schema, which the "_sqlite_migrate_new" suffix
// makes vanishingly unlikely for a real schema.sql.
func newTableName(table string) string {
	return table + "_sqlite_migrate_new"
}

func rebuildTable(td schemadiff.TableDiff, beforeCols, afterCols []tableColumn) (tableRebuild, error) {
	tmpName := newTableName(td.Name)

	createSQL, err := renameCreateTableSQL(td.After.SQL, tmpName)
	if err != nil {
		return tableRebuild{}, err
	}

	// beforeCols includes generated columns: a column that was generated
	// before and is a plain column after still has a value to carry over,
	// and reading a generated column in the SELECT is always allowed.
	beforeByName := make(map[string]tableColumn, len(beforeCols))
	for _, c := range beforeCols {
		beforeByName[asciiLower(c.name)] = c
	}

	var targetCols, sourceCols []string
	rowidTarget, rowidSource, carryRowid := rowidColumn(td, beforeCols, afterCols)
	if carryRowid {
		targetCols = append(targetCols, rowidTarget)
		sourceCols = append(sourceCols, rowidSource)
	}
	for _, c := range afterCols {
		if c.generated {
			// SQLite rejects an INSERT naming a generated column; its value
			// is recomputed from the copied columns instead.
			continue
		}
		if carryRowid && quoteIdent(c.name) == rowidTarget {
			continue
		}
		bc, ok := beforeByName[asciiLower(c.name)]
		if !ok {
			// A column only present after the change (e.g. added alongside
			// a type change elsewhere in the same table) has no source
			// value to copy; SQLite fills its DEFAULT (or NULL) for every
			// existing row instead.
			continue
		}
		targetCols = append(targetCols, quoteIdent(c.name))
		sourceCols = append(sourceCols, quoteIdent(bc.name))
	}

	// A rowid mapped into a new alias column alone isn't a carried column:
	// with every real column replaced there is still nothing to copy.
	if len(targetCols) == 0 || (len(targetCols) == 1 && carryRowid && rowidTarget != rowidSource) {
		return tableRebuild{}, fmt.Errorf("no column or rowid carries over from the old table, so the copy step has nothing to select")
	}

	copySQL := fmt.Sprintf(
		"INSERT INTO %s (%s)\nSELECT %s\nFROM %s;",
		quoteIdent(tmpName),
		strings.Join(targetCols, ", "),
		strings.Join(sourceCols, ", "),
		quoteIdent(td.Name),
	)

	rb := tableRebuild{
		create: strings.TrimRight(createSQL, "; \t\n") + ";",
		copy:   copySQL,
		drop:   fmt.Sprintf("DROP TABLE %s;", quoteIdent(td.Name)),
		rename: fmt.Sprintf("ALTER TABLE %s RENAME TO %s;", quoteIdent(tmpName), quoteIdent(td.Name)),
	}
	if hasAutoincrement(td.After.SQL) {
		rb.seqCarry = autoincrementCarryStatements(td.Name, tmpName)
	}
	return rb, nil
}

// rowidColumn returns the target and source names to copy a rowid table's
// rowid through, when it isn't already carried by an INTEGER PRIMARY KEY
// alias column present on both sides. Without it the copy renumbers every
// row, silently breaking anything that stores rowids — an FTS5
// external-content index (content='t') most of all. When the after table
// has an alias column the before table lacks, the old rowid is copied into
// that column. Otherwise it's copied through the first of SQLite's three
// rowid spellings that no real column on either side shadows; a table
// shadowing all three has no way to name its rowid at all, so there is
// nothing to copy.
func rowidColumn(td schemadiff.TableDiff, beforeCols, afterCols []tableColumn) (target, source string, ok bool) {
	if td.Before.WithoutRowID || td.After.WithoutRowID {
		return "", "", false
	}
	beforeTaken := make(map[string]bool, len(beforeCols))
	for _, c := range beforeCols {
		beforeTaken[asciiLower(c.name)] = true
	}
	if hasRowidAlias(td.After) {
		alias := rowidAliasName(td.After)
		if beforeTaken[asciiLower(alias)] {
			return "", "", false
		}
		for _, name := range []string{"rowid", "_rowid_", "oid"} {
			if !beforeTaken[name] {
				return quoteIdent(alias), name, true
			}
		}
		return "", "", false
	}
	taken := make(map[string]bool, len(beforeCols)+len(afterCols))
	for k := range beforeTaken {
		taken[k] = true
	}
	for _, c := range afterCols {
		taken[asciiLower(c.name)] = true
	}
	for _, name := range []string{"rowid", "_rowid_", "oid"} {
		if !taken[name] {
			return name, name, true
		}
	}
	return "", "", false
}

// rowidAliasName is the name of the single primary-key column of a table
// hasRowidAlias has already confirmed aliases its rowid.
func rowidAliasName(t *schemadiff.Table) string {
	for _, c := range t.Columns {
		if c.PrimaryKeySeq > 0 {
			return c.Name
		}
	}
	return ""
}

// hasRowidAlias reports whether a rowid table's primary key is an alias for
// its rowid. SQLite builds a separate "pk"-origin index for every primary
// key except exactly that alias case — which, unlike matching the declared
// type against INTEGER, also gets INTEGER PRIMARY KEY DESC (not an alias)
// right.
func hasRowidAlias(t *schemadiff.Table) bool {
	if t.WithoutRowID {
		return false
	}
	hasPK := false
	for _, c := range t.Columns {
		if c.PrimaryKeySeq > 0 {
			hasPK = true
			break
		}
	}
	if !hasPK {
		return false
	}
	for _, idx := range t.Indexes {
		if idx.Origin == "pk" {
			return false
		}
	}
	return true
}

// autoincrementCarryStatements carries the old table's sqlite_sequence
// high-water mark over to the temporary table before the old table (and
// its own sqlite_sequence row) is dropped. Without this, AUTOINCREMENT's
// never-reuse guarantee breaks across a rebuild: the copy step's INSERT
// only advances the new table's sqlite_sequence row to the highest
// surviving row's id, which can be lower than the old table's high-water
// mark once any row has ever been deleted.
//
// The UPDATE takes the higher of the two values (a no-op unless a row was
// deleted); the INSERT covers the case where the copy produced no rows at
// all, so the UPDATE has no tmpName row to raise.
func autoincrementCarryStatements(oldName, tmpName string) []string {
	oldLit := quoteLiteral(oldName)
	tmpLit := quoteLiteral(tmpName)
	return []string{
		fmt.Sprintf(
			"UPDATE sqlite_sequence SET seq = (SELECT MAX(seq) FROM sqlite_sequence WHERE name IN (%s, %s)) WHERE name = %s;",
			tmpLit, oldLit, tmpLit,
		),
		fmt.Sprintf(
			"INSERT INTO sqlite_sequence (name, seq)\nSELECT %s, seq FROM sqlite_sequence WHERE name = %s\nAND NOT EXISTS (SELECT 1 FROM sqlite_sequence WHERE name = %s);",
			tmpLit, oldLit, tmpLit,
		),
	}
}

// orderByDependency returns diffs ordered so that, for the create-and-copy
// phase, a table whose After foreign keys target another table also being
// rebuilt is ordered after that parent table. A genuine cycle among the
// rebuilt tables can't be fully ordered; visiting stops at the first
// already-in-progress node and falls back to the deterministic name order,
// since correctness for a cycle depends on the commit-time foreign_key_check
// rather than statement order (see the spec's "Multi-table rebuild ordering"
// decision).
func orderByDependency(diffs []schemadiff.TableDiff) []schemadiff.TableDiff {
	byKey := make(map[string]schemadiff.TableDiff, len(diffs))
	keys := make([]string, 0, len(diffs))
	for _, td := range diffs {
		k := asciiLower(td.Name)
		byKey[k] = td
		keys = append(keys, k)
	}
	sort.Strings(keys)

	visited := make(map[string]bool, len(keys))
	inProgress := make(map[string]bool, len(keys))
	order := make([]string, 0, len(keys))

	var visit func(key string)
	visit = func(key string) {
		if visited[key] || inProgress[key] {
			return
		}
		inProgress[key] = true

		td := byKey[key]
		deps := make([]string, 0, len(td.After.ForeignKeys))
		for _, fk := range td.After.ForeignKeys {
			dep := asciiLower(fk.Table)
			if dep == key {
				continue
			}
			if _, ok := byKey[dep]; ok {
				deps = append(deps, dep)
			}
		}
		sort.Strings(deps)
		for _, d := range deps {
			visit(d)
		}

		inProgress[key] = false
		visited[key] = true
		order = append(order, key)
	}

	for _, k := range keys {
		visit(k)
	}

	result := make([]schemadiff.TableDiff, 0, len(order))
	for _, k := range order {
		result = append(result, byKey[k])
	}
	return result
}

// asciiLower folds ASCII letters to lower case, matching SQLite's own
// case-insensitive identifier comparison (which never applies Unicode
// case-folding rules) and internal/schemadiff's identity key.
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

// quoteIdent double-quote-wraps a SQL identifier; see internal/sqlident.QuoteIdent.
func quoteIdent(name string) string {
	return sqlident.QuoteIdent(name)
}

// quoteLiteral single-quote-wraps a string for use as a SQL text literal
// (as opposed to quoteIdent's double-quoted identifier), doubling any
// embedded single quote.
func quoteLiteral(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "''") + "'"
}

// hasAutoincrement reports whether a table's verbatim CREATE TABLE text
// declares AUTOINCREMENT as an actual keyword token — never a mention
// inside a `--`/`/* */` comment, a `'...'` string literal (e.g. a CHECK
// constraint's allowed values), or a quoted identifier, all of which
// sqlite_master.sql preserves verbatim alongside the real DDL.
func hasAutoincrement(sql string) bool {
	i, n := 0, len(sql)
	for i < n {
		c := sql[i]
		switch {
		case c == '-' && i+1 < n && sql[i+1] == '-':
			j := strings.IndexByte(sql[i:], '\n')
			if j < 0 {
				return false
			}
			i += j + 1
		case c == '/' && i+1 < n && sql[i+1] == '*':
			j := strings.Index(sql[i+2:], "*/")
			if j < 0 {
				return false
			}
			i += 2 + j + 2
		case c == '\'' || c == '"' || c == '`':
			j := skipQuoted(sql, i, c)
			i = j
		case c == '[':
			j := strings.IndexByte(sql[i:], ']')
			if j < 0 {
				return false
			}
			i += j + 1
		case sqlident.IsIdentByte(c):
			j := i
			for j < n && sqlident.IsIdentByte(sql[j]) {
				j++
			}
			if strings.EqualFold(sql[i:j], "autoincrement") {
				return true
			}
			i = j
		default:
			i++
		}
	}
	return false
}

// skipQuoted returns the index just past the quoted run starting at i (which
// must hold the opening quote byte), treating a doubled quote as an escaped
// quote character rather than the end of the run — the rule shared by SQL
// string literals ('...') and quoted identifiers ("..." and `...`).
// An unterminated run (impossible in valid SQL text, but not assumed here)
// is treated as extending to the end of the string.
func skipQuoted(s string, i int, q byte) int {
	j := i + 1
	for j < len(s) {
		if s[j] == q {
			if j+1 < len(s) && s[j+1] == q {
				j += 2
				continue
			}
			return j + 1
		}
		j++
	}
	return j
}

// containsIdentifierWord reports whether lowerNeedle occurs in
// lowerHaystack at an identifier word boundary — not as part of a longer
// identifier on either side. Both arguments must already be lower-cased.
// lowerNeedle == "" always reports false: found by Phase 8 fuzzing, an
// empty needle matches at every position (per strings.Index's own
// contract), which grew from without bound and eventually panicked on an
// out-of-range slice — and a bare, unquoted identifier can never be empty
// in real SQL syntax anyway (SQLite only allows an empty name quoted, e.g.
// CREATE TABLE "" (...), which referencesTable's quotedForms check already
// covers on its own).
func containsIdentifierWord(lowerHaystack, lowerNeedle string) bool {
	if lowerNeedle == "" {
		return false
	}
	from := 0
	for {
		i := strings.Index(lowerHaystack[from:], lowerNeedle)
		if i < 0 {
			return false
		}
		pos := from + i
		before := byte(' ')
		if pos > 0 {
			before = lowerHaystack[pos-1]
		}
		afterPos := pos + len(lowerNeedle)
		after := byte(' ')
		if afterPos < len(lowerHaystack) {
			after = lowerHaystack[afterPos]
		}
		if !sqlident.IsIdentByte(before) && !sqlident.IsIdentByte(after) {
			return true
		}
		from = pos + 1
	}
}

// renameCreateTableSQL rewrites a table's own verbatim CREATE TABLE text
// (as read back from sqlite_master.sql) to create it under newName instead,
// leaving every column, constraint, and table-level option (STRICT,
// WITHOUT ROWID) untouched. It never reconstructs the statement from parsed
// fields — those don't capture everything schema.sql can express (CHECK
// expressions, COLLATE, GENERATED ALWAYS AS columns), so only editing the
// verbatim source text preserves them exactly.
func renameCreateTableSQL(sql, newName string) (string, error) {
	i := 0
	n := len(sql)

	skipSpace := func() {
		for i < n && isSpaceByte(sql[i]) {
			i++
		}
	}
	matchKeyword := func(kw string) bool {
		skipSpace()
		if i+len(kw) > n || !strings.EqualFold(sql[i:i+len(kw)], kw) {
			return false
		}
		end := i + len(kw)
		if end < n && sqlident.IsIdentByte(sql[end]) {
			return false
		}
		i = end
		return true
	}

	if !matchKeyword("CREATE") {
		return "", fmt.Errorf("rebuild: expected CREATE TABLE, got: %.40q", sql)
	}
	save := i
	if !matchKeyword("TEMPORARY") {
		i = save
		matchKeyword("TEMP")
	}
	if !matchKeyword("TABLE") {
		return "", fmt.Errorf("rebuild: expected TABLE keyword, got: %.40q", sql)
	}
	save = i
	if matchKeyword("IF") {
		if !matchKeyword("NOT") || !matchKeyword("EXISTS") {
			return "", fmt.Errorf("rebuild: malformed IF NOT EXISTS clause: %.40q", sql)
		}
	} else {
		i = save
	}

	skipSpace()
	end, err := scanIdentifier(sql, i)
	if err != nil {
		return "", fmt.Errorf("rebuild: %w: %.40q", err, sql)
	}

	return sql[:i] + quoteIdent(newName) + sql[end:], nil
}

func scanIdentifier(s string, i int) (int, error) {
	if i >= len(s) {
		return 0, fmt.Errorf("expected identifier at end of input")
	}
	switch s[i] {
	case '"', '`':
		q := s[i]
		j := i + 1
		for j < len(s) {
			if s[j] == q {
				if j+1 < len(s) && s[j+1] == q {
					j += 2
					continue
				}
				return j + 1, nil
			}
			j++
		}
		return 0, fmt.Errorf("unterminated quoted identifier")
	case '[':
		j := strings.IndexByte(s[i:], ']')
		if j < 0 {
			return 0, fmt.Errorf("unterminated bracket identifier")
		}
		return i + j + 1, nil
	default:
		j := i
		for j < len(s) && sqlident.IsIdentByte(s[j]) {
			j++
		}
		if j == i {
			return 0, fmt.Errorf("expected identifier")
		}
		return j, nil
	}
}

func isSpaceByte(c byte) bool {
	return c == ' ' || c == '\t' || c == '\n' || c == '\r' || c == '\v' || c == '\f'
}
