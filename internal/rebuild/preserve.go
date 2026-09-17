package rebuild

// Preserve recreates every index, trigger, view, and foreign key that
// referenced a table before its rebuild, once the rebuilt table is in
// place under its original name. Foreign keys are handled implicitly: a
// rebuilt table's own FK clauses live inside its verbatim CREATE TABLE
// text (generator.go reuses that text unchanged), and a foreign key
// defined on some other, non-rebuilt table just resolves by name once the
// rebuilt table exists again after its rename — neither needs a separate
// recreate step here. Explicit indexes need no separate drop step (SQLite
// drops one automatically along with its table) and are simply recreated
// once every rebuilt table is back — see indexStatements.
//
// Triggers and views are handled differently, and not just for the table
// they're directly attached to: SQLite's ALTER TABLE RENAME (with
// legacy_alter_table off, the only mode this package relies on) recompiles
// every trigger and view in the whole schema to track the rename, and
// fails with "no such table" if any of them currently reference a table
// that's missing for that instant — whether or not that trigger or view is
// itself attached to the table being renamed. So every trigger and view
// that references any rebuilt table, or references another trigger/view
// already in that set (transitively — e.g. a view built on a view that
// itself selects from a rebuilt table), is dropped before the first
// rebuilt table's CREATE and only recreated after the last rebuilt table's
// RENAME — see affectedObjects.

import (
	"context"
	"database/sql"
	"fmt"
	"sort"
	"strings"

	"github.com/mdg-labs/sqlite-migrate/internal/schemadiff"

	_ "modernc.org/sqlite"
)

// catalogObject is one trigger or view definition, as read back from
// sqlite_master.
type catalogObject struct {
	name    string
	tblName string
	sql     string
}

// tableColumn is one column as read back from pragma_table_xinfo, which —
// unlike the pragma_table_info schemadiff.Column comes from — also lists
// generated columns.
type tableColumn struct {
	name      string
	generated bool
}

// catalog is every trigger and view defined in a schema, plus the full
// column list of each requested table, gathered by replaying it into a
// temporary database — schemadiff.Schema carries neither (Phase 1's Parse
// never reads triggers or views back, and skips generated columns), so
// rebuild does its own replay here rather than depending on a wider Phase 1
// change. columns is keyed by asciiLower table name; a requested table the
// schema doesn't define has no entry.
type catalog struct {
	triggers []catalogObject
	views    []catalogObject
	columns  map[string][]tableColumn
}

