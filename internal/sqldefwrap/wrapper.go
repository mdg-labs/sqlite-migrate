// Package sqldefwrap isolates the one call this project makes into the
// sqldef library: its pure string-in/string-out SQLite DDL generator,
// which already gets the "easy" cases right (CREATE TABLE, ADD COLUMN
// including REFERENCES) and so is reused rather than reimplemented. Kept
// behind this package's own interface so sqldef can be swapped later
// without touching the public API or the rebuild generator.
package sqldefwrap

import (
	"fmt"
	"strings"

	"github.com/sqldef/sqldef/v3/database"
	"github.com/sqldef/sqldef/v3/parser"
	"github.com/sqldef/sqldef/v3/schema"

	"github.com/mdg-labs/sqlite-migrate/internal/sqlident"
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
	return foldForeignKeysIntoAddColumn(ddls)
}

// parseAddColumn parses ddl as "ALTER TABLE <table> ADD COLUMN <column>
// <definition>", the shape sqldef emits for a new column. ok is false if
// ddl isn't shaped like that.
func parseAddColumn(ddl string) (table, column, definition string, ok bool) {
	rest, ok := strings.CutPrefix(ddl, "ALTER TABLE ")
	if !ok {
		return "", "", "", false
	}
	table, rest, ok = scanIdentThen(rest, ' ')
	if !ok {
		return "", "", "", false
	}
	rest, ok = strings.CutPrefix(rest, "ADD COLUMN ")
	if !ok {
		return "", "", "", false
	}
	column, rest, ok = scanIdentThen(rest, ' ')
	if !ok || rest == "" {
		return "", "", "", false
	}
	return table, column, rest, true
}

// parseAddForeignKey parses ddl as "ALTER TABLE <table> ADD CONSTRAINT
// [<name>] FOREIGN KEY (<column>) REFERENCES <refTable> (<refColumn>)
// <tail>", the shape sqldef emits for a REFERENCES clause — syntax
// SQLite's ALTER TABLE has never supported. The constraint name is
// optional and, when absent, ok is false if ddl isn't shaped like that.
func parseAddForeignKey(ddl string) (table, column, refTable, refColumn, tail string, ok bool) {
	rest, matched := strings.CutPrefix(ddl, "ALTER TABLE ")
	if !matched {
		return "", "", "", "", "", false
	}
	table, rest, matched = scanIdentThen(rest, ' ')
	if !matched {
		return "", "", "", "", "", false
	}
	rest, matched = strings.CutPrefix(rest, "ADD CONSTRAINT")
	if !matched {
		return "", "", "", "", "", false
	}
	rest = strings.TrimLeft(rest, " ")
	if !strings.HasPrefix(rest, "FOREIGN KEY") {
		_, rest, matched = scanIdentThen(rest, ' ')
		if !matched {
			return "", "", "", "", "", false
		}
		rest = strings.TrimLeft(rest, " ")
	}
	rest, matched = strings.CutPrefix(rest, "FOREIGN KEY (")
	if !matched {
		return "", "", "", "", "", false
	}
	column, rest, matched = scanIdentThen(rest, ')')
	if !matched {
		return "", "", "", "", "", false
	}
	rest, matched = strings.CutPrefix(rest, " REFERENCES ")
	if !matched {
		return "", "", "", "", "", false
	}
	refTable, rest, matched = scanIdentThen(rest, ' ')
	if !matched {
		return "", "", "", "", "", false
	}
	rest, matched = strings.CutPrefix(rest, "(")
	if !matched {
		return "", "", "", "", "", false
	}
	refColumn, rest, matched = scanIdentThen(rest, ')')
	if !matched {
		return "", "", "", "", "", false
	}
	return table, column, refTable, refColumn, rest, true
}

// scanIdentThen scans one SQL identifier at the start of s and requires it
// be followed immediately by delim, returning the identifier and the
// remainder of s past delim.
func scanIdentThen(s string, delim byte) (ident, rest string, ok bool) {
	tok, end, identOK := sqlident.ScanIdent(s, 0)
	if !identOK || end >= len(s) || s[end] != delim {
		return "", "", false
	}
	return tok, s[end+1:], true
}

// foldForeignKeysIntoAddColumn repairs sqldef's SQLite output for a new
// column that carries a REFERENCES clause. sqldef always emits the foreign
// key as a separate "ALTER TABLE ... ADD CONSTRAINT ... FOREIGN KEY ..."
// statement — syntax SQLite's ALTER TABLE has never supported — rather
// than folding it into the ADD COLUMN definition the way SQLite requires.
// When such a statement's single referencing column is one this same Diff
// call just added, its REFERENCES clause is moved onto that ADD COLUMN
// statement and the invalid ADD CONSTRAINT statement is dropped.
//
// A REFERENCES clause added to a column that already existed — sqldef
// still emits it as the same invalid ADD CONSTRAINT statement, but there is
// no ADD COLUMN statement in this call to fold it into — is out of this
// wrapper's scope (see New) and reported as an error rather than passed
// through, since executing it would fail with a syntax error, potentially
// after other statements from the same call already applied.
func foldForeignKeysIntoAddColumn(ddls []string) ([]string, error) {
	type addedColumn struct {
		index      int
		definition string
	}
	added := make(map[string]addedColumn)
	for i, ddl := range ddls {
		table, column, definition, ok := parseAddColumn(ddl)
		if !ok {
			continue
		}
		added[table+"\x00"+column] = addedColumn{index: i, definition: definition}
	}

	folded := make([]string, len(ddls))
	copy(folded, ddls)
	drop := make(map[int]bool)

	for i, ddl := range ddls {
		table, column, refTable, refColumn, tail, ok := parseAddForeignKey(ddl)
		if !ok {
			continue
		}
		col, isAdded := added[table+"\x00"+column]
		if !isAdded {
			return nil, fmt.Errorf(
				"sqldefwrap: %q adds a foreign key to an existing column; "+
					"SQLite's ALTER TABLE cannot add a foreign key outside a new column's definition", ddl)
		}
		folded[col.index] = fmt.Sprintf("ALTER TABLE %s ADD COLUMN %s %s REFERENCES %s (%s)%s",
			table, column, col.definition, refTable, refColumn, tail)
		drop[i] = true
	}
	if len(drop) == 0 {
		return ddls, nil
	}

	result := make([]string, 0, len(folded)-len(drop))
	for i, ddl := range folded {
		if !drop[i] {
			result = append(result, ddl)
		}
	}
	return result, nil
}
