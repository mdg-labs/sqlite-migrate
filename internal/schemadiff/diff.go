package schemadiff

import (
	"sort"
	"strings"
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
// set of table/column/index/foreign-key differences between them.
func Diff(before, after *Schema) *SchemaDiff {
	d := &SchemaDiff{}

	for _, name := range after.SortedTableNames() {
		if _, ok := before.Tables[name]; !ok {
			d.AddedTables = append(d.AddedTables, after.Tables[name])
		}
	}
	for _, name := range before.SortedTableNames() {
		if _, ok := after.Tables[name]; !ok {
			d.RemovedTables = append(d.RemovedTables, before.Tables[name])
		}
	}

	for _, name := range before.SortedTableNames() {
		beforeTable, ok := before.Tables[name]
		if !ok {
			continue
		}
		afterTable, ok := after.Tables[name]
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
		afterCols[c.Name] = c
	}
	beforeCols := make(map[string]Column, len(before.Columns))
	for _, c := range before.Columns {
		beforeCols[c.Name] = c
	}

	for _, c := range after.Columns {
		beforeCol, ok := beforeCols[c.Name]
		if !ok {
			td.AddedColumns = append(td.AddedColumns, c)
			continue
		}
		if !columnsEqual(beforeCol, c) {
			td.ChangedColumns = append(td.ChangedColumns, ColumnDiff{Name: c.Name, Before: beforeCol, After: c})
		}
	}
	for _, c := range before.Columns {
		if _, ok := afterCols[c.Name]; !ok {
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
	beforeSet := make(map[ForeignKey]bool, len(before))
	for _, fk := range before {
		beforeSet[fk] = true
	}
	afterSet := make(map[ForeignKey]bool, len(after))
	for _, fk := range after {
		afterSet[fk] = true
	}

	for _, fk := range after {
		if !beforeSet[fk] {
			added = append(added, fk)
		}
	}
	for _, fk := range before {
		if !afterSet[fk] {
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
		return idx.Name + "\x00" + idx.Origin + "\x00" + boolStr(idx.Unique) + "\x00" +
			strings.Join(idx.Columns, ",") + "\x00" + normalizeSQL(idx.SQL)
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

// normalizeSQL collapses whitespace so cosmetic differences in how SQLite
// echoes back a CREATE TABLE statement (which it does verbatim from the
// source text) don't register as a change on their own.
func normalizeSQL(sql string) string {
	return strings.Join(strings.Fields(sql), " ")
}
