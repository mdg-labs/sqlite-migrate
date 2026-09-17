package schemadiff

import (
	"sort"
	"strings"

	"github.com/mdg-labs/sqlite-migrate/internal/sqlident"
)

// SchemaDiff is the set of table/column/index/foreign-key differences
// between two structured schemas produced by Parse.
type SchemaDiff struct {
	AddedTables   []*Table
	RemovedTables []*Table
	ChangedTables []TableDiff
}

// Empty reports whether the two schemas were identical.
func (d *SchemaDiff) Empty() bool {
	return len(d.AddedTables) == 0 && len(d.RemovedTables) == 0 && len(d.ChangedTables) == 0
}

// TableDiff is what changed about one table that exists in both schemas.
type TableDiff struct {
	Name   string
	Before *Table
	After  *Table

	AddedColumns   []Column
	RemovedColumns []Column
	ChangedColumns []ColumnDiff

	AddedForeignKeys   []ForeignKey
	RemovedForeignKeys []ForeignKey

	AddedIndexes   []Index
	RemovedIndexes []Index

	// SQLChanged is true when the table's own CREATE TABLE text differs
	// even though nothing else tracked above does — the only signal
	// available for constructs PRAGMA introspection doesn't expose at
	// all (CHECK constraints, COLLATE, GENERATED ALWAYS AS). It is never
	// consulted for safe/destructive classification, only to detect that
	// a rebuild-worthy change happened.
	SQLChanged bool
}

// Empty reports whether this table has no detected differences at all.
func (td *TableDiff) Empty() bool {
	return len(td.AddedColumns) == 0 && len(td.RemovedColumns) == 0 && len(td.ChangedColumns) == 0 &&
		len(td.AddedForeignKeys) == 0 && len(td.RemovedForeignKeys) == 0 &&
		len(td.AddedIndexes) == 0 && len(td.RemovedIndexes) == 0 &&
		!td.SQLChanged
}

// ColumnDiff is a column present in both schemas whose definition changed.
type ColumnDiff struct {
	Name   string
	Before Column
	After  Column
}

// Diff compares two structured schemas produced by Parse and returns the
// set of table/column/index/foreign-key differences between them. Table
// identity is matched ASCII-case-insensitively, as SQLite itself treats
// identifiers: a table or column that only changed case is neither added
// nor removed.
func Diff(before, after *Schema) *SchemaDiff {
	d := &SchemaDiff{}

	beforeByKey := make(map[string]*Table, len(before.Tables))
	for _, t := range before.Tables {
		beforeByKey[asciiLower(t.Name)] = t
	}
	afterByKey := make(map[string]*Table, len(after.Tables))
	for _, t := range after.Tables {
		afterByKey[asciiLower(t.Name)] = t
	}

	for _, name := range after.SortedTableNames() {
		if _, ok := beforeByKey[asciiLower(name)]; !ok {
			d.AddedTables = append(d.AddedTables, after.Tables[name])
		}
	}
	for _, name := range before.SortedTableNames() {
		if _, ok := afterByKey[asciiLower(name)]; !ok {
			d.RemovedTables = append(d.RemovedTables, before.Tables[name])
		}
	}

	for _, name := range before.SortedTableNames() {
		beforeTable := before.Tables[name]
		afterTable, ok := afterByKey[asciiLower(name)]
		if !ok {
			continue
		}

		td := diffTable(beforeTable, afterTable)
		if !td.Empty() {
			d.ChangedTables = append(d.ChangedTables, td)
		}
	}

	return d
}

func diffTable(before, after *Table) TableDiff {
	td := TableDiff{Name: before.Name, Before: before, After: after}

	afterCols := make(map[string]Column, len(after.Columns))
	for _, c := range after.Columns {
		afterCols[asciiLower(c.Name)] = c
	}
	beforeCols := make(map[string]Column, len(before.Columns))
	for _, c := range before.Columns {
		beforeCols[asciiLower(c.Name)] = c
	}

	for _, c := range after.Columns {
		beforeCol, ok := beforeCols[asciiLower(c.Name)]
		if !ok {
			td.AddedColumns = append(td.AddedColumns, c)
			continue
		}
		if !columnsEqual(beforeCol, c) {
			td.ChangedColumns = append(td.ChangedColumns, ColumnDiff{Name: c.Name, Before: beforeCol, After: c})
		}
	}
	for _, c := range before.Columns {
		if _, ok := afterCols[asciiLower(c.Name)]; !ok {
			td.RemovedColumns = append(td.RemovedColumns, c)
		}
	}

	td.AddedForeignKeys, td.RemovedForeignKeys = diffForeignKeys(before.ForeignKeys, after.ForeignKeys)
	td.AddedIndexes, td.RemovedIndexes = diffIndexes(before.Indexes, after.Indexes)

	td.SQLChanged = normalizeSQL(before.SQL) != normalizeSQL(after.SQL)

	return td
}

