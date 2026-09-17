package sqlitemigrate

// modernc.org/sqlite is the pure-Go SQLite driver Runner opens its pinned
// connection through; the blank import registers it with database/sql
// under the "sqlite" driver name ahead of Runner.Apply using it.
import _ "modernc.org/sqlite"

// Runner applies a sequence of Migration values to a database over a
// single pinned connection. Apply runs each pending migration inside one
// transaction, suspending PRAGMA foreign_keys before BEGIN and running
// foreign_key_check and integrity_check before COMMIT, recording progress
// in a bookkeeping table.
