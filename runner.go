package sqlitemigrate

// modernc.org/sqlite is the pure-Go SQLite driver Runner opens its pinned
// connection through; the blank import registers it with database/sql
// under the "sqlite" driver name ahead of Runner.Apply using it.
import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"sort"
	"time"

	_ "modernc.org/sqlite"
)

// busyTimeoutMillis bounds how long a connection waits on SQLITE_BUSY
// before giving up, so a brief lock held by another process or connection
// (e.g. a concurrent read of the same database) doesn't fail Apply or
// Snapshot outright.
const busyTimeoutMillis = 5000

// Runner applies a sequence of Migration values to a database over a
// single pinned connection. Apply runs each pending migration inside one
// transaction, suspending PRAGMA foreign_keys before BEGIN and running
// foreign_key_check and integrity_check before COMMIT, recording progress
// in a bookkeeping table.
type Runner struct {
	// DBPath is the on-disk SQLite database file Apply and Snapshot
	// operate on.
	DBPath string
	// SnapshotDir is where Apply's automatic pre-apply backup, and any
	// direct Snapshot call, write snapshot files. Defaults to DBPath's
	// own directory when empty.
	SnapshotDir string
	// RetainSnapshots is the number of snapshot files kept per database
	// after a successful snapshot; older ones are pruned. RetainSnapshots
	// <= 0 keeps every snapshot.
	RetainSnapshots int
}

// schemaMigrationsTable is the bookkeeping table's name — shared with
// drift.go so CheckDrift excludes it from schema comparisons rather than
// reporting the drift package's own bookkeeping as manual drift.
const schemaMigrationsTable = "schema_migrations"

var bookkeepingTableSQL = fmt.Sprintf(`
CREATE TABLE IF NOT EXISTS %s (
	version TEXT PRIMARY KEY,
	slug TEXT NOT NULL,
	checksum TEXT NOT NULL,
	applied_at TEXT NOT NULL
) STRICT
`, schemaMigrationsTable)

// ChecksumMismatchError reports that a migration already recorded as
// applied no longer matches the checksum of the file Apply was given for
// it — an "immutable" migration file edited after being applied.
type ChecksumMismatchError struct {
	Version string
	Want    string
	Got     string
}

func (e *ChecksumMismatchError) Error() string {
	return fmt.Sprintf("sqlitemigrate: migration %s checksum mismatch: recorded %s, got %s (file was edited after being applied)", e.Version, e.Want, e.Got)
}

// MissingMigrationError reports that a migration recorded as applied has
// no corresponding entry in the migrations Apply was given — its file was
// deleted after being applied, which the immutability guarantee treats the
// same as an edited file.
type MissingMigrationError struct {
	Version string
}

func (e *MissingMigrationError) Error() string {
	return fmt.Sprintf("sqlitemigrate: migration %s is recorded as applied but its file is missing (file was deleted after being applied)", e.Version)
}

// snapshotDir returns r's configured snapshot directory, defaulting to
// DBPath's own directory.
func (r *Runner) snapshotDir() string {
	if r.SnapshotDir != "" {
		return r.SnapshotDir
	}
	return filepath.Dir(r.DBPath)
}

// Snapshot backs up r's database via VACUUM INTO into r's snapshot
// directory, per Safety Model: an atomic, single-file image safe to
// restore from at any time, never a raw copy of a live database. It
// returns the snapshot file's path.
func (r *Runner) Snapshot(ctx context.Context) (string, error) {
	return Snapshot(ctx, r.DBPath, r.snapshotDir(), r.RetainSnapshots)
}

// Apply applies every migration in migrations not yet recorded as applied,
// in version order, inside one transaction on a single pinned connection.
// It backs up the database via Snapshot first, suspends PRAGMA
// foreign_keys before BEGIN (multi-table rebuilds spanning an FK
// relationship, including a cycle, otherwise can't be expressed in any
// per-statement order), runs PRAGMA foreign_key_check and
// PRAGMA integrity_check inside the transaction before COMMIT, and rolls
// back the whole batch on any failure — including a checksum mismatch on a
// migration already recorded as applied. It returns the migrations it
// applied in this call.
func (r *Runner) Apply(ctx context.Context, migrations []Migration) ([]Migration, error) {
	sorted := make([]Migration, len(migrations))
	copy(sorted, migrations)
	sortMigrations(sorted)

	if _, err := r.Snapshot(ctx); err != nil {
		var warn *SnapshotWarning
		if !errors.As(err, &warn) {
			return nil, fmt.Errorf("sqlitemigrate: backup before apply: %w", err)
		}
	}

	db, err := sql.Open("sqlite", r.DBPath)
	if err != nil {
		return nil, fmt.Errorf("sqlitemigrate: open database %q: %w", r.DBPath, err)
	}
	defer func() { _ = db.Close() }()
	// A single pinned connection is what makes the PRAGMA foreign_keys
	// suspension below and the transaction that follows it apply to the
	// same SQLite connection: PRAGMA foreign_keys is per-connection state,
	// and database/sql's pool would otherwise be free to hand the
	// transaction a different connection than the one the PRAGMA ran on.
	db.SetMaxOpenConns(1)

	if _, err := db.ExecContext(ctx, fmt.Sprintf("PRAGMA busy_timeout = %d", busyTimeoutMillis)); err != nil {
		return nil, fmt.Errorf("sqlitemigrate: set busy_timeout: %w", err)
	}

	if _, err := db.ExecContext(ctx, `PRAGMA foreign_keys = OFF`); err != nil {
		return nil, fmt.Errorf("sqlitemigrate: suspend foreign_keys before apply: %w", err)
	}

	applied, err := r.applyInTransaction(ctx, db, sorted)

	if _, ferr := db.ExecContext(ctx, `PRAGMA foreign_keys = ON`); ferr != nil && err == nil {
		err = fmt.Errorf("sqlitemigrate: restore foreign_keys after apply: %w", ferr)
	}

	return applied, err
}

