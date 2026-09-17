# sqlite-migrate

A Go CLI + embeddable library that generates and safely applies SQLite
schema migrations from a plain-SQL `schema.sql` source of truth, including
full automatic rebuilds for changes SQLite's native `ALTER TABLE` can't
express.

`schema.sql` is the only thing you write by hand. `sqlite-migrate`
diffs it against the migration journal it already generated, works out
whether the change is a plain `ALTER TABLE`, a rename, or something that
needs a full 12-step rebuild, and writes the migration file for you.

## Install

```
go install github.com/mdg-labs/sqlite-migrate/cmd/sqlite-migrate@latest
```

This puts a `sqlite-migrate` binary on your `PATH` (assuming
`$(go env GOPATH)/bin` is on it).

## Quickstart

Start from an empty directory and a `schema.sql`:

```sql
-- schema.sql
CREATE TABLE users (
    id INTEGER PRIMARY KEY,
    email TEXT NOT NULL
) STRICT;
```

Every table must be declared `STRICT`; `sqlite-migrate` refuses to parse a
schema that isn't.

Generate the first migration:

```
$ sqlite-migrate generate
wrote migrations/20240102150405_create_users.sql
```

This creates `migrations/20240102150405_create_users.sql`, a plain SQL file
with a checksum header. Nothing has touched a database yet.

Apply it. `apply` defaults to a dry run — pass `--yes` to actually run it:

```
$ sqlite-migrate apply --db app.db
dry run: 1 pending migration(s) would be applied:
  20240102150405_create_users
rerun with --yes to apply

$ sqlite-migrate apply --db app.db --yes
applied 1 migration(s):
  20240102150405_create_users
```

Confirm it landed:

```
$ sqlite-migrate status --db app.db
20240102150405_create_users  applied at 2024-01-02T15:04:09Z
```

`app.db` now has a `users` table matching `schema.sql`. From here, every
time you edit `schema.sql`, run `sqlite-migrate generate` again to produce
the next migration and `sqlite-migrate apply --db app.db --yes` to apply
it.

## CLI reference

Every command accepts `-schema`/`-dir` or `-db`/`-dir` as shown below; none
of them read any other configuration or environment variable.

### `generate`

Diffs `schema.sql` against the migration journal (every file already in the
migrations directory, replayed in order) and, if anything changed, writes
the next migration file.

```
sqlite-migrate generate [flags]
```

| Flag | Default | Meaning |
|---|---|---|
| `-schema` | `schema.sql` | Path to the schema.sql source of truth |
| `-dir` | `migrations` | Directory holding generated migration files |
| `-m`, `-message` | (none) | Slug used in the migration file name, e.g. `-m add_users_index` |
| `-allow-destructive` | `false` | Generate a migration even if it would drop a table or column |
| `-assume-renames` | `false` | Treat every ambiguous drop+add pair as a rename, without prompting |
| `-assume-no-renames` | `false` | Treat every ambiguous drop+add pair as a genuine drop+add, without prompting |

When `schema.sql` renamed a table or column, `sqlite-migrate` can't always
tell that apart from a drop of the old one plus an add of a new one with
the same shape. In that case, run interactively, `generate` prompts:

```
Rename table "users" to "accounts"? [Y/n]
```

Pressing Enter (or answering `y`) generates `ALTER TABLE ... RENAME ...`,
preserving the existing data; answering `n` treats it as a genuine
drop-and-add instead, which is then gated behind `--allow-destructive` like
any other drop. In a script or CI job with no terminal attached, use
`--assume-renames` or `--assume-no-renames` to answer every such prompt the
same way without blocking on input; passing both is an error.

A change that would drop a table or column is refused by default:

```
$ sqlite-migrate generate
generate: refusing to generate a destructive migration:
  users: column(s) dropped: legacy_flag
rerun with --allow-destructive to generate this migration anyway
```

Pass `-allow-destructive` once you've confirmed the drop is intentional (a
genuine drop-and-add, not an unconfirmed rename) and the generated
migration is safe to apply.

