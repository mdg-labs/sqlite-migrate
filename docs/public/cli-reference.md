---
layout: page
title: CLI reference
permalink: /cli-reference/
---

Every command below lives under the single `sqlite-migrate` binary:
`sqlite-migrate <command> [flags]`. Flags follow Go's standard `flag`
package conventions (`-flag value` or `-flag=value`; boolean flags need no
value to set them true).

## `generate`

Diffs `schema.sql` against the schema your existing migrations produce
(by replaying them into a scratch database — never against a live
database), and writes the next migration file.

| Flag | Default | Meaning |
| --- | --- | --- |
| `-schema` | `schema.sql` | Path to the schema.sql source of truth |
| `-dir` | `migrations` | Directory holding migration files |
| `-m`, `-message` | *(derived)* | Slug for the generated file name, e.g. `-m "add user email"` |
| `-allow-destructive` | `false` | Generate the migration even if it would drop a table or column |
| `-assume-renames` | `false` | Treat every ambiguous drop+add pair as a rename, without prompting |
| `-assume-no-renames` | `false` | Treat every ambiguous drop+add pair as a genuine drop+add, without prompting |

Behavior:

- A change that doesn't remove any table or column that existed before is
  **safe** and is always generated automatically — including a full
  rebuild, when SQLite has no direct `ALTER TABLE` for what changed (a
  type change, a new `CHECK`/`UNIQUE` constraint, a changed foreign-key
  action).
- When a column or table disappears from one name while a plausibly
  related one appears under another, `generate` asks in the terminal —
  `Rename users.email to users.email_address? [Y/n]` — and on confirmation
  generates a rename (data preserved, no destructive flag needed) instead
  of a drop-and-add. Declining, or running non-interactively with neither
  `-assume-renames` nor `-assume-no-renames` set, falls back to treating it
  as a genuine drop-and-add.
- A genuinely **destructive** change (a column or table removed from
  `schema.sql` with nothing plausibly renamed in its place) is refused: no
  file is written, and the error lists exactly which table(s)/column(s)
  would be lost. Rerun with `-allow-destructive` to generate it anyway —
  it's still auto-generated, never hand-written SQL.
- Before writing anything, the candidate migration is verified by
  replaying the full journal plus the candidate into a scratch database
  and comparing the result against `schema.sql`; a candidate that doesn't
  reproduce it exactly is refused rather than written.
- No changes detected → `generate` prints `no changes detected` and exits
  `0` without writing a file.

Exit codes: `0` success (including "no changes"), `1` refused (destructive
change without the flag, verification failure, parse error), `2` bad flags.

## `check`

Static, connection-free verification of the migration journal — safe to
run in CI without a database. Never opens a target database.

| Flag | Default | Meaning |
| --- | --- | --- |
| `-schema` | `schema.sql` | Path to the schema.sql source of truth |
| `-dir` | `migrations` | Directory holding migration files |

Verifies, for every migration file: its checksum header still matches its
body (catching a file edited after being generated), and that replaying
the whole journal still reproduces `schema.sql` exactly. Prints
`ok: N migration(s) verified` on success.

Exit codes: `0` ok, `1` a checksum mismatch or drift was found, `2` bad
flags.

## `apply`

Runs pending migrations against a target database, through the public
runtime's transactional `Runner.Apply`.

| Flag | Default | Meaning |
| --- | --- | --- |
| `-db` | *(required)* | Path to the target SQLite database file |
| `-dir` | `migrations` | Directory holding migration files |
| `-yes` | `false` | Actually apply pending migrations; without it, `apply` only prints what would run |
| `-retain-snapshots` | `0` | Snapshot files to keep per database after a successful apply; `0` keeps every snapshot |

**Dry-run is the default.** Without `-yes`, `apply` reads what's already
recorded as applied, prints the pending migrations, and touches nothing.
With `-yes`:

1. Backs up the database via `VACUUM INTO` (see [Safety
   model](safety-model#automatic-backup)).
2. Opens one pinned connection and one transaction for the whole batch of
   pending migrations, with `PRAGMA foreign_keys` suspended for the
   duration (needed so a multi-table rebuild spanning a foreign-key
   relationship, including a cycle, can be applied in any statement
   order).
3. Runs `PRAGMA foreign_key_check` and `PRAGMA integrity_check` before
   `COMMIT`; any violation rolls back the whole batch.
4. Records each applied migration's version, slug, checksum, and
   timestamp in a bookkeeping table (`schema_migrations`).

A migration already recorded as applied whose checksum no longer matches
its file — edited after being applied — aborts the whole apply rather than
silently reapplying or skipping it. There is no `rollback` command:
recovery is restoring the pre-apply snapshot (see [Safety
model](safety-model#automatic-backup)).

Exit codes: `0` success (including "no pending migrations"), `1` a
migration failed, a checksum mismatch or missing file was found, or a
foreign-key/integrity violation was found, `2` bad flags (including a
missing `-db`).

## `verify`

A ground-truth check against a real target database, independent of its
own bookkeeping table: replays the migration journal into a scratch
database and compares the result structurally against the target
database's *actual* schema.

| Flag | Default | Meaning |
| --- | --- | --- |
| `-db` | *(required)* | Path to the target SQLite database file |
| `-dir` | `migrations` | Directory holding migration files |

On drift, reports each difference as either "in database, not produced by
the migration journal" or "produced by the migration journal, not in
database" — useful when a database may have been modified by hand, or when
auditing a database you didn't necessarily apply these exact migrations to
yourself.

Exit codes: `0` no drift, `1` drift found (or an I/O error), `2` bad flags.

## `status`

Reports, for a given database, which migrations are applied and which are
still pending.

| Flag | Default | Meaning |
| --- | --- | --- |
| `-db` | *(required)* | Path to the target SQLite database file |
| `-dir` | `migrations` | Directory holding migration files |

For each migration in the journal, prints one of: `pending`, `applied at
<timestamp>`, or `applied, checksum mismatch (file was edited after being
applied)`. A migration recorded in the database's bookkeeping table with
no corresponding file on disk is reported separately as `applied,
migration file missing`.

Exit codes: always `0` on a successful read; `1` on an I/O error, `2` bad
flags.
