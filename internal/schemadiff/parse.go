// Package schemadiff turns a schema.sql source of truth into a structured
// schema, diffs two structured schemas, and classifies each diff entry as
// safe or destructive. It never hand-writes a SQL grammar: a schema.sql is
// replayed into a temporary SQLite database and read back via
// sqlite_master and the PRAGMA table/foreign-key/index introspection
// calls, reusing SQLite's own parser instead of reimplementing one.
package schemadiff

// Parse replays a schema.sql string into a temporary SQLite database and
// reads back its structure (tables, columns, indexes, triggers, views,
// foreign keys) via sqlite_master and PRAGMA introspection.