Before writing anything, `generate` replays the candidate migration into a
temporary database and verifies it reproduces `schema.sql` exactly,
refusing to write a migration that doesn't.

Exit codes: `0` a migration was written (or there was nothing to do — "no
changes detected"); `1` generate refused to proceed (a destructive change
without `-allow-destructive`, both `-assume-renames` and
`-assume-no-renames` given, a schema.sql or migration file that fails to
parse, or a candidate that fails its own verification); `2` a flag was
invalid.

### `apply`

Runs every migration not yet recorded as applied against a target
database, as a single transaction: `VACUUM INTO` backup first,
`foreign_key_check`/`integrity_check` before commit, all or nothing.

```
sqlite-migrate apply -db <path> [flags]
```

| Flag | Default | Meaning |
|---|---|---|
| `-db` | (required) | Path to the target SQLite database file |
| `-dir` | `migrations` | Directory holding generated migration files |
| `-yes` | `false` | Apply pending migrations; without this flag, apply only prints what would run |
| `-retain-snapshots` | `0` | Snapshot files to keep per database after a successful apply; `0` keeps every snapshot |

`apply` is a dry run by default — it prints the pending migrations and
exits without touching `-db` at all. Pass `-yes` to actually run them.

Exit codes: `0` the dry run printed its plan, or `-yes` applied everything
(or found nothing pending) successfully; `1` apply failed — the whole
transaction, including any partial work, was rolled back and the database
is unchanged; `2` a flag was invalid, or `-db` was omitted.

### `check`

Statically verifies the migration journal without opening any target
database: every migration file's checksum header still matches its body
(catching a file edited after being generated), and replaying the whole
journal still reproduces `schema.sql` exactly. Safe to run in CI with no
database available.

```
sqlite-migrate check [flags]
```

| Flag | Default | Meaning |
|---|---|---|
| `-schema` | `schema.sql` | Path to the schema.sql source of truth |
| `-dir` | `migrations` | Directory holding generated migration files |

Exit codes: `0` every migration's checksum matched and the journal
reproduces `schema.sql` exactly; `1` a checksum mismatch or drift was
found; `2` a flag was invalid.

### `verify`

A ground-truth check against a real target database, independent of its
own bookkeeping table: replays the migration journal into a temporary
database and compares the result structurally against the target
database's actual schema, reporting anything that doesn't match on either
side.

```
sqlite-migrate verify -db <path> [flags]
```

| Flag | Default | Meaning |
|---|---|---|
| `-db` | (required) | Path to the target SQLite database file |
| `-dir` | `migrations` | Directory holding generated migration files |

Exit codes: `0` the database matches the migration journal exactly; `1`
drift was detected, or the database or journal couldn't be read; `2` a
flag was invalid, or `-db` was omitted.

### `status`

For each migration in the journal, reports whether the target database has
it recorded as applied or still pending.

```
sqlite-migrate status -db <path> [flags]
```

| Flag | Default | Meaning |
|---|---|---|
| `-db` | (required) | Path to the target SQLite database file |
| `-dir` | `migrations` | Directory holding generated migration files |

A migration recorded as applied whose checksum no longer matches its file
(edited after being applied) is reported as such rather than silently
shown as up to date; a migration recorded as applied in the database with
no corresponding file in `-dir` is reported separately as orphaned.

Exit codes: `0` always, once the database and journal were read
successfully; `1` the database or journal couldn't be read; `2` a flag was
invalid, or `-db` was omitted.

## Safety model, briefly

Every `apply` runs as one transaction on a single pinned connection: a
`VACUUM INTO` backup is taken first (never a raw file copy),
`foreign_key_check` and `integrity_check` run before commit, and dry-run is
the default — nothing touches `-db` until you pass `-yes`. Destructive
changes (dropping a table or column) are refused at `generate` time unless
you pass `-allow-destructive`. The full explanation of this model, and the
project's FAQ, lives in the published documentation site.

## License

MIT — see [LICENSE](LICENSE).
