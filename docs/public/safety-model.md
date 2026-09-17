---
layout: page
title: Safety model
permalink: /safety-model/
---

Automatically generating a database rebuild — a real `CREATE TABLE` /
`INSERT ... SELECT` / `DROP TABLE` / rename, not a suggestion for a human
to review — is only reasonable with a real safety net underneath it. This
page explains that net in plain terms.

## Automatic backup

Every real `apply` (`sqlite-migrate apply --db app.db --yes`) backs up the
target database first, via SQLite's `VACUUM INTO` — never a raw copy of
the database file. A raw copy of a live SQLite file can capture a
database mid-write or mid-checkpoint; `VACUUM INTO` always produces a
single, consistent, complete file, regardless of what pragmas or journal
mode the live database is using.

The backup is written to a temporary name next to the database, fsynced,
then atomically renamed into its final name —
`<database file>.<nanosecond timestamp>.snapshot`, in the same directory
as the database file by default — so a crash mid-backup can never leave a
half-written file that looks like a valid snapshot.

**This snapshot is the only recovery mechanism.** There is no `rollback`
command and no down-migrations: rebuilds are auto-generated specifically
because they're routine, not because they're meant to be hand-reviewed
and hand-reverted. To undo an apply, restore the snapshot file over the
database file directly — the tool doesn't do this for you, since
"restore over a live path" is exactly the kind of raw file operation this
safety model avoids doing to your primary database automatically.

By default every snapshot is kept; pass `-retain-snapshots N` to `apply`
to prune down to the `N` most recent snapshots per database after each
successful apply.

## The destructive-change gate

Whether a change is safe or destructive is decided by **comparing which
tables and columns exist before and after** — never by scanning the
generated SQL text for the word `DROP`. Every rebuild, even a completely
safe one that loses zero data (adding a `CHECK` constraint to an existing
column, say), involves creating a new table and dropping the old one
internally. A classifier that reacted to `DROP` in the generated SQL would
misfire on that common, safe case and demand a human decision that was
never actually needed.

So: if every table and column that existed in the old schema still exists
in `schema.sql` (regardless of type or constraint changes), the change is
**safe** and `generate` writes it automatically, no confirmation needed.
Only when a table or column that existed before is genuinely absent
afterward is the change **destructive** — and even then, `generate`
doesn't refuse to *ever* produce it, because removing a column from
`schema.sql` already *is* the maintainer's decision; the tool shouldn't
hand that decision back as SQL to write by hand.

Without `-allow-destructive`, `generate` refuses outright: it writes
nothing, and its error names exactly which table(s)/column(s) would be
lost. Rerunning with `-allow-destructive` produces the migration —
including the drop — through the same automatic rebuild machinery, the
same pre-apply backup, and the same verification as any other change.
It's one flag, not one per statement kind: `DROP TABLE` and `DROP COLUMN`
are the same kind of decision from the maintainer's side.

A drop-and-add pair that `generate` can't confidently tell apart from a
rename is never treated as destructive without asking first — see the
rename prompt in the [`generate` reference](cli-reference#generate).

## Why STRICT tables

SQLite's column types are, by default, only advisory ("type affinity") —
a column declared `INTEGER` can still silently store a string. That
matters here specifically because generating a type change automatically
means trusting that the *declared* type is the truth about what's stored;
without an enforced type system, a maintainer's schema.sql could say one
thing while rows actually hold another, and no diff-based tool can see
that from `schema.sql` alone.

`STRICT` tables (supported since SQLite 3.37) close that hole by making
SQLite itself enforce the declared column types, rather than bolting a
best-effort warning system onto a fundamentally unenforced one. Because
of that, **every table in `schema.sql` must be declared `STRICT`** — a
non-`STRICT` table is refused with a clear error at `generate` time,
rather than silently trusted.

```sql
CREATE TABLE users (
    id    INTEGER PRIMARY KEY,
    email TEXT NOT NULL UNIQUE
) STRICT;
```

## The rest of the net

- **Every `apply` is one transaction, on a single pinned connection** —
  a crash mid-migration can't leave a half-migrated database.
- **`PRAGMA foreign_key_check` and `PRAGMA integrity_check` run inside
  that transaction, before `COMMIT`.** Any violation rolls back the whole
  batch, not just the offending statement.
- **Every generated migration is verified before it's ever written to
  disk**: `generate` replays the full journal plus the candidate migration
  into a scratch database and compares the result against `schema.sql`
  structurally. A rebuild that doesn't reproduce the intended schema
  exactly is refused at generation time, not discovered later.
- **Checksums are verified, not just recorded.** Every migration file's
  checksum is checked again on every subsequent `generate` and `apply` —
  an "immutable" migration file edited after being applied is caught, not
  silently accepted.
- **`apply` defaults to a dry run.** Nothing touches the database until
  `-yes` is passed explicitly.