func columnsEqual(a, b Column) bool {
	return a.Type == b.Type &&
		a.NotNull == b.NotNull &&
		a.HasDefault == b.HasDefault &&
		a.DefaultValue == b.DefaultValue &&
		a.PrimaryKeySeq == b.PrimaryKeySeq
}

func diffForeignKeys(before, after []ForeignKey) (added, removed []ForeignKey) {
	// Table, From and To are folded to lower case for the identity key only
	// — a foreign key whose target table or column only changed case is
	// not a different key, matching how Diff matches table/column names.
	key := func(fk ForeignKey) ForeignKey {
		fk.Table = asciiLower(fk.Table)
		fk.From = asciiLower(fk.From)
		fk.To = asciiLower(fk.To)
		return fk
	}

	beforeSet := make(map[ForeignKey]bool, len(before))
	for _, fk := range before {
		beforeSet[key(fk)] = true
	}
	afterSet := make(map[ForeignKey]bool, len(after))
	for _, fk := range after {
		afterSet[key(fk)] = true
	}

	for _, fk := range after {
		if !beforeSet[key(fk)] {
			added = append(added, fk)
		}
	}
	for _, fk := range before {
		if !afterSet[key(fk)] {
			removed = append(removed, fk)
		}
	}
	return added, removed
}

func diffIndexes(before, after []Index) (added, removed []Index) {
	// Comparing sqlite_master.sql (normalized) catches everything the
	// column list and origin/unique flags miss on their own: a changed
	// partial-index WHERE clause, DESC/COLLATE on a column, or a change
	// inside an indexed expression pragma_index_info can't name at all.
	key := func(idx Index) string {
		cols := make([]string, len(idx.Columns))
		for i, c := range idx.Columns {
			cols[i] = asciiLower(c)
		}
		return asciiLower(idx.Name) + "\x00" + idx.Origin + "\x00" + boolStr(idx.Unique) + "\x00" +
			strings.Join(cols, ",") + "\x00" + normalizeSQL(idx.SQL)
	}

	beforeSet := make(map[string]bool, len(before))
	for _, idx := range before {
		beforeSet[key(idx)] = true
	}
	afterSet := make(map[string]bool, len(after))
	for _, idx := range after {
		afterSet[key(idx)] = true
	}

	for _, idx := range after {
		if !beforeSet[key(idx)] {
			added = append(added, idx)
		}
	}
	for _, idx := range before {
		if !afterSet[key(idx)] {
			removed = append(removed, idx)
		}
	}

	sort.Slice(added, func(i, j int) bool { return added[i].Name < added[j].Name })
	sort.Slice(removed, func(i, j int) bool { return removed[i].Name < removed[j].Name })

	return added, removed
}

func boolStr(b bool) string {
	if b {
		return "1"
	}
	return "0"
}

// asciiLower folds ASCII letters to lower case, matching SQLite's own
// case-insensitive identifier comparison (which never applies Unicode
// case-folding rules).
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

// normalizeSQL reduces a CREATE TABLE/INDEX statement, as SQLite echoes it
// back verbatim from the source text, to a token stream so that keyword
// case, comments, identifier-quoting style and the comma/whitespace
// placement ALTER TABLE ADD COLUMN introduces when splicing a new column
// definition into stored SQL don't register as a change. String literal
// contents are preserved exactly, so a genuine change (a CHECK expression,
// a COLLATE clause, a GENERATED ALWAYS AS expression, and so on) still
// compares unequal.
func normalizeSQL(sql string) string {
	tokens := sqlTokens(sql)
	foldIdentifierPositions(tokens)
	texts := make([]string, len(tokens))
	for i, tok := range tokens {
		texts[i] = tok.text
	}
	return strings.Join(texts, " ")
}

// sqlToken is one token of sqlTokens' output. quoted is set only for a
// token unwrapped from a double-quoted run: whether its case should be
// folded depends on where it sits in the statement's grammar, which
// sqlTokens itself has no way to know.
type sqlToken struct {
	text   string
	quoted bool
}

