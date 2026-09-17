// Package sqldefwrap isolates the one call this project makes into the
// sqldef library: its pure string-in/string-out SQLite DDL generator,
// which already gets the "easy" cases right (CREATE TABLE, ADD COLUMN
// including REFERENCES) and so is reused rather than reimplemented. Kept
// behind this package's own interface so sqldef can be swapped later
// without touching the public API or the rebuild generator.
package sqldefwrap

// github.com/sqldef/sqldef/v3/schema exposes the GenerateIdempotentDDLs
// function Diff will wrap in Phase 4: a pure string-in/string-out SQLite
// DDL diff that needs no live database connection.
import _ "github.com/sqldef/sqldef/v3/schema"

// Diff calls sqldef's GenerateIdempotentDDLs to produce the DDL statements
// that take currentDDL to desiredDDL, using the same DDL strings
// schemadiff already has on hand from its own replay step.
