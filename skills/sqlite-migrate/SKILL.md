---
name: sqlite-migrate
description: Drive the sqlite-migrate CLI to author schema.sql changes and turn them into safe, generated SQLite migrations in a project that has adopted it — when to run generate/check/apply/verify/status, how to answer the rename prompt, what --allow-destructive gates, and where the pre-apply backup lives.
---

# sqlite-migrate

`sqlite-migrate` is a Go CLI that generates SQLite schema migrations from a
plain-SQL `schema.sql` file and applies them safely. This skill is for
driving that CLI correctly in a project that already has it installed
(a `schema.sql` file and a `migrations/` directory of generated `*.sql`
files at its root, applied to one or more `*.db` files). It assumes the
`sqlite-migrate` binary is already on `PATH` in this project; if it isn't,
stop and ask rather than trying to install or build it.

## `schema.sql` is the only source of truth

`schema.sql` is hand-edited; every file under `migrations/` is generated —
**never hand-edit a file under `migrations/`**, and never hand-edit a
target `.db` file directly. To change the schema: edit `schema.sql` to
describe the schema you want, then run `generate` to produce the
migration that gets an existing database there.

Every ordinary table in `schema.sql` **must** be declared `STRICT`:

```sql
CREATE TABLE users (
    id INTEGER PRIMARY KEY,
    age INTEGER
) STRICT;
```

A table missing `STRICT` makes every command that parses `schema.sql`
(`generate`, `check`) refuse outright with an error naming the table — add
`STRICT` to it rather than looking for a flag to relax this; there isn't
one. (A virtual table — FTS5, R-Tree — is exempt, since SQLite never
allows one to be declared `STRICT` at all.)

## The five commands, and when to run each

