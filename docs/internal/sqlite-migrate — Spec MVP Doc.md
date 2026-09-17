# sqlite-migrate — Spec / MVP Doc

2026-09-17 · @ghotso

## Problem Statement

SQLite has no `ALTER COLUMN`. It can add a column, rename a column or table, and (on SQLite ≥ 3.35) drop a simple column — that's the entire native `ALTER TABLE` vocabulary. Anything else — changing a column's type, adding or changing a `CHECK`/`UNIQUE`/`NOT NULL` constraint, changing a foreign-key action — has no direct SQL statement. The only way to do it is SQLite's own documented 12-step procedure: create a new table with the desired structure, copy every row across, drop the old table, rename the new one into place, and recreate every index, trigger, view and foreign key that pointed at it.

Every schema-migration tool for SQLite has to confront this. Most don't confront it well:

- They generate the easy 20% (new tables, new columns) correctly and automatically.
- They either refuse, silently do nothing, or produce wrong SQL for the other 80% — anything that needs the 12-step rebuild.
- None of them reliably tell the difference between a genuine data-loss change (a column the maintainer actually removed) and an internal implementation detail of a safe rebuild (which always involves creating a new table and dropping the old one, even when zero data is lost).

**Goal:** a tool that generates the full rebuild automatically and safely for every change that doesn't discard data, and asks for one explicit, single confirmation only for the cases that genuinely do — rather than pushing all non-trivial schema changes onto the maintainer as hand-written SQL.

## Why No Existing Tool Solves This

Checked across both languages and both tool categories (schema-diff generators and ORM migration systems). None combines plain-SQL schema source + automatic diffing + automatic rebuild generation with FK/index/trigger preservation + a single destructive-change gate + rename handling:

| Tool | Auto-diffs schema.sql? | Generates rebuilds (type/constraint changes)? | Preserves triggers/views across rebuild? | Handles renames? | Source of truth |
| --- | --- | --- | --- | --- | --- |
| **sqldef** (Go) | Yes | No — silently misses or mishandles them | N/A (doesn't attempt them) | No — emits ADD+DROP | Plain SQL |
| **Atlas** (Go) | Yes | Partially, but its destructive-change safety check requires a paid plan since v0.38 | Partially | No | Plain SQL / HCL |
| **David Rothlis' migrator** (Python) | Yes | No — PRAGMA table\_info diffing is blind to CHECK/UNIQUE/COLLATE/generated columns | No — explicitly unsupported | No | Plain SQL |
| **sqlite-utils `.transform()`** (Python) | No — it's an execution primitive, not a diff engine | Yes, but only when you tell it explicitly what changed (`rename=`, `types=`, `drop=`) | Yes | Yes, but only if you declare it yourself | N/A (imperative API) |
| **Alembic batch mode** (Python) | Yes | Yes, with the same class of detection gaps as sqldef (CHECK/type comparisons often skipped) | Yes | Yes, but only if declared in the migration script | SQLAlchemy ORM models, not SQL |
| **Django migrations** (Python) | Yes | Yes, via automatic table rebuild under the hood | Yes | Yes — interactively asks "did you rename X to Y?" during generation | Django ORM models, not SQL |

The closest existing behavior (automatic diff + automatic rebuild + rename disambiguation) belongs to Django, and it's inseparable from Django's ORM as the schema's source of truth — adopting it means giving up a plain, hand-writable `schema.sql` in favor of Python model classes. Nothing combines that behavior with a plain-SQL source, in any language.

**Conclusion: this is a genuine gap, not a case of reinventing something that already exists.**

## Core Design Principles

**1. `schema.sql` (plain SQL DDL) is the single source of truth.** Not Go structs, not an ORM, not a custom DSL. It's what SQLite and every diffing approach already speak natively, and it stays hand-readable and hand-editable without a generator round-trip.

**2. Diffing is replay-based, never against a live database.** The "current" schema is computed by replaying every existing migration file, in order, into a fresh temporary/in-memory SQLite database and reading back its actual schema (`sqlite_master`). `generate` diffs `schema.sql` against that computed result. A live database can have drift (manual edits, a half-applied migration, divergent instances) — diffing against it means diffing against an unknown state. No separate snapshot file format is needed; the replay computes the diff target on demand.

**3. Destructive vs. safe is decided by comparing schemas, never by scanning generated SQL text.** This is the single most important correctness rule in the whole design. Every rebuild — even a completely safe one that changes nothing about which data exists — involves creating a new table and dropping the old one internally. A classifier that flags any statement containing the word `DROP` will misfire on the common case (e.g. adding a `CHECK` constraint to an existing column) and force a human decision that was never actually needed.

Instead: compute the column/table list from the old (replayed) schema and from `schema.sql`. If every column and table that existed before still exists after (regardless of type or constraint changes), the change is **safe** — generate the full rebuild automatically, no matter what SQL statements it internally requires. Only if a column or table that existed before is genuinely absent afterward is the change **destructive**.

**4. Safe changes are always auto-generated, including full rebuilds.** New tables, new columns, new indexes, renames (see below), type changes, constraint changes — all generated automatically: the new table, the `INSERT ... SELECT` copy, the drop of the old table, the rename, and the recreation of every index, trigger, view and foreign key that referenced the original table. This is the actual point of building this tool rather than using sqldef directly — sqldef doesn't attempt this at all.

**5. Destructive changes are still auto-generated — gated behind one explicit flag, not hand-written.** If the maintainer removed a column or table from `schema.sql`, that removal already *is* their decision; the tool shouldn't hand the work back to them as SQL to write by hand. `generate --allow-destructive` produces the migration (including the drop) with the same automatic rebuild machinery, backup, and verification as any other change. Without the flag: `generate` refuses outright, writes nothing, and reports exactly which column(s)/table(s) would be lost and which flag to rerun with. One flag, not one per statement kind (`DROP TABLE` and `DROP COLUMN` are the same kind of decision from the maintainer's side).