func loadCatalog(ctx context.Context, schemaSQL string, tables []string) (catalog, error) {
	// Found by Phase 8 fuzzing (see schemadiff.Parse's identical guard):
	// modernc.org/sqlite silently stops executing at the first NUL byte in
	// a query string with no error at all, which would drop every trigger,
	// view, and column declared after it from this catalog without a
	// trace.
	if i := strings.IndexByte(schemaSQL, 0); i >= 0 {
		return catalog{}, fmt.Errorf("rebuild: schema SQL contains a NUL byte at offset %d", i)
	}

	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		return catalog{}, fmt.Errorf("rebuild: open catalog probe database: %w", err)
	}
	defer func() { _ = db.Close() }()
	// An in-memory SQLite database is private to the connection that opened
	// it; pinning the pool to one connection keeps every query below on the
	// same connection that replayed schemaSQL (see schemadiff.Parse, which
	// pins for the identical reason).
	db.SetMaxOpenConns(1)

	if _, err := db.ExecContext(ctx, schemaSQL); err != nil {
		return catalog{}, fmt.Errorf("rebuild: replay schema for catalog lookup: %w", err)
	}

	rows, err := db.QueryContext(ctx, `
		SELECT type, name, tbl_name, sql
		FROM sqlite_master
		WHERE type IN ('trigger', 'view')
		ORDER BY name
	`)
	if err != nil {
		return catalog{}, fmt.Errorf("rebuild: read trigger/view catalog: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var cat catalog
	for rows.Next() {
		var kind string
		var obj catalogObject
		if err := rows.Scan(&kind, &obj.name, &obj.tblName, &obj.sql); err != nil {
			return catalog{}, fmt.Errorf("rebuild: scan trigger/view catalog row: %w", err)
		}
		switch kind {
		case "trigger":
			cat.triggers = append(cat.triggers, obj)
		case "view":
			cat.views = append(cat.views, obj)
		}
	}
	if err := rows.Err(); err != nil {
		return catalog{}, fmt.Errorf("rebuild: read trigger/view catalog: %w", err)
	}

	cat.columns = make(map[string][]tableColumn, len(tables))
	for _, t := range tables {
		cols, err := readXColumns(ctx, db, t)
		if err != nil {
			return catalog{}, err
		}
		if len(cols) > 0 {
			cat.columns[asciiLower(t)] = cols
		}
	}
	return cat, nil
}

func readXColumns(ctx context.Context, db *sql.DB, table string) ([]tableColumn, error) {
	// hidden is 2 for a VIRTUAL and 3 for a STORED generated column; 1 marks
	// a virtual table's hidden column, which a rebuilt ordinary table never has.
	rows, err := db.QueryContext(ctx, `SELECT name, hidden FROM pragma_table_xinfo(?) ORDER BY cid`, table)
	if err != nil {
		return nil, fmt.Errorf("rebuild: read columns for %q: %w", table, err)
	}
	defer func() { _ = rows.Close() }()

	var out []tableColumn
	for rows.Next() {
		var c tableColumn
		var hidden int
		if err := rows.Scan(&c.name, &hidden); err != nil {
			return nil, fmt.Errorf("rebuild: scan column row for %q: %w", table, err)
		}
		c.generated = hidden == 2 || hidden == 3
		out = append(out, c)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("rebuild: read columns for %q: %w", table, err)
	}
	return out, nil
}

// indexStatements returns the CREATE INDEX statements needed to restore
// td's table's explicit indexes to the state schema.sql (the source the
// diff's After side was parsed from) actually declares, once the rebuilt
// table is back in place under its original name.
func indexStatements(td schemadiff.TableDiff) []string {
	var stmts []string
	for _, idx := range td.After.Indexes {
		// "u"/"pk" origin indexes are implied by a UNIQUE/PRIMARY KEY
		// constraint inside the CREATE TABLE text itself (already reused
		// verbatim for the new table) and have no sqlite_master.sql text of
		// their own to replay; only an explicit CREATE INDEX ("c" origin)
		// needs its own statement here. Unlike triggers and views, an index
		// can't reference any table other than its own, so it needs no
		// drop-before/recreate-after treatment — see affectedObjects.
		if idx.Origin != "c" || idx.SQL == "" {
			continue
		}
		stmts = append(stmts, asStatement(idx.SQL))
	}
	return stmts
}

// affected is every trigger and view that must be dropped before the first
// rebuilt table's CREATE and recreated only after the last rebuilt table's
// RENAME.
type affected struct {
	views    []catalogObject
	triggers []catalogObject
}

// affectedObjects computes affected by closing rebuiltTables under
// "references": starting from the rebuilt tables themselves (definitely
// missing for the whole rebuild window), it repeatedly adds any
// not-yet-added trigger or view whose stored SQL text names something
// already in that missing set — including another trigger or view just
// added, so a view built on a view built on a rebuilt table is still
// caught — until a full pass adds nothing more.
func affectedObjects(rebuiltTables []string, cat catalog) affected {
	missing := make(map[string]bool, len(rebuiltTables))
	for _, t := range rebuiltTables {
		missing[asciiLower(t)] = true
	}

	viewIn := make(map[string]bool, len(cat.views))
	trigIn := make(map[string]bool, len(cat.triggers))
	for {
		changed := false
		for _, v := range cat.views {
			key := asciiLower(v.name)
			if viewIn[key] {
				continue
			}
			if referencesAny(v.sql, missing) {
				viewIn[key] = true
				missing[key] = true
				changed = true
			}
		}
		for _, tr := range cat.triggers {
			key := asciiLower(tr.name)
			if trigIn[key] {
				continue
			}
			if referencesAny(tr.sql, missing) {
				trigIn[key] = true
				missing[key] = true
				changed = true
			}
		}
		if !changed {
			break
		}
	}

	var out affected
	for _, v := range cat.views {
		if viewIn[asciiLower(v.name)] {
			out.views = append(out.views, v)
		}
	}
	for _, tr := range cat.triggers {
		if trigIn[asciiLower(tr.name)] {
			out.triggers = append(out.triggers, tr)
		}
	}
	return out
}

// mergeAffected is the union of a and b, deduplicated by name: the drop
// set has to cover what exists before the migration as well as what the
// after schema defines, since both can reference a rebuilt table.
func mergeAffected(a, b affected) affected {
	merge := func(x, y []catalogObject) []catalogObject {
		seen := make(map[string]bool, len(x)+len(y))
		var out []catalogObject
		for _, o := range append(append([]catalogObject{}, x...), y...) {
			key := asciiLower(o.name)
			if seen[key] {
				continue
			}
			seen[key] = true
			out = append(out, o)
		}
		return out
	}
	return affected{views: merge(a.views, b.views), triggers: merge(a.triggers, b.triggers)}
}

// stillDefined is the subset of dropped that cat still defines, each with
// cat's definition rather than the dropped one: an object can reference a
// rebuilt table only on the before side (so it's dropped) yet survive with
// a changed definition, and must come back regardless.
func stillDefined(dropped affected, cat catalog) affected {
	pick := func(xs, defs []catalogObject) []catalogObject {
		byKey := make(map[string]catalogObject, len(defs))
		for _, d := range defs {
			byKey[asciiLower(d.name)] = d
		}
		var out []catalogObject
		for _, x := range xs {
			if d, ok := byKey[asciiLower(x.name)]; ok {
				out = append(out, d)
			}
		}
		return out
	}
	return affected{views: pick(dropped.views, cat.views), triggers: pick(dropped.triggers, cat.triggers)}
}

// referencesAny reports whether sql names any identifier in names.
func referencesAny(sql string, names map[string]bool) bool {
	for name := range names {
		if referencesTable(sql, name) {
			return true
		}
	}
	return false
}

// dropAffectedStatements must run before any of the rebuilt tables' own
// CREATE/DROP/RENAME statements — see affectedObjects and the package
// doc comment above. Triggers are dropped first since nothing in aff can
// reference a trigger by name; IF EXISTS makes the rest of the order
// immaterial (DROP doesn't recompile or otherwise validate what it drops).
func dropAffectedStatements(aff affected) []string {
	var stmts []string
	for _, tr := range aff.triggers {
		stmts = append(stmts, fmt.Sprintf("DROP TRIGGER IF EXISTS %s;", quoteIdent(tr.name)))
	}
	for _, v := range aff.views {
		stmts = append(stmts, fmt.Sprintf("DROP VIEW IF EXISTS %s;", quoteIdent(v.name)))
	}
	return stmts
}

// recreateAffectedStatements recreates what dropAffectedStatements dropped
// and the after schema still defines (see stillDefined), once every rebuilt
// table is back in place under its original name. Views are recreated
// first, in dependency order (a view built on another affected view is
// recreated after the view it selects from), since a trigger's body can
// select from a view but a view can't reference a trigger.
func recreateAffectedStatements(aff affected) []string {
	var stmts []string
	for _, v := range topoSortViews(aff.views) {
		stmts = append(stmts, asStatement(v.sql))
	}
	for _, tr := range aff.triggers {
		stmts = append(stmts, asStatement(tr.sql))
	}
	return stmts
}

// topoSortViews orders views so that any view among them referenced by
// another view in the set comes first. SQLite doesn't resolve a view's
// references at CREATE VIEW time, so this order isn't required for the
// script to run; it keeps the output deterministic and readable. The
// visited set alone also stops the DFS on a (never-queryable) view cycle.
func topoSortViews(views []catalogObject) []catalogObject {
	byName := make(map[string]catalogObject, len(views))
	keys := make([]string, 0, len(views))
	for _, v := range views {
		k := asciiLower(v.name)
		byName[k] = v
		keys = append(keys, k)
	}
	sort.Strings(keys)

	visited := make(map[string]bool, len(keys))
	order := make([]catalogObject, 0, len(keys))
	var visit func(key string)
	visit = func(key string) {
		if visited[key] {
			return
		}
		visited[key] = true

		v := byName[key]
		var deps []string
		for _, other := range keys {
			if other == key {
				continue
			}
			if referencesTable(v.sql, byName[other].name) {
				deps = append(deps, other)
			}
		}
		sort.Strings(deps)
		for _, d := range deps {
			visit(d)
		}
		order = append(order, v)
	}
	for _, k := range keys {
		visit(k)
	}
	return order
}

func asStatement(sql string) string {
	return strings.TrimRight(strings.TrimSpace(sql), "; \t\n") + ";"
}

// referencesTable reports whether a view's SQL text names table as an
// identifier — bare, or quoted with ", `, or [ ]. sqlite_master exposes no
// dependency tracking for views (unlike a trigger's tbl_name column, which
// names the table it fires on directly), so this is the only signal
// available short of hand-parsing the view's query, which schemadiff
// deliberately avoids doing for SQL in general.
func referencesTable(sql, table string) bool {
	lower := strings.ToLower(sql)
	needle := strings.ToLower(table)

	// A quote character inside a quoted identifier is written doubled.
	quotedForms := []string{
		`"` + strings.ReplaceAll(needle, `"`, `""`) + `"`,
		"`" + strings.ReplaceAll(needle, "`", "``") + "`",
		"[" + needle + "]",
	}
	for _, quoted := range quotedForms {
		if strings.Contains(lower, quoted) {
			return true
		}
	}

	return containsIdentifierWord(lower, needle)
}