// sqlTokens tokenizes SQL source text: whitespace is discarded, comments
// are stripped, string literals are kept verbatim (including their
// quotes), the backtick- and bracket-quoted identifier forms are unwrapped
// to their bare, lower-cased name, and every other run of identifier
// characters is lower-cased. A double-quoted token is unwrapped but its
// case is kept exactly as written here: SQLite's double-quoted string
// (DQS) fallback treats "…" as a string literal whenever it doesn't name a
// column, and this tokenizer has no schema to tell the two cases apart on
// its own. foldIdentifierPositions folds the case of the ones that
// grammatically must be an identifier (a table or column name) after the
// fact. Everything else (punctuation, operators) becomes a
// single-character token, so that a change in the whitespace around it
// never affects the token stream.
func sqlTokens(s string) []sqlToken {
	var tokens []sqlToken
	n := len(s)
	for i := 0; i < n; {
		c := s[i]
		switch {
		case c == ' ' || c == '\t' || c == '\n' || c == '\r' || c == '\v' || c == '\f':
			i++
		case c == '-' && i+1 < n && s[i+1] == '-':
			j := strings.IndexByte(s[i:], '\n')
			if j < 0 {
				i = n
			} else {
				i += j
			}
		case c == '/' && i+1 < n && s[i+1] == '*':
			j := strings.Index(s[i+2:], "*/")
			if j < 0 {
				i = n
			} else {
				i += j + 4
			}
		case c == '\'':
			j := sqlident.ScanQuoted(s, i, '\'')
			tokens = append(tokens, sqlToken{text: s[i:j]})
			i = j
		case c == '"':
			j := sqlident.ScanQuoted(s, i, c)
			tokens = append(tokens, sqlToken{text: sqlident.Unquote(s[i:j], c), quoted: true})
			i = j
		case c == '`':
			j := sqlident.ScanQuoted(s, i, c)
			tokens = append(tokens, sqlToken{text: asciiLower(sqlident.Unquote(s[i:j], c))})
			i = j
		case c == '[':
			j := strings.IndexByte(s[i:], ']')
			if j < 0 {
				tokens = append(tokens, sqlToken{text: asciiLower(s[i+1:])})
				i = n
			} else {
				tokens = append(tokens, sqlToken{text: asciiLower(s[i+1 : i+j])})
				i += j + 1
			}
		case sqlident.IsIdentByte(c):
			j := i + 1
			for j < n && sqlident.IsIdentByte(s[j]) {
				j++
			}
			tokens = append(tokens, sqlToken{text: asciiLower(s[i:j])})
			i = j
		default:
			tokens = append(tokens, sqlToken{text: string(c)})
			i++
		}
	}
	return tokens
}

// tableConstraintKeywords are the tokens that start a table constraint
// rather than a column definition in a CREATE TABLE's column list, per
// SQLite's grammar. A quoted constraint name (e.g. CONSTRAINT "pk_users")
// is left as-is: only the table name and column names are folded here.
var tableConstraintKeywords = map[string]bool{
	"constraint": true,
	"primary":    true,
	"unique":     true,
	"check":      true,
	"foreign":    true,
}

// foldIdentifierPositions folds the case of quoted tokens that sit where
// SQLite's grammar requires an identifier — a CREATE TABLE's own table
// name, and each column's name at the start of its definition — since
// SQLite identifiers compare case-insensitively there regardless of
// quoting style. It leaves every other token, quoted or not, untouched:
// in particular the contents of a CHECK, DEFAULT or GENERATED expression,
// where a double-quoted token still might be a DQS string literal whose
// case is a genuine change.
func foldIdentifierPositions(tokens []sqlToken) {
	n := len(tokens)
	i := 0
	for i < n && tokens[i].text != "create" {
		i++
	}
	if i >= n {
		return
	}
	i++
	for i < n && (tokens[i].text == "temp" || tokens[i].text == "temporary") {
		i++
	}
	if i >= n || tokens[i].text != "table" {
		return
	}
	i++
	if i+2 < n && tokens[i].text == "if" && tokens[i+1].text == "not" && tokens[i+2].text == "exists" {
		i += 3
	}
	if i >= n {
		return
	}

	nameIdx := i
	if i+2 < n && tokens[i+1].text == "." {
		nameIdx = i + 2
		i += 3
	} else {
		i++
	}
	if nameIdx < n && tokens[nameIdx].quoted {
		tokens[nameIdx].text = asciiLower(tokens[nameIdx].text)
	}

	for i < n && tokens[i].text != "(" {
		i++
	}
	if i >= n {
		return
	}
	i++

	depth := 1
	atFieldStart := true
	for i < n && depth > 0 {
		switch tokens[i].text {
		case "(":
			depth++
			atFieldStart = false
		case ")":
			depth--
			atFieldStart = false
		case ",":
			if depth == 1 {
				atFieldStart = true
			}
		default:
			if depth == 1 && atFieldStart {
				if tokens[i].quoted && !tableConstraintKeywords[tokens[i].text] {
					tokens[i].text = asciiLower(tokens[i].text)
				}
				atFieldStart = false
			}
		}
		i++
	}
}