**6. Every generated migration is verified before it's ever written to disk.** After generating a candidate migration (safe or destructive), replay `schema.sql` into one temporary database and the full migration history plus the new candidate into another, then compare their resulting schemas. A mismatch means the generator produced something wrong — refuse to write the file rather than trust the generator blindly. This check is what actually enforces correctness; it doesn't depend on trusting any particular diffing library's own claims about what it changed.

## Rename Handling

No diff-based tool reliably distinguishes a rename from a drop-and-add on its own — confirmed across every generator checked (sqldef, Rothlis' migrator). But the tools that do handle it well (Drizzle Kit, Django) don't make the maintainer pre-declare anything either: they ask, interactively, at the moment `generate` notices the ambiguity.

**This tool follows that pattern, not a pre-declaration flag.** *Corrected — an earlier draft of this section required a `--rename` flag or a schema.sql annotation ahead of time, which just relocates the same friction rather than removing it: you'd still have to remember to declare something you already expressed by renaming it in schema.sql.* Instead: when `generate` sees a column or table disappear from one name while a plausibly-related one appears under another (matching type, position, or a high name-similarity heuristic), it asks directly in the terminal — "Rename `orders.customer_name` to `orders.buyer_name`? \[Y/n\]" — and on confirmation generates a `RENAME COLUMN`/`RENAME TABLE` (or the equivalent rebuild step), preserving the data, with **no destructive flag needed**, because nothing is actually lost. Declining, or running non-interactively without an answer, falls back to treating it as a genuine drop-and-add, correctly gated behind `--allow-destructive`.

For CI or scripted use where no terminal is attached, a flag (`--assume-renames` / `--assume-no-renames`) sets the default answer for every ambiguous case in that run, rather than requiring one flag per rename.

## Safety Model

Automating rebuilds is only defensible with a real safety net underneath it. This is what makes automatic generation trustworthy rather than reckless:

- **Automatic backup before any apply**, via SQLite's `VACUUM INTO` (never a raw file copy of a live database) — written to a temp name, fsynced, then atomically renamed into place. This is the actual undo mechanism: there are no down-migrations, reverting means restoring this snapshot.
- **Every apply is transactional**, on a single pinned connection, so a crash mid-migration can't leave a half-migrated database.
- **`PRAGMA foreign_key_check` and `PRAGMA integrity_check` run inside the transaction, before commit.** Any violation rolls back the whole batch, not just the offending statement.
- **Every generated migration is verified against `schema.sql` via the replay-based drift check (Core Design Principle 6) before it's written to disk** — a rebuild that doesn't reproduce the intended schema exactly is refused at generation time, not discovered later.
- **Checksums, not trust.** Every migration file's checksum is recorded when it's generated and verified again on every subsequent `generate` and every `apply`. An "immutable" migration file edited after being applied is caught, not silently accepted.
- **Dry-run is the default for `apply`.** It prints what would run; nothing touches the database until `--yes` is passed explicitly.
- **A database newer than the binary refuses to proceed** — an older binary must never apply against a schema state it doesn't recognize.
- **Drift between the real database and what replaying the migration journal produces is a hard stop**, not a warning — further applies are blocked until it's resolved or explicitly acknowledged.

## MVP Feature Scope

| In scope (v1) | Out of scope (later) |
| --- | --- |
| `schema.sql` as the single source of truth | Go-struct/tag schema definitions, alternative DSLs |
| Replay-based diffing (temp-DB replay vs. schema.sql, never a live database) | Live-DB-only diff mode |
| Schema-diff-based classification of destructive vs. safe changes (never scanning generated SQL text) | Multi-database support (Postgres/MySQL) |
| Full automatic 12-step rebuild generation for every safe structural change (type, constraint, rename), with index/trigger/view/foreign-key preservation | Query builder / ORM features |
| Single `--allow-destructive` flag gating genuine data loss (`DROP TABLE`, `DROP COLUMN`) — still auto-generated, not hand-written | Team/cloud collaboration features |
| Interactive rename confirmation during `generate` (heuristic match → "Rename X to Y? \[Y/n\]"), with `--assume-renames`/`--assume-no-renames` for non-interactive/CI use | GUI / web dashboard |
| Replay-based drift verification of every generated migration before it's written to disk | Automatic rollback beyond restoring a pre-migration snapshot |
| Migration journal (bookkeeping table) + per-file checksums, verified on every generate and apply |  |
| Automatic `VACUUM INTO` backup before every apply, atomic rename, retention pruning |  |
| `PRAGMA foreign_key_check` + `PRAGMA integrity_check` inside the migration transaction, before commit |  |
| Refuse to apply if the DB schema version is newer than the binary embeds, or an applied migration's checksum no longer matches |  |
| Dry-run by default; explicit flag required to apply |  |
| Ephemeral-DB drift check exposed as its own CLI command, for verifying a real deployed database independent of `generate`/`apply` |  |
| Installable agent skill (`skills/sqlite-migrate/SKILL.md`) teaching a downstream AI coding agent to drive this tool correctly in a consuming project |  |

Also embeddable as a Go library (`go:embed`-friendly), not just a standalone CLI — needed for use inside other single-binary Go projects.

## CLI Design (draft)

| Command | Purpose |
| --- | --- |
| `sqlite-migrate generate` | Diff `schema.sql` against the replayed schema. Safe changes (including full rebuilds) generate automatically. An ambiguous drop+add prompts interactively to confirm a rename; a genuinely destructive change is refused with a message naming exactly which column(s)/table(s) would be lost — rerun with `--allow-destructive` to generate it anyway |
| `sqlite-migrate check` | Static checks, no DB connection needed: verify every migration's checksum, verify the replay-based drift check still passes — the CI-friendly command |
| `sqlite-migrate apply` | Run pending migrations against a target DB inside one transaction, with automatic pre-migration backup and `foreign_key_check`/`integrity_check` before commit. Dry-run by default; `--yes` required to actually apply |
| `sqlite-migrate verify` | Ground-truth check against a real target database: replay the journal, compare against the database's actual schema, report drift or tampering |
| `sqlite-migrate status` | Show which migrations are applied vs. pending for a given database |

No `rollback` command: recovery is restoring the pre-apply `VACUUM INTO` snapshot, which exists specifically because rebuilds are auto-generated and need a safety net rather than a human reviewing every one by hand.

## Decisions on the Remaining Risks

**Type narrowing without `STRICT` tables — decided: require `STRICT` tables.** SQLite has supported `STRICT` since 3.37 (2021), it's the modern default recommendation from the SQLite project itself, and it closes this hole outright instead of bolting a warning system onto a fundamentally unenforced type system. `schema.sql` tables must be `STRICT`; `generate` refuses (or warns hard) on a non-`STRICT` table rather than trying to reason about type coercion risk.

**Constraint tightening failing loudly at apply time — decided: accepted v1 behavior, not a gap to close.** A `NOT NULL`/`CHECK` addition that existing rows violate fails the rebuild's copy step, rolls back cleanly, and loses nothing. Validating against live data at `generate` time would require a database connection at generation time, which breaks `generate`'s CI-friendly, connection-free design for no real safety gain — the failure is already safe, just late. Not worth the added complexity for v1.

**Concurrent schema authors / migration-numbering conflicts — decided: timestamp-based version identifiers, not sequential integers.** Use a sortable timestamp (`YYYYMMDDHHMMSS`) as the migration version instead of `0001`, `0002`, etc. — the same convention Rails, Django and Alembic settled on for exactly this reason. Two branches generating migrations independently almost never collide, and when they do, it's immediately visible instead of silently renumbering.

**Migration file naming — decided: `<timestamp>_<slug>.sql`, single file, no up/down pair.** One file per migration (e.g. `20260917143022_add_users_email.sql`), holding only the forward SQL — consistent with "no `rollback` command" (Core Design Principle-adjacent: recovery is the `VACUUM INTO` snapshot, not a down-migration). The slug comes from an optional `generate -m "<message>"` flag; when omitted, derive it from the single table the diff centers on (e.g. `add_users_email`), or fall back to `schema_change` when the diff touches several unrelated tables and no single name is representative. The slug is cosmetic only — the timestamp is the real identity, and the checksum lives in the bookkeeping table (Safety Model), never in the filename or a sidecar file, so a renamed file doesn't break verification.

**Multi-table rebuild ordering within one migration — decided: rebuild in FK-dependency order with `PRAGMA foreign_keys` off for the whole migration transaction, not per-statement.** When a single diff requires rebuilding two or more tables that reference each other (including a genuine cycle), per-statement FK enforcement can't be satisfied by any statement order — this is exactly why Runner.Apply (Phase 5) already suspends `PRAGMA foreign_keys` before `BEGIN` and only runs `foreign_key_check` once, before `COMMIT`, rather than enforcing it statement-by-statement. The rebuild generator orders the create/copy/drop/rename steps for all affected tables using the same dependency graph sqldef already builds for its own multi-table output (`schema.SortTablesByDependencies`), doing every affected table's create-and-copy before any of their drops, so a row can always find its (also-being-rebuilt) referenced table during the copy step; a genuine FK cycle across two rebuilt tables is resolved by the shared FK-check-at-commit-only design rather than requiring a topological order that can't exist. See testdata scenario 18.

## Naming and Licensing

**Name: `sqlite-migrate`.** Chosen over more "brandable" options (Sqoot, Hermit, Schemasmith, etc.) because a CLI tool is found by search intent, not brand recall — `sqlite-migrate generate` needs no explanation. **One caveat:** `simonw/sqlite-migrate` already exists on GitHub, a Python migration tool by Simon Willison in the Datasette/sqlite-utils ecosystem. No Go module-path collision (this lives under `github.com/mdg-labs/sqlite-migrate`), but the identical name in an adjacent, well-known ecosystem is a real discoverability risk worth a conscious decision, not a default: keep the name as-is (different language ecosystem, arguably fine), or add a qualifier (`go-sqlite-migrate`).

**License: MIT**, as the default recommendation. This is a library/CLI meant for broad adoption and embedding into other projects, including closed-source ones — AGPL (used for MDG Labs' hosted products like Hoserva and SlugBase, where it protects against a SaaS competitor reselling the service) would work against that goal here. MIT/Apache-2.0 is the standard choice for infra tooling in this shape (sqldef, golang-migrate and goose are all MIT/Apache).

**Language: Go.** Not because Hoserva happens to be Go, but because the research above found real, mature prior art for parts of this problem in Python (sqlite-utils, Django) and comparatively little in Go (only sqldef, which has the documented gaps this whole project exists to close). Building in Go fills an actual gap in that ecosystem rather than adding another entrant to an already well-served one.

## Repository Architecture

Module: `github.com/mdg-labs/sqlite-migrate`. Go ≥ 1.22. SQLite driver: `modernc.org/sqlite` (pure Go, no cgo — same choice Hoserva already made, and it keeps this tool cross-compilation-friendly for anyone embedding it).

**Pinned dependencies (decided at Phase 0 kickoff, recorded in `go.mod`, never left to whatever `go get` resolves that day):** `modernc.org/sqlite` and `github.com/sqldef/sqldef/v3` (see `sqldefwrap` below), each pinned to the latest tagged release at the moment Phase 0 starts — confirm the exact version with `go list -m -versions <module>` rather than hardcoding a number in this doc, since it will drift. Record the pinned versions in the Phase 0 commit message so they're auditable later.

**Split rule: public root package = runtime (what an embedding binary needs); `internal/` = generation-time tooling (what only the `sqlite-migrate` CLI itself needs).** Go's `internal/` visibility rule enforces this automatically — a consumer like Hoserva can only ever import the runtime, never accidentally depend on the generator internals.

```
sqlite-migrate/
├── go.mod
├── LICENSE                      # MIT
├── README.md
├── Makefile                     # test, lint, build, golden-update targets
├── migration.go                 # PUBLIC (package sqlitemigrate): Migration struct, Load()/LoadDir(), filename parsing
├── runner.go                    # PUBLIC: Runner, Apply() — transactional apply, bookkeeping table, FK/integrity checks
├── snapshot.go                  # PUBLIC: VACUUM INTO backup, atomic rename, retention pruning
├── drift.go                     # PUBLIC: replay-based drift check (used by generate-time verification AND the `verify` CLI command against a real DB)
├── checksum.go                  # PUBLIC: per-migration checksum compute/verify
├── *_test.go                    # unit + integration tests beside each file above
├── cmd/
│   └── sqlite-migrate/
│       └── main.go              # CLI entrypoint — thin, wires internal/ generation tooling + the public runtime together
├── internal/
│   ├── schemadiff/              # schema.sql → structured schema (via temp-DB replay + sqlite_master/PRAGMA reads, not a hand-written SQL parser), diff, safe/destructive classification
│   │   ├── parse.go
│   │   ├── diff.go
│   │   ├── classify.go
│   │   └── *_test.go
│   ├── rename/                  # rename-candidate heuristic (type/position/name-similarity match on a drop+add pair) + interactive CLI prompt
│   │   ├── detect.go
│   │   ├── prompt.go
│   │   └── *_test.go
│   ├── rebuild/                 # the 12-step rebuild SQL generator — the actual core differentiator of this project
│   │   ├── generator.go         # CREATE new → INSERT...SELECT → DROP old → RENAME
│   │   ├── preserve.go          # recreate indexes/triggers/views/FKs that referenced the rebuilt table
│   │   └── *_test.go
│   └── sqldefwrap/              # isolated wrapper around the sqldef library — handles the "easy" cases (CREATE TABLE, ADD COLUMN incl. REFERENCES) that sqldef already gets right
│       ├── wrapper.go
│       └── *_test.go
├── testdata/
│   ├── schemas/                 # named before/after schema.sql pairs, one per scenario (see Testing & Verification Strategy)
│   └── golden/                  # expected generated migration SQL per scenario
└── .github/
    └── workflows/
        └── ci.yml               # go build, go vet, go test -race, golangci-lint
```

**Why `sqldefwrap` stays instead of being replaced:** Core Design Principles established that sqldef's narrow job (CREATE TABLE, ADD COLUMN) is already correct and shouldn't be reinvented — only the rebuild path (which sqldef doesn't attempt) is genuinely new work. Keeping it behind an internal interface means it can be swapped later without touching the public API or the rebuild generator.

**Concrete API to wrap (decided, so Phase 4 isn't a research spike):** sqldef exposes a pure string-in/string-out diff function that needs no database connection — `schema.GenerateIdempotentDDLs(mode schema.GeneratorMode, sqlParser database.Parser, desiredSQL string, currentSQL string, config database.GeneratorConfig, defaultSchema string) ([]string, error)` in `github.com/sqldef/sqldef/v3/schema`, called with `schema.GeneratorModeSQLite3`. This is the right layer to wrap — not the top-level `sqldef.Run()` (which drives a live `database.Database` connection and is built for the CLI, not for embedding) and not the `database/sqlite3` package (which exports/imports against a real DB). `sqldefwrap.Diff(desiredDDL, currentDDL string) ([]string, error)` becomes a thin call into `GenerateIdempotentDDLs`, feeding it the two DDL strings `internal/schemadiff` already has on hand from its own replay step — no second database connection needed on sqldef's side.

## Development Roadmap

Sequenced so each phase is independently testable and the highest-risk logic (classification, rebuild generation) is built and hardened before anything is wired into a CLI. Each phase lists its deliverable and what "done" means — an agent building this should not move to the next phase until the current one's acceptance criteria pass.

**Phase 0 — Scaffolding** Deliverable: repo skeleton exactly as in Repository Architecture, `go.mod` initialized with the pinned dependency versions recorded above, empty package files with doc comments stating each package's responsibility, `.golangci.yml` (sane Go defaults — `govet`, `staticcheck`, `errcheck`, `unused` at minimum), CI workflow running `go build ./... && go vet ./... && go test ./... && golangci-lint run` on an empty tree, `LICENSE` (MIT), `Makefile` with `test`/`lint`/`build` targets. Done when: CI is green on the empty skeleton.

**Phase 1 — Schema parsing & classification (`internal/schemadiff`)** Deliverable: parse a `schema.sql` string into a structured schema by replaying it into a temporary SQLite database and reading back `sqlite_master` + `PRAGMA table_info`/`PRAGMA foreign_key_list`/`PRAGMA index_list` (reuse SQLite's own parser via execution, never hand-write a SQL grammar). Diff two structured schemas. Classify the diff as safe or destructive per Core Design Principle 3 (column/table presence, not SQL text). Done when: unit tests pass for every case in the testdata matrix below (Testing & Verification Strategy) — add table, add column (with/without constraints/FK), type change, constraint add/change, drop column, drop table, and combinations of the above in one diff.

**Phase 2 — Rebuild generator (`internal/rebuild`)** Deliverable: given a table classified as needing a rebuild, generate the full 12-step SQL (create new table → `INSERT ... SELECT` with explicit column mapping → drop old → rename → recreate every index/trigger/view/foreign key that referenced the original table). Done when: golden-file tests pass — generated SQL matches the recorded expected output for each testdata scenario, AND executing that SQL against a seeded SQLite database (real rows, not just an empty schema) preserves every row's data correctly and every index/trigger/view still exists and still works.

**Phase 3 — Rename detection (`internal/rename`)** Deliverable: heuristic that flags a dropped-column-plus-added-column pair (or dropped-table-plus-added-table) within the same diff as a rename candidate when type and structural position are compatible; an interactive CLI confirmation step; `--assume-renames`/`--assume-no-renames` flags for non-interactive runs. Done when: unit tests cover true renames, false positives (an unrelated drop+add that happens to share a type) resolved correctly by the flag/prompt answer, and the CLI prompt is exercised via a scripted-input integration test.

**Phase 4 — sqldef integration (`internal/sqldefwrap`)** Deliverable: thin wrapper around sqldef's SQLite generator as a Go library (never the CLI binary) for the cases it already handles correctly — new tables, new columns including `REFERENCES`. Isolated behind an interface internal to this package. Done when: wrapper output is verified against a small independent test suite covering the known sqldef edge cases (a `REFERENCES` clause on `ADD COLUMN`, a `DEFAULT` combined with a foreign key — which SQLite itself refuses and must surface as a clear error, not a silently wrong statement).

**Phase 5 — Migration runtime (public API: `migration.go`, `runner.go`, `snapshot.go`, `drift.go`, `checksum.go`)** Deliverable: `Migration` struct, `Load`/`LoadDir` (with `go:embed` support), checksum compute/verify, `Runner.Apply()` (single transaction, `PRAGMA foreign_keys` suspended before `BEGIN`, `foreign_key_check`+`integrity_check` before `COMMIT`, bookkeeping table), `Snapshot()` (`VACUUM INTO`, temp-name-then-atomic-rename, retention pruning), replay-based `CheckDrift()`. Done when: integration tests run against real on-disk SQLite files (not just in-memory), including a deliberately failing migration (bad constraint) proving the transaction rolls back cleanly and the database is byte-identical to before the attempt.

**Phase 6 — `generate` command** Deliverable: wire phases 1–4 together — replay journal → diff against `schema.sql` → classify → route to `sqldefwrap` (easy cases) or `rebuild` (hard cases) → resolve rename ambiguity → gate destructive changes behind `--allow-destructive` → verify the candidate via `drift.go` before writing → write the migration file + checksum. Done when: end-to-end tests cover every testdata scenario from a cold repo (no existing migrations) through several sequential schema.sql changes.

**Phase 7 — `apply`, `check`, `verify`, `status` commands** Deliverable: wire the remaining CLI commands to the public runtime package. Dry-run output by default for `apply`; `--yes` required to execute. Done when: a CLI integration test runs a realistic multi-migration upgrade sequence against a real SQLite file end-to-end (generate several migrations, apply them, verify against the result, check status).

**Phase 8 — Hardening** Deliverable: fuzz/mutation testing specifically on the classifier and rebuild generator — this is where Hoserva's own experience (10 review rounds, all in the hand-rolled tokenizer/classifier layer, none in sqldef itself) says the real risk lives. Edge cases: quoted identifiers, SQL comments inside `schema.sql`, triggers with nested `BEGIN...END`, case-insensitive keyword matching. Done when: a mutation-testing run against `internal/schemadiff` and `internal/rebuild` hits a high kill rate (treat anything below \~90% as needing more tests, not as acceptable).

**Phase 9 — Docs & release** Deliverable, two distinct doc surfaces — never merge them:

- **README** (repo root): quickstart + full CLI reference, the front door for anyone landing on GitHub.
- **Public user-docs site, published via GitHub Pages**, source in a `docs/public/` directory (plain Markdown, no generator dependency needed for an MVP CLI's docs — GitHub Pages' built-in Jekyll renders it via a `_config.yml`), deployed by `.github/workflows/pages.yml` (`actions/deploy-pages` on push to `main`) rather than the "deploy from branch" `/docs`-folder option, specifically so it never picks up `docs/internal/` (maintainer-facing spec/design docs, which stay unpublished). Covers: installation, quickstart, full CLI reference (`generate`/`check`/`apply`/`verify`/`status`), the safety model in plain terms (backup/restore via the `VACUUM INTO` snapshot, the destructive-change gate, why `STRICT` is required), and an FAQ. This is user documentation — how to use the tool — not a restatement of this design doc.

`v0.1.0` tag, release workflow. Done when: a person who has never seen this project can `go install` it and generate + apply their first migration from the README alone, and the Pages site is live and covers all five CLI commands plus the safety model.

**Third deliverable — an installable agent skill, not a doc page.** This is for the case where a *downstream* project has adopted `sqlite-migrate` and its maintainer is now driving schema changes through an AI coding agent (Claude Code, Cursor, etc.) rather than by hand. The convention is the open agent-skills ecosystem CLI, [`vercel-labs/skills`](https://github.com/vercel-labs/skills) (npm package `skills`, MIT-licensed, 75+ supported agents including Claude Code) — a source repo publishes a skill simply by holding a `skills/<name>/SKILL.md` at its root; no npm publish step, no registry submission. Add `skills/sqlite-migrate/SKILL.md` to this repo, installable by any consuming project's maintainer with:

```
npx skills add mdg-labs/sqlite-migrate --skill sqlite-migrate
```

Content: how to author/edit `schema.sql` (the `STRICT` requirement, why it's the only source of truth), when to run `generate` vs. `check` vs. `apply` vs. `verify` vs. `status`, how to read and correctly answer the interactive rename prompt, what `--allow-destructive` actually gates and why an agent should never pass it reflexively to get past a refusal, the non-interactive flags for CI (`--assume-renames`/`--assume-no-renames`, `check`'s exit codes), and where the `VACUUM INTO` snapshot lives for manual recovery. This is a using-the-tool skill for a downstream agent — distinct from this repo's own internal build-orchestration skill (Development Roadmap workflow, not published, never installed by `npx skills add`). Done when: a scripted install (`npx skills add` against this repo) succeeds and the resulting `SKILL.md` alone lets an agent complete testdata scenario 07 (a type change) end-to-end without consulting anything else.

## Testing & Verification Strategy

**Golden-file testing is the backbone.** For every scenario, `testdata/schemas/<NN_name>/before.sql` and `after.sql` define the two schema states, and `testdata/golden/<NN_name>.sql` holds the exact expected generated migration. A test runs the generator against `before.sql`→`after.sql` and diffs the result against the golden file byte-for-byte; a deliberate change to expected output goes through `make golden-update` (regenerate + manual review + commit), never a silent overwrite.

**Required scenario matrix for Phase 1–2 (expand as edge cases are found, never shrink):**

| # | Scenario | Expected classification |
| --- | --- | --- |
| 01 | New table | Safe — direct `CREATE TABLE` |
| 02 | New column, no constraints | Safe — direct `ADD COLUMN` |
| 03 | New column with `NOT NULL DEFAULT` | Safe — direct `ADD COLUMN` |
| 04 | New column with `REFERENCES` | Safe — direct `ADD COLUMN`, FK clause preserved |
| 05 | New column with `REFERENCES` + non-NULL default | Safe — direct `ADD COLUMN`; SQLite only rejects this combination once the table already holds rows and `foreign_keys` enforcement is on, a state neither `generate` (schema-only replay) nor `Runner.Apply` (which suspends `foreign_keys` for the whole migration transaction) ever reaches |
| 06 | New index | Safe — direct `CREATE INDEX` |
| 07 | Existing column's type changed | Safe — full rebuild, no column lost |
| 08 | `CHECK` constraint added to existing column | Safe — full rebuild |
| 09 | `UNIQUE` constraint added | Safe — full rebuild |
| 10 | Foreign-key action changed (e.g. `ON DELETE`) | Safe — full rebuild |
| 11 | Rebuild on a table with an existing index/trigger/view referencing it | Safe — rebuild + index/trigger/view recreated, verified still functional after |
| 12 | Column dropped from `schema.sql` | Destructive — refused without `--allow-destructive` |
| 13 | Table dropped from `schema.sql` | Destructive — refused without `--allow-destructive` |
| 14 | Column renamed (type/position match) | Rename candidate — prompts; on confirm, `RENAME COLUMN`, data preserved; on decline, falls through to destructive gate |
| 15 | Table renamed | Same as 14, table-level |
| 16 | Two unrelated changes in one diff (e.g. new column + type change elsewhere) | Both handled independently, one migration file |
| 17 | `STRICT` table required but `schema.sql` table isn't marked `STRICT` | Refused with a clear error (Known Open Risks decision) |
| 18 | Two tables in one diff both need a rebuild and reference each other via FK (including a genuine cycle) | Safe — both rebuilt in one migration per the FK-dependency-order decision above; `foreign_key_check` passes at commit, no per-statement FK order required |
| 19 | `CHECK` constraint added to a column whose name contains a non-ASCII (multi-byte UTF-8) character, left unquoted | Safe — full rebuild; found by Phase 8 fuzzing: `internal/schemadiff`'s hand-rolled `sqlTokens` tokenizer only recognized ASCII identifier bytes, so an unquoted non-ASCII identifier fragmented into one garbled single-byte token per raw UTF-8 byte instead of staying fused into its own token — fixed by treating any byte with the high bit set as an identifier byte, matching SQLite's own tokenizer and `internal/rebuild`'s existing `isIdentByte` |
| 20 | `CHECK` constraint added to a column of a table whose name contains an unquoted `$` character (e.g. `foo$bar`) | Safe — full rebuild; found by Phase 8 fuzzing: `internal/rebuild`'s `isIdentByte` (used by `renameCreateTableSQL`'s identifier scanner) and `internal/schemadiff`'s `sqlTokens` didn't recognize `$` as an identifier byte, even though SQLite itself does (verified directly: `CREATE TABLE foo$bar (...)` is accepted unquoted) — `renameCreateTableSQL` spliced the new table name in mid-identifier, producing unparseable SQL. Fixed by adding `$` to both scanners' identifier-byte sets. Two further Phase 8 fuzzing findings, not expressible as a diff-classification scenario, are covered by unit tests instead: `schemadiff.Parse` and `rebuild`'s `loadCatalog` now refuse a schema/SQL string containing a NUL byte outright, since `modernc.org/sqlite` silently stops executing at the first one with no error, which would otherwise drop every table/column/trigger/view declared after it without a trace; and `rebuild`'s `containsIdentifierWord` (the bare-word half of `referencesTable`'s trigger/view dependency scan) now returns false immediately for an empty needle instead of panicking on an out-of-range slice, reachable because SQLite allows an empty *quoted* table name (`CREATE TABLE "" (...)`) |

**Phase 5 runtime tests run against real on-disk SQLite files**, not only `:memory:` — `VACUUM INTO` and file-rename atomicity can't be verified any other way. Include at least one test that kills the process (or simulates the equivalent) mid-transaction and confirms the database file is unchanged.

**Phase 8 fuzzing/mutation targets `internal/schemadiff` and `internal/rebuild` specifically** — not the CLI or the runtime — because that's precisely where Hoserva's own build history shows real bugs concentrate (a from-scratch SQL tokenizer and drift comparator, not the generator sitting in front of it). Any new bug found here becomes scenario `NN` in the table above before it's fixed, so it can never silently regress.

## Next Steps

**Naming — decided: keep `sqlite-migrate` as-is.** The `simonw/sqlite-migrate` collision is Python/Datasette-ecosystem; Go tooling searches don't meaningfully overlap with it in practice, and the CLI command clarity outweighs the discoverability risk. No qualifier needed.
