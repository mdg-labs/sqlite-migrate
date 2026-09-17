---
layout: page
title: FAQ
permalink: /faq/
---

**Why not just use sqldef directly?**
sqldef diffs plain SQL correctly for the "easy" 20% — new tables, new
columns — but doesn't attempt the SQLite rebuild SQLite itself has no
`ALTER TABLE` for (a type change, a new `CHECK`/`UNIQUE` constraint, a
changed foreign-key action). sqlite-migrate uses sqldef internally for the
cases it already gets right, and adds the full 12-step rebuild generator,
a destructive-vs-safe classifier that doesn't misfire on a rebuild's
internal `DROP`, and rename detection on top.

**Why is there no `rollback` command?**
Because rebuilds are auto-generated, the tool needs a real safety net
rather than a human reviewing every rebuild by hand before it could be
reverted. That net is the automatic pre-apply `VACUUM INTO` snapshot — see
[Safety model](safety-model#automatic-backup) for how to restore from it.
A down-migration would have to be generated with the same confidence as
the up-migration, for every rebuild, which doesn't reduce the actual risk
the snapshot already covers.

**What happens if I hand-edit a generated migration file?**
Its checksum, recorded when it was generated, no longer matches its body.
`check` and `apply` both recompute and compare it before doing anything
else, and refuse with a checksum-mismatch error rather than silently
trusting the edited file.

**`generate` is asking me to confirm a rename — what if I answer wrong?**
Answering "yes" to a rename that isn't one, or "no" to one that is, is
recoverable either way: a wrong "yes" produces a rename statement that
just won't match what you actually wanted (fix `schema.sql` and generate
again); a wrong "no" falls through to the destructive-change gate, which
still refuses to drop anything without `-allow-destructive`. Nothing is
silently lost in either case. For non-interactive/CI runs, set a default
answer for the whole run with `-assume-renames` or `-assume-no-renames`
instead of leaving an unattended prompt to hang.

**Does this work against an existing database that wasn't created by
`sqlite-migrate`?**
Not directly — `generate` diffs `schema.sql` against what replaying the
migration journal produces, not against a live database. Write a first
migration (or set of migrations) whose replayed result matches your
existing database's actual schema, then use `verify` to confirm the two
actually match before running `apply` against it.

**Does sqlite-migrate support Postgres or MySQL?**
No — SQLite only. Multi-database support is explicitly out of scope; the
rebuild machinery this tool exists for is specific to SQLite's lack of
`ALTER COLUMN`.

**What SQLite driver does it use?**
`modernc.org/sqlite`, a pure-Go implementation with no cgo dependency —
this keeps both the CLI and any binary embedding this project as a
library cross-compilation-friendly.

**Can I embed this in my own Go binary instead of shelling out to the
CLI?**
Yes — the root `sqlitemigrate` package (`Migration`, `Runner`, `Snapshot`,
`CheckDrift`, checksum verification) is the public runtime API, safe for
an embedding binary to import directly (including via `go:embed` for the
migrations directory itself). Everything under `internal/` is
generation-time tooling the `sqlite-migrate` CLI itself uses and an
embedding binary never needs.
