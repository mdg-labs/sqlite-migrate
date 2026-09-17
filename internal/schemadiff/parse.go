// Package schemadiff turns a schema.sql source of truth into a structured
// schema, diffs two structured schemas, and classifies each diff entry as
// safe or destructive. It never hand-writes a SQL grammar: a schema.sql is
// replayed into a temporary SQLite database and read back via
// sqlite_master and the PRAGMA table/foreign-key/index introspection
// calls, reusing SQLite's own parser instead of reimplementing one.
package schemadiff

import (
	"context"
	"database/sql"
	"fmt"
	"sort"
	"strings"

	_ "modernc.org/sqlite"
)

// Schema is the structured representation of a schema.sql file, as read
// back from a temporary SQLite database it was replayed into.
type Schema struct {
	Tables map[string]*Table
}

// SortedTableNames returns the schema's table names in a deterministic
// order, for callers that need stable iteration (diffing, tests).
func (s *Schema) SortedTableNames() []string {
	names := make([]string, 0, len(s.Tables))
	for name := range s.Tables {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// Table is one table's structure as read back from sqlite_master and the
// table/foreign-key/index PRAGMAs.
type Table struct {
	Name         string
	SQL          string
	Strict       bool
	WithoutRowID bool
	Columns      []Column
	ForeignKeys  []ForeignKey
	Indexes      []Index
}

// Column finds a table's column by name, or reports it isn't present.
func (t *Table) Column(name string) (Column, bool) {
	for _, c := range t.Columns {
		if c.Name == name {
			return c, true
		}
	}
	return Column{}, false
}

// Column is a single column's structure, as read back from
// PRAGMA table_info.
type Column struct {
	Name          string
	Type          string
	NotNull       bool
	HasDefault    bool
	DefaultValue  string
	PrimaryKeySeq int
}

// ForeignKey is a single foreign-key reference, as read back from
// PRAGMA foreign_key_list.
type ForeignKey struct {
	Table    string
	From     string
	To       string
	OnUpdate string
	OnDelete string
}

// Index is a single index, as read back from PRAGMA index_list and
// PRAGMA index_info. Origin is SQLite's own classification: "c" for an
// explicit CREATE INDEX, "u" for one implied by a UNIQUE constraint, "pk"
// for one implied by a PRIMARY KEY. Columns holds an empty string in place
// of any column that is really an indexed expression (e.g.
// "lower(email)"), which pragma_index_info can't name; SQL is that index's
// own sqlite_master.sql text and is what actually distinguishes those
// cases from one another. SQL is empty for "u"/"pk" indexes: SQLite still
// gives them their own sqlite_master row, but that row's sql column is
// NULL.
type Index struct {
	Name    string
	Unique  bool
	Origin  string
	Columns []string
	SQL     string
}

// NotStrictError reports that a table in schema.sql is missing the
// required STRICT qualifier (Core Design Principle: type narrowing
// without STRICT tables is closed by requiring STRICT outright, not by
// reasoning about coercion risk).
type NotStrictError struct {
	Table string
}

func (e *NotStrictError) Error() string {
	return fmt.Sprintf("table %q is not STRICT: schema.sql tables must be declared STRICT", e.Table)
}

// Parse replays a schema.sql string into a temporary, in-memory SQLite
// database and reads back its structure (tables, columns, indexes, foreign
// keys) via sqlite_master and PRAGMA introspection, reusing SQLite's own
// parser instead of hand-writing a SQL grammar. It returns a *NotStrictError
// if any table isn't declared STRICT.
func Parse(ctx context.Context, schemaSQL string) (*Schema, error) {
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		return nil, fmt.Errorf("schemadiff: open temp database: %w", err)
	}
	defer func() { _ = db.Close() }()
	// An in-memory SQLite database is private to the connection that
	// created it: pinning the pool to a single connection is what makes
	// every PRAGMA query below see the schema just replayed, instead of a
	// second, empty in-memory database a different pooled connection would
	// otherwise get.
	db.SetMaxOpenConns(1)

	if _, err := db.ExecContext(ctx, schemaSQL); err != nil {
		return nil, fmt.Errorf("schemadiff: replay schema.sql: %w", err)
	}

	tableMeta, err := readTableMeta(ctx, db)
	if err != nil {
		return nil, err
	}

	schema := &Schema{Tables: make(map[string]*Table, len(tableMeta))}
	for _, meta := range tableMeta {
		if !meta.strict {
			return nil, &NotStrictError{Table: meta.name}
		}

		table := &Table{
			Name:         meta.name,
			SQL:          meta.sql,
			Strict:       meta.strict,
			WithoutRowID: meta.withoutRowID,
		}

		table.Columns, err = readColumns(ctx, db, meta.name)
		if err != nil {
			return nil, err
		}
		table.ForeignKeys, err = readForeignKeys(ctx, db, meta.name)
		if err != nil {
			return nil, err
		}
		table.Indexes, err = readIndexes(ctx, db, meta.name)
		if err != nil {
			return nil, err
		}

		schema.Tables[meta.name] = table
	}

	return schema, nil
}

type tableMeta struct {
	name         string
	sql          string
	strict       bool
	withoutRowID bool
}

func readTableMeta(ctx context.Context, db *sql.DB) ([]tableMeta, error) {
	rows, err := db.QueryContext(ctx, `
		SELECT sqlite_master.name, sqlite_master.sql, pragma_table_list.strict, pragma_table_list.wr
		FROM sqlite_master
		JOIN pragma_table_list ON pragma_table_list.name = sqlite_master.name
		WHERE sqlite_master.type = 'table'
		  AND sqlite_master.name NOT LIKE 'sqlite\_%' ESCAPE '\'
		ORDER BY sqlite_master.name
	`)
	if err != nil {
		return nil, fmt.Errorf("schemadiff: read table list: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []tableMeta
	for rows.Next() {
		var m tableMeta
		var strict, wr int
		if err := rows.Scan(&m.name, &m.sql, &strict, &wr); err != nil {
			return nil, fmt.Errorf("schemadiff: scan table list row: %w", err)
		}
		m.strict = strict != 0
		m.withoutRowID = wr != 0
		out = append(out, m)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("schemadiff: read table list: %w", err)
	}
	return out, nil
}

func readColumns(ctx context.Context, db *sql.DB, table string) ([]Column, error) {
	rows, err := db.QueryContext(ctx, `SELECT name, type, "notnull", dflt_value, pk FROM pragma_table_info(?)`, table)
	if err != nil {
		return nil, fmt.Errorf("schemadiff: read columns for %q: %w", table, err)
	}
	defer func() { _ = rows.Close() }()

	var out []Column
	for rows.Next() {
		var c Column
		var notNull int
		var dflt sql.NullString
		if err := rows.Scan(&c.Name, &c.Type, &notNull, &dflt, &c.PrimaryKeySeq); err != nil {
			return nil, fmt.Errorf("schemadiff: scan column row for %q: %w", table, err)
		}
		c.NotNull = notNull != 0
		c.HasDefault = dflt.Valid
		c.DefaultValue = strings.TrimSpace(dflt.String)
		c.Type = strings.ToUpper(strings.TrimSpace(c.Type))
		out = append(out, c)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("schemadiff: read columns for %q: %w", table, err)
	}
	return out, nil
}

func readForeignKeys(ctx context.Context, db *sql.DB, table string) ([]ForeignKey, error) {
	rows, err := db.QueryContext(ctx, `
		SELECT "table", "from", "to", seq, on_update, on_delete
		FROM pragma_foreign_key_list(?)
		ORDER BY id, seq
	`, table)
	if err != nil {
		return nil, fmt.Errorf("schemadiff: read foreign keys for %q: %w", table, err)
	}

	var out []ForeignKey
	var seqs []int
	for rows.Next() {
		var fk ForeignKey
		var to sql.NullString
		var seq int
		if err := rows.Scan(&fk.Table, &fk.From, &to, &seq, &fk.OnUpdate, &fk.OnDelete); err != nil {
			_ = rows.Close()
			return nil, fmt.Errorf("schemadiff: scan foreign key row for %q: %w", table, err)
		}
		fk.To = to.String
		out = append(out, fk)
		seqs = append(seqs, seq)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return nil, fmt.Errorf("schemadiff: read foreign keys for %q: %w", table, err)
	}
	// rows must be closed before the per-FK parent-table lookup below: with
	// the connection pool pinned to one connection (see Parse), a second
	// query issued while this one is still open would block forever
	// waiting for a connection the pool will never hand back.
	_ = rows.Close()

	for i := range out {
		if out[i].To != "" {
			continue
		}
		// A REFERENCES clause without an explicit column list (e.g.
		// "user_id INTEGER REFERENCES users") implicitly targets the
		// parent table's primary key column at the same position as this
		// child column within the (possibly composite) key;
		// pragma_foreign_key_list reports that case as a NULL "to" rather
		// than naming the column.
		pk, err := readPrimaryKeyColumnAt(ctx, db, out[i].Table, seqs[i])
		if err != nil {
			return nil, err
		}
		out[i].To = pk
	}

	return out, nil
}

// readPrimaryKeyColumnAt returns the parent table's primary key column at
// position seq (0-based) within its key, i.e. the column with
// pragma_table_info's pk = seq + 1. A parent with only a rowid, or with no
// primary key column at that position, resolves to "" — SQLite itself
// treats that key as a mismatch.
func readPrimaryKeyColumnAt(ctx context.Context, db *sql.DB, table string, seq int) (string, error) {
	rows, err := db.QueryContext(ctx, `SELECT name FROM pragma_table_info(?) WHERE pk = ?`, table, seq+1)
	if err != nil {
		return "", fmt.Errorf("schemadiff: read primary key for %q: %w", table, err)
	}
	defer func() { _ = rows.Close() }()

	if rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return "", fmt.Errorf("schemadiff: scan primary key column for %q: %w", table, err)
		}
		return name, nil
	}
	if err := rows.Err(); err != nil {
		return "", fmt.Errorf("schemadiff: read primary key for %q: %w", table, err)
	}
	return "", nil
}

func readIndexes(ctx context.Context, db *sql.DB, table string) ([]Index, error) {
	rows, err := db.QueryContext(ctx, `
		SELECT pragma_index_list.name, pragma_index_list."unique", pragma_index_list.origin, sqlite_master.sql
		FROM pragma_index_list(?)
		LEFT JOIN sqlite_master ON sqlite_master.type = 'index' AND sqlite_master.name = pragma_index_list.name
		ORDER BY pragma_index_list.name
	`, table)
	if err != nil {
		return nil, fmt.Errorf("schemadiff: read indexes for %q: %w", table, err)
	}

	var out []Index
	for rows.Next() {
		var idx Index
		var unique int
		var sqlText sql.NullString
		if err := rows.Scan(&idx.Name, &unique, &idx.Origin, &sqlText); err != nil {
			_ = rows.Close()
			return nil, fmt.Errorf("schemadiff: scan index row for %q: %w", table, err)
		}
		idx.Unique = unique != 0
		idx.SQL = sqlText.String
		out = append(out, idx)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return nil, fmt.Errorf("schemadiff: read indexes for %q: %w", table, err)
	}
	// rows must be closed before issuing the per-index column query below:
	// with the connection pool pinned to one connection (see Parse), a
	// second query issued while this one is still open would block
	// forever waiting for a connection the pool will never hand back.
	_ = rows.Close()

	for i := range out {
		cols, err := readIndexColumns(ctx, db, out[i].Name)
		if err != nil {
			return nil, err
		}
		out[i].Columns = cols
	}

	return out, nil
}

func readIndexColumns(ctx context.Context, db *sql.DB, index string) ([]string, error) {
	rows, err := db.QueryContext(ctx, `SELECT name FROM pragma_index_info(?) ORDER BY seqno`, index)
	if err != nil {
		return nil, fmt.Errorf("schemadiff: read index columns for %q: %w", index, err)
	}
	defer func() { _ = rows.Close() }()

	var out []string
	for rows.Next() {
		// pragma_index_info reports a NULL name for a column that is
		// really an indexed expression (e.g. lower(email)); Index.SQL
		// carries the expression itself, so an empty placeholder here is
		// enough to preserve column count and position.
		var name sql.NullString
		if err := rows.Scan(&name); err != nil {
			return nil, fmt.Errorf("schemadiff: scan index column row for %q: %w", index, err)
		}
		out = append(out, name.String)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("schemadiff: read index columns for %q: %w", index, err)
	}
	return out, nil
}
