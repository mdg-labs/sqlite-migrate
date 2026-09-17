package sqlitemigrate

// CheckDrift replays a migration sequence into a temporary database and
// compares the resulting schema against the target, so both generate-time
// verification and the `verify` CLI command against a real database share
// the same replay-based comparison rather than trusting generated SQL
// text.

import (
	"context"
	"database/sql"
	"fmt"
	"sort"
	"strings"

	_ "modernc.org/sqlite"
)

// SchemaObject is one row of sqlite_master: a table, index, trigger or
// view, exactly as SQLite itself reports it. Comparing these — never
// generated SQL text — is what CheckDrift and ReplaySchema use as their
// ground truth.
type SchemaObject struct {
	Type    string
	Name    string
	TblName string
	SQL     string
}

// DriftReport is the set of schema objects present on only one side of a
// comparison. An empty report means the two schemas are identical.
type DriftReport struct {
	OnlyInExpected []SchemaObject
	OnlyInActual   []SchemaObject
}

// Empty reports whether the two compared schemas were identical.
func (r *DriftReport) Empty() bool {
	return len(r.OnlyInExpected) == 0 && len(r.OnlyInActual) == 0
}

// ReplaySchema replays a sequence of migrations, in order, into a fresh
// temporary SQLite database and returns the resulting schema. This is the
// only way either call site — generate-time verification of a candidate
// migration, or the standalone verify command — determines what a
// migration journal actually produces, rather than trusting the generated
// SQL text's own claims about what it does.
func ReplaySchema(ctx context.Context, migrations []Migration) ([]SchemaObject, error) {
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		return nil, fmt.Errorf("sqlitemigrate: open replay database: %w", err)
	}
	defer func() { _ = db.Close() }()
	// An in-memory SQLite database is private to the connection that
	// created it: pinning the pool to a single connection is what makes
	// every migration in the loop below, and the schema read at the end,
	// see the same database rather than a fresh empty one from a
	// different pooled connection.
	db.SetMaxOpenConns(1)

	for _, m := range migrations {
		// modernc.org/sqlite silently stops executing at the first NUL byte
		// in a query string and reports no error at all, so a migration
		// file corrupted to contain one would replay as if truncated there
		// rather than fail — which would make the drift check compare a
		// truncated replay against a truncated apply and see no drift at
		// all. Refusing outright, before executing any of it, is the only
		// safe response.
		if i := strings.IndexByte(m.SQL, 0); i >= 0 {
			return nil, fmt.Errorf("sqlitemigrate: migration %q contains a NUL byte at offset %d", m.Filename, i)
		}
		if _, err := db.ExecContext(ctx, m.SQL); err != nil {
			return nil, fmt.Errorf("sqlitemigrate: replay migration %q: %w", m.Filename, err)
		}
	}

	return readSchemaObjects(ctx, db)
}

// CaptureSchema reads back db's current schema the same way ReplaySchema
// reads back a replayed one, so a real deployed database and a replayed
// migration journal are always compared like for like.
func CaptureSchema(ctx context.Context, db *sql.DB) ([]SchemaObject, error) {
	return readSchemaObjects(ctx, db)
}

func readSchemaObjects(ctx context.Context, db *sql.DB) ([]SchemaObject, error) {
	// schema_migrations is Runner's own bookkeeping table, not part of the
	// schema a migration journal describes, so it must never show up as
	// drift between a replayed journal and a database Runner.Apply itself
	// produced.
	rows, err := db.QueryContext(ctx, `
		SELECT type, name, tbl_name, COALESCE(sql, '')
		FROM sqlite_master
		WHERE name NOT LIKE 'sqlite\_%' ESCAPE '\'
		  AND tbl_name != ?
		ORDER BY type, name
	`, schemaMigrationsTable)
	if err != nil {
		return nil, fmt.Errorf("sqlitemigrate: read schema objects: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []SchemaObject
	for rows.Next() {
		var o SchemaObject
		if err := rows.Scan(&o.Type, &o.Name, &o.TblName, &o.SQL); err != nil {
			return nil, fmt.Errorf("sqlitemigrate: scan schema object: %w", err)
		}
		out = append(out, o)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("sqlitemigrate: read schema objects: %w", err)
	}
	return out, nil
}

// CheckDrift replays migrations into a fresh temporary database and
// compares the resulting schema against expected. expected is either
// another replay (generate-time: verifying a freshly generated candidate
// against schema.sql replayed as a single statement) or a real deployed
// database's captured schema (the verify command), so both call sites
// share this one comparison.
func CheckDrift(ctx context.Context, migrations []Migration, expected []SchemaObject) (*DriftReport, error) {
	actual, err := ReplaySchema(ctx, migrations)
	if err != nil {
		return nil, err
	}
	return diffSchemaObjects(expected, actual), nil
}

func diffSchemaObjects(expected, actual []SchemaObject) *DriftReport {
	key := func(o SchemaObject) string {
		return o.Type + "\x00" + o.Name + "\x00" + o.TblName + "\x00" + o.SQL
	}

	expectedSet := make(map[string]SchemaObject, len(expected))
	for _, o := range expected {
		expectedSet[key(o)] = o
	}
	actualSet := make(map[string]SchemaObject, len(actual))
	for _, o := range actual {
		actualSet[key(o)] = o
	}

	report := &DriftReport{}
	for k, o := range expectedSet {
		if _, ok := actualSet[k]; !ok {
			report.OnlyInExpected = append(report.OnlyInExpected, o)
		}
	}
	for k, o := range actualSet {
		if _, ok := expectedSet[k]; !ok {
			report.OnlyInActual = append(report.OnlyInActual, o)
		}
	}

	sort.Slice(report.OnlyInExpected, func(i, j int) bool { return report.OnlyInExpected[i].Name < report.OnlyInExpected[j].Name })
	sort.Slice(report.OnlyInActual, func(i, j int) bool { return report.OnlyInActual[i].Name < report.OnlyInActual[j].Name })

	return report
}