func (r *Runner) applyInTransaction(ctx context.Context, db *sql.DB, sorted []Migration) ([]Migration, error) {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("sqlitemigrate: begin apply transaction: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	if _, err := tx.ExecContext(ctx, bookkeepingTableSQL); err != nil {
		return nil, fmt.Errorf("sqlitemigrate: create bookkeeping table: %w", err)
	}

	recorded, err := recordedChecksums(ctx, tx)
	if err != nil {
		return nil, err
	}

	var pending []Migration
	sortedVersions := make(map[string]struct{}, len(sorted))
	for _, m := range sorted {
		sortedVersions[m.Version] = struct{}{}
		want, ok := recorded[m.Version]
		if !ok {
			pending = append(pending, m)
			continue
		}
		if want != m.Checksum {
			return nil, &ChecksumMismatchError{Version: m.Version, Want: want, Got: m.Checksum}
		}
	}
	for version := range recorded {
		if _, ok := sortedVersions[version]; !ok {
			return nil, &MissingMigrationError{Version: version}
		}
	}

	for _, m := range pending {
		if _, err := tx.ExecContext(ctx, m.SQL); err != nil {
			return nil, fmt.Errorf("sqlitemigrate: apply migration %s: %w", m.Filename, err)
		}
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO schema_migrations (version, slug, checksum, applied_at) VALUES (?, ?, ?, ?)`,
			m.Version, m.Slug, m.Checksum, time.Now().UTC().Format(time.RFC3339),
		); err != nil {
			return nil, fmt.Errorf("sqlitemigrate: record migration %s: %w", m.Filename, err)
		}
	}

	if err := checkForeignKeys(ctx, tx); err != nil {
		return nil, err
	}
	if err := checkIntegrity(ctx, tx); err != nil {
		return nil, err
	}

	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("sqlitemigrate: commit apply transaction: %w", err)
	}

	return pending, nil
}

func recordedChecksums(ctx context.Context, tx *sql.Tx) (map[string]string, error) {
	rows, err := tx.QueryContext(ctx, `SELECT version, checksum FROM schema_migrations`)
	if err != nil {
		return nil, fmt.Errorf("sqlitemigrate: read applied migrations: %w", err)
	}
	defer func() { _ = rows.Close() }()

	recorded := make(map[string]string)
	for rows.Next() {
		var version, checksum string
		if err := rows.Scan(&version, &checksum); err != nil {
			return nil, fmt.Errorf("sqlitemigrate: scan applied migration: %w", err)
		}
		recorded[version] = checksum
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("sqlitemigrate: read applied migrations: %w", err)
	}
	return recorded, nil
}

// ForeignKeyViolationError reports one row PRAGMA foreign_key_check found
// violating a foreign key, found before COMMIT so the whole apply rolls
// back instead of leaving the violation committed.
type ForeignKeyViolationError struct {
	Table         string
	RowID         sql.NullInt64
	ReferredTable string
	ForeignKeyID  int64
}

func (e *ForeignKeyViolationError) Error() string {
	return fmt.Sprintf("sqlitemigrate: foreign key violation in %q referencing %q (rowid %v)", e.Table, e.ReferredTable, e.RowID)
}

func checkForeignKeys(ctx context.Context, tx *sql.Tx) error {
	rows, err := tx.QueryContext(ctx, `PRAGMA foreign_key_check`)
	if err != nil {
		return fmt.Errorf("sqlitemigrate: run foreign_key_check: %w", err)
	}
	defer func() { _ = rows.Close() }()

	if rows.Next() {
		var v ForeignKeyViolationError
		if err := rows.Scan(&v.Table, &v.RowID, &v.ReferredTable, &v.ForeignKeyID); err != nil {
			return fmt.Errorf("sqlitemigrate: scan foreign_key_check violation: %w", err)
		}
		return &v
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("sqlitemigrate: run foreign_key_check: %w", err)
	}
	return nil
}

// IntegrityCheckError reports that PRAGMA integrity_check found the
// database inconsistent before COMMIT.
type IntegrityCheckError struct {
	Messages []string
}

func (e *IntegrityCheckError) Error() string {
	return fmt.Sprintf("sqlitemigrate: integrity_check failed: %v", e.Messages)
}

func checkIntegrity(ctx context.Context, tx *sql.Tx) error {
	rows, err := tx.QueryContext(ctx, `PRAGMA integrity_check`)
	if err != nil {
		return fmt.Errorf("sqlitemigrate: run integrity_check: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var messages []string
	for rows.Next() {
		var msg string
		if err := rows.Scan(&msg); err != nil {
			return fmt.Errorf("sqlitemigrate: scan integrity_check result: %w", err)
		}
		if msg != "ok" {
			messages = append(messages, msg)
		}
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("sqlitemigrate: run integrity_check: %w", err)
	}
	if len(messages) > 0 {
		return &IntegrityCheckError{Messages: messages}
	}
	return nil
}

func sortMigrations(migrations []Migration) {
	sort.Slice(migrations, func(i, j int) bool { return migrations[i].Version < migrations[j].Version })
}