- **`sqlite-migrate generate [-schema schema.sql] [-dir migrations] [-m "message"]`**
  Run after every `schema.sql` edit. Diffs the current migration journal
  (replayed from `migrations/`) against `schema.sql` and writes one new
  timestamped file under `migrations/` (or prints `no changes detected` if
  the edit didn't change the effective schema). Never touches a database.
- **`sqlite-migrate check [-schema schema.sql] [-dir migrations]`**
  A connection-free, static gate: recomputes every migration file's
  checksum and confirms replaying the whole journal still reproduces
  `schema.sql` exactly. Run it in CI, or any time you want to confirm the
  journal and `schema.sql` still agree without touching a real database.
  Exits non-zero on any mismatch.
- **`sqlite-migrate apply -db <path> [-dir migrations] [--yes]`**
  Applies every migration not yet recorded against `<path>`. **Dry-run is
  the default** — without `--yes` it only prints the pending migrations
  and exits; nothing is touched. Add `--yes` to actually execute: it takes
  an automatic `VACUUM INTO` backup first, then runs every pending
  migration in one transaction, with `PRAGMA foreign_key_check` and
  `PRAGMA integrity_check` before commit. Always run the dry-run first and
  read its output before adding `--yes`.
- **`sqlite-migrate verify -db <path> [-dir migrations]`**
  Ground truth: captures the target database's actual live schema and
  compares it structurally against replaying the migration journal, so it
  catches drift even if the bookkeeping table itself is wrong or missing.
  Run it after `apply`, or any time you're not sure the database really
  matches what the journal says it should.
- **`sqlite-migrate status -db <path> [-dir migrations]`**
  Lists every migration in the journal as `pending`, `applied at
  <timestamp>`, or `applied, checksum mismatch` (a migration file was
  edited after being applied), plus any bookkeeping entry whose file is
  now missing. Use it to see what's outstanding before deciding whether to
  `apply`.

## The rename prompt

`generate` sometimes finds a dropped table/column and an added one that
look like they could be the same thing renamed — e.g. dropping column
`age` while adding `date_of_birth` in the same table. Renaming is a
different destructive-risk shape than a real drop-and-add: a genuine
rename should preserve the underlying data (`ALTER TABLE ... RENAME
COLUMN`/`RENAME TABLE`), while a real drop-and-add discards it. When
`generate` can't tell which one you meant, it asks, once per ambiguous
pair:

```
Rename users.age to users.date_of_birth? [Y/n]
```

Answer based on what you actually did in `schema.sql`:

- **It is a rename** (same data, new name/table) — answer `y` (or press
  Enter; `Y` is the default). `generate` emits the `RENAME`
  statement and treats it as safe.
- **It is genuinely a drop and a separate, unrelated add** — answer `n`.
  `generate` then treats the dropped side as a real, destructive removal,
  which requires `--allow-destructive` (below) to actually generate.

In a non-interactive context (CI, or any run where you cannot read a
prompt and type a reply), never leave this to `generate`'s default input
handling — if no answer is available on stdin, it falls back to declining
every prompt, which will make a real rename look like a destructive
drop-and-add. Instead pass exactly one of:

- **`--assume-renames`** — treat every ambiguous pair as a rename,
  without asking.
- **`--assume-no-renames`** — treat every ambiguous pair as a genuine
  drop-and-add, without asking.

Passing both is an error. Only pass one of these flags when you actually
know, from the `schema.sql` edit you made, which case you're in — don't
default to `--assume-renames` reflexively just to silence the prompt.

## `--allow-destructive`

`generate` refuses to write a migration that would make a table or column
present before the change genuinely absent after it — a table dropped
entirely, or a column removed from a table that still exists — unless
`--allow-destructive` is also passed. It refuses with a message naming
exactly which table(s)/column(s) would be lost.

This gate is about tables and columns named in `schema.sql`, never about
how the migration itself is implemented: a type change or constraint
change on a column that's still present (like `07_type_change` below)
often requires an internal full-table rebuild — create a new table, copy
the data across, drop the old table, rename the new one into place — but
that internal drop is not what this gate is looking at, and never
triggers it. Only a column or table you actually removed from `schema.sql`
does.

Because of that, **a refusal here is a signal to double-check
`schema.sql`**, not a reflex to add the flag: it almost always means either
(a) you genuinely intend to drop that data, in which case
`--allow-destructive` is correct, or (b) an edit to `schema.sql` that
should have changed a column in place, not removed it, went wrong — e.g. a
copy-paste that dropped a column definition instead of editing it, or a
rename that should have been confirmed and wasn't (see the prompt above).
Re-read the `schema.sql` diff before ever passing this flag; never add it
just to make `generate` stop refusing.

## Non-interactive / CI flags

For any run without a human able to answer a prompt (CI, or an agent
running unattended):

- `generate --assume-renames` or `generate --assume-no-renames` — resolve
  every rename prompt without asking (see above; pick the one that
  matches what the `schema.sql` edit actually did).
- `generate --allow-destructive` — only when the destructive change is
  actually intended (see above).
- `check` never prompts and never touches a database — it's the natural
  CI gate to run on every `schema.sql`/`migrations/` change, independent
  of `apply`.
- `apply` never prompts either; its safety gate is `--yes` itself
  (default is dry-run, matching the "look before you leap" model) — a CI
  deploy step passes `--yes` deliberately, the same way a human would.

## Where the backup lives

Every `apply --yes` run takes an automatic `VACUUM INTO` backup of the
target database before applying anything, in the **same directory as the
database file itself** (unless the embedding program configured a
different `SnapshotDir`, which the CLI does not expose as a flag). The
file is named `<db-file-name>.<20-digit-timestamp>.snapshot` — e.g. for
`-db path/to/app.db`, a snapshot named
`path/to/app.db.01789675652686520507.snapshot` next to it. If a migration
run goes wrong, restore by copying the most recent (highest-numbered)
snapshot for that database back over the original file; `apply` never
deletes a snapshot unless `-retain-snapshots N` was passed with `N > 0`
(the default, `0`, keeps every snapshot forever).

## Walkthrough: a column type change end to end

Say `schema.sql` currently has:

```sql
CREATE TABLE users (
    id INTEGER PRIMARY KEY,
    age TEXT
) STRICT;
```

and you're asked to fix `age` to be an integer, on a project that already
has this migrated into `app.db`.

1. Edit `schema.sql` in place, changing only the column's type:

   ```sql
   CREATE TABLE users (
       id INTEGER PRIMARY KEY,
       age INTEGER
   ) STRICT;
   ```

2. Generate the migration:

   ```
   sqlite-migrate generate -schema schema.sql -dir migrations
   ```

   This column still exists before and after (only its type changed), so
   there is no destructive removal and no rename ambiguity — no prompt
   appears, no flag is needed, and the command prints `wrote
   migrations/<timestamp>_update_users.sql`. Internally, because a plain
   `ALTER TABLE` can't change a column's type, the generated file performs
   a full rebuild (new table with the new type, copy the data across, drop
   the old table, rename the new one into place) — this is expected and
   still classified safe, since `age` isn't absent afterward.

3. Confirm the journal and `schema.sql` still agree, without touching a
   database:

   ```
   sqlite-migrate check -schema schema.sql -dir migrations
   ```

4. See what applying would do first (the default, no `--yes`):

   ```
   sqlite-migrate apply -db app.db -dir migrations
   ```

5. Apply it for real:

   ```
   sqlite-migrate apply -db app.db -dir migrations --yes
   ```

   This takes the automatic snapshot described above, then runs the
   rebuild in one transaction with `foreign_key_check`/`integrity_check`
   before committing.

6. Confirm the database now matches:

   ```
   sqlite-migrate status -db app.db -dir migrations
   sqlite-migrate verify -db app.db -dir migrations
   ```

   `status` should show the new migration `applied at <timestamp>`;
   `verify` should print `ok: database matches the migration journal`.
