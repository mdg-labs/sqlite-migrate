// Package sqldefwrap isolates the one call this project makes into the
// sqldef library: its pure string-in/string-out SQLite DDL generator,
// which already gets the "easy" cases right (CREATE TABLE, ADD COLUMN
// including REFERENCES) and so is reused rather than reimplemented. Kept
// behind this package's own interface so sqldef can be swapped later
// without touching the public API or the rebuild generator.
package sqldefwrap

import (
	"fmt"
	"regexp"

	"github.com/sqldef/sqldef/v3/database"
	"github.com/sqldef/sqldef/v3/parser"
	"github.com/sqldef/sqldef/v3/schema"
)

// Differ generates the DDL statements that take a database at currentDDL to
// desiredDDL. Callers depend on this interface, not on sqldef's own types,
// so the concrete diff engine can be swapped later.
type Differ interface {
	Diff(desiredDDL, currentDDL string) ([]string, error)
}

// New returns the sqldef-backed Differ, for the schema changes sqldef
// already gets right: new tables and new columns, including REFERENCES.
// Anything else (type changes, constraint changes) is classified upstream
// and routed to internal/rebuild instead.
func New() Differ {
	return sqldefDiffer{}
}

type sqldefDiffer struct{}

// Diff calls sqldef's GenerateIdempotentDDLs, the pure string-in/string-out
// SQLite DDL diff — not sqldef.Run(), which drives a live DB connection and
// is CLI-only. EnableDrop is left false: this wrapper's job is limited to
// the additive cases sqldef gets right, so any DROP/REVOKE it would emit
// for a case outside that job is commented out rather than executed.
func (sqldefDiffer) Diff(desiredDDL, currentDDL string) ([]string, error) {
	ddls, err := schema.GenerateIdempotentDDLs(
		schema.GeneratorModeSQLite3,
		database.NewParser(parser.ParserModeSQLite3),
		desiredDDL,
		currentDDL,
		database.GeneratorConfig{},
		"",
	)
	if err != nil {
		return nil, fmt.Errorf("sqldefwrap: generate DDLs: %w", err)
	}
	return foldForeignKeysIntoAddColumn(ddls), nil
}

// identPattern matches one SQL identifier: either an unquoted run of word
// characters, or a double-quoted identifier with SQLite's doubled-quote
// escaping for an embedded quote.
const identPattern = `[A-Za-z0-9_]+|"(?:[^"]|"")+"`

var (
	addColumnStmt = regexp.MustCompile(
		`^ALTER TABLE (` + identPattern + `) ADD COLUMN (` + identPattern + `) (.+)$`)
	addForeignKeyStmt = regexp.MustCompile(
		`^ALTER TABLE (` + identPattern + `) ADD CONSTRAINT\s*(?:` + identPattern + `)?\s*` +
			`FOREIGN KEY \((` + identPattern + `)\) REFERENCES (` + identPattern + `) \((` + identPattern + `)\)(.*)$`)
)

// foldForeignKeysIntoAddColumn repairs sqldef's SQLite output for a new
// column that carries a REFERENCES clause. sqldef always emits the foreign
// key as a separate "ALTER TABLE ... ADD CONSTRAINT ... FOREIGN KEY ..."
// statement — syntax SQLite's ALTER TABLE has never supported — rather
// than folding it into the ADD COLUMN definition the way SQLite requires.
// When such a statement's single referencing column is one this same Diff
// call just added, its REFERENCES clause is moved onto that ADD COLUMN
// statement and the invalid ADD CONSTRAINT statement is dropped.
func foldForeignKeysIntoAddColumn(ddls []string) []string {
	type addedColumn struct {
		index      int
		definition string
	}
	added := make(map[string]addedColumn)
	for i, ddl := range ddls {
		m := addColumnStmt.FindStringSubmatch(ddl)
		if m == nil {
			continue
		}
		added[m[1]+"\x00"+m[2]] = addedColumn{index: i, definition: m[3]}
	}
	if len(added) == 0 {
		return ddls
	}

	folded := make([]string, len(ddls))
	copy(folded, ddls)
	drop := make(map[int]bool)

	for i, ddl := range ddls {
		m := addForeignKeyStmt.FindStringSubmatch(ddl)
		if m == nil {
			continue
		}
		table, column, refTable, refColumn, tail := m[1], m[2], m[3], m[4], m[5]
		col, ok := added[table+"\x00"+column]
		if !ok {
			continue
		}
		folded[col.index] = fmt.Sprintf("ALTER TABLE %s ADD COLUMN %s %s REFERENCES %s (%s)%s",
			table, column, col.definition, refTable, refColumn, tail)
		drop[i] = true
	}
	if len(drop) == 0 {
		return ddls
	}

	result := make([]string, 0, len(folded)-len(drop))
	for i, ddl := range folded {
		if !drop[i] {
			result = append(result, ddl)
		}
	}
	return result
}
