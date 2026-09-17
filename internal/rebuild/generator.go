// Package rebuild generates the full 12-step SQL sequence SQLite's native
// ALTER TABLE can't express: create the new table, copy rows across with
// an explicit column mapping, drop the old table, rename the new one into
// place, and recreate everything that referenced the original.
package rebuild

// Generate produces the create-new, INSERT...SELECT, drop-old, rename
// sequence for a table classified as needing a rebuild.
