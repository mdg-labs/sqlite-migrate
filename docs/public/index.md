---
layout: page
title: Home
permalink: /
---

**sqlite-migrate** is a Go CLI and embeddable library that generates and
safely applies SQLite schema migrations from a plain-SQL `schema.sql`
source of truth — including full automatic rebuilds for changes SQLite's
native `ALTER TABLE` can't express (a type change, a new `CHECK`/`UNIQUE`
constraint, a changed foreign-key action, a rename).

You write `schema.sql`. `sqlite-migrate generate` diffs it against the
schema your existing migrations produce and writes the next migration file
for you — a plain `ALTER TABLE`/`CREATE TABLE` when that's enough, or the
full create-copy-drop-rename rebuild when it isn't. See
[Safety model](safety-model) for what makes that automation trustworthy
rather than reckless.

- [CLI reference](cli-reference) — every flag on every command
- [Safety model](safety-model) — backups, the destructive-change gate, `STRICT`
- [FAQ](faq)

## Installation

```sh
go install github.com/mdg-labs/sqlite-migrate/cmd/sqlite-migrate@latest
```

This requires a Go toolchain matching (or newer than) the `go` directive in
this repository's `go.mod`; `go install` downloads a matching toolchain
automatically if the one you have installed is older.

If no tagged release exists yet, build from a clone instead:

```sh
git clone https://github.com/mdg-labs/sqlite-migrate.git
cd sqlite-migrate
go build -o sqlite-migrate ./cmd/sqlite-migrate
```

Either way you get a single, statically-linked `sqlite-migrate` binary —
the SQLite driver (`modernc.org/sqlite`) is pure Go, so there's no cgo
toolchain or system SQLite library to install alongside it.

## Agent skill

If you drive schema changes through an AI coding agent (Claude Code,
Cursor, or any other agent the [`vercel-labs/skills`](https://github.com/vercel-labs/skills)
CLI supports), install the `sqlite-migrate` skill so it runs `generate`,
`check`, `apply`, `verify`, and `status` correctly and never reaches for
`--allow-destructive` just to get past a refusal:

```sh
npx skills add mdg-labs/sqlite-migrate --skill sqlite-migrate
```

Read the skill itself on GitHub:
[`skills/sqlite-migrate/SKILL.md`](https://github.com/mdg-labs/sqlite-migrate/blob/main/skills/sqlite-migrate/SKILL.md).

## Quickstart

**1. Write `schema.sql`.** Every table must be declared `STRICT` — see
[why](safety-model#why-strict-tables) in the safety model.

```sql
CREATE TABLE users (
    id         INTEGER PRIMARY KEY,
    email      TEXT NOT NULL UNIQUE,
    created_at TEXT NOT NULL DEFAULT (datetime('now'))
) STRICT;
```

**2. Generate the first migration.** With no `migrations/` directory yet,
`generate` treats this as a cold repo and writes a migration that creates
the table from nothing:

```sh
sqlite-migrate generate
# wrote migrations/20260917143022_create_users.sql
```

**3. Check the migration journal is internally consistent** (no DB
connection needed — safe to run in CI):

```sh
sqlite-migrate check
# ok: 1 migration(s) verified
```

**4. See what `apply` would do.** `apply` defaults to a dry run — nothing
touches the database until you pass `--yes`:

```sh
sqlite-migrate apply --db app.db
# dry run: 1 pending migration(s) would be applied:
#   20260917143022_create_users
# rerun with --yes to apply
```

**5. Apply it for real:**

```sh
sqlite-migrate apply --db app.db --yes
# applied 1 migration(s):
#   20260917143022_create_users
```

This first backs up `app.db` via `VACUUM INTO` (see
[Safety model](safety-model#automatic-backup)), then runs the migration in
one transaction with `foreign_key_check`/`integrity_check` before commit.

**6. Change `schema.sql` and repeat.** Add a column, tighten a constraint,
change a column's type, rename something — `generate` figures out which of
those is a plain `ALTER TABLE`, which needs a full rebuild, and asks a
one-line question in your terminal only when it can't tell a rename from a
drop-and-add on its own:

```sh
sqlite-migrate generate
# Rename `users.email` to `users.email_address`? [Y/n]
```

**7. Check what's applied to a given database at any time:**

```sh
sqlite-migrate status --db app.db
# 20260917143022_create_users  applied at 2026-09-17T14:30:25Z
```

**8. Verify a real database still matches the migration journal**,
independent of its own bookkeeping table — useful after a manual edit or to
audit a database you didn't apply the migrations to yourself:

```sh
sqlite-migrate verify --db app.db
# ok: database matches the migration journal
```

Full flags for every command: [CLI reference](cli-reference).
