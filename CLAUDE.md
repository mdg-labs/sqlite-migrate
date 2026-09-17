# sqlite-migrate

A Go CLI + embeddable library that generates and safely applies SQLite
schema migrations from a plain-SQL `schema.sql` source of truth, including
full automatic rebuilds for changes SQLite's native `ALTER TABLE` can't
express. The full design — problem statement, safety model, repository
architecture, and the phased Development Roadmap this project is built
from — lives in [`docs/internal/sqlite-migrate — Spec MVP Doc.md`](docs/internal/sqlite-migrate%20—%20Spec%20MVP%20Doc.md).
Read it before touching anything; it is the single source of truth for
*why* the code is shaped the way it is, not just what it does.

## Working model

This repo is built phase-by-phase against that doc's Development Roadmap,
using the `build` skill (`.claude/skills/build/`) — an agent orchestrator
that dispatches `task-executor`/`task-verifier` subagent pairs in isolated
scratch clones, one phase (or triaged issue) at a time, and lands a passing
attempt as a pull request against `main` for the maintainer to merge by
hand. There is no `beta` branch here: `main` is what `go install` and every
downstream consumer pulls, so nothing reaches it without a human reading
the diff — see the `build` skill for the full landing model.

- **Never push directly to `main`.** Every change lands as a PR, always.
- **Never close an issue by hand** (`gh issue close`) unless the maintainer explicitly asks — closing happens via a merged PR's `Fixes #` trailer.
- Don't open an issue for something finished in the same session — that's bookkeeping theatre.

## Label set

| Kind | Labels |
|---|---|
| Type (exactly one) | `feat`, `bug`, `chore`, `docs`, `spike` |
| Area | `area:schemadiff`, `area:rebuild`, `area:rename`, `area:sqldefwrap`, `area:runtime`, `area:cli`, `area:docs`, `area:ci`, `area:skill` |
| Extras | `epic`, `data-loss-risk`, `blocked` |
| Status (machine-managed) | `status:new`, `status:ready`, `status:in-progress`, `status:in-review`, `status:implemented`, `status:closed`, `status:cancelled` |

`data-loss-risk` stands in for what Hoserva calls `safety-critical`: it
marks work touching the destructive-vs-safe classifier
(`internal/schemadiff/classify.go`), the rebuild generator
(`internal/rebuild/`), the `VACUUM INTO` backup/restore path
(`snapshot.go`), checksum verification (`checksum.go`), or the replay-based
drift check (`drift.go`) — anywhere a plausible bug silently loses or
corrupts a user's database. There is no `needs-sudo` label and no hardware
tier here: nothing in this project touches the host beyond the working
directory and a SQLite file the user names.

## Area → paths

| Area | Paths |
|---|---|
| `area:schemadiff` | `internal/schemadiff/` |
| `area:rebuild` | `internal/rebuild/` |
| `area:rename` | `internal/rename/` |
| `area:sqldefwrap` | `internal/sqldefwrap/` |
| `area:runtime` | `migration.go`, `runner.go`, `snapshot.go`, `drift.go`, `checksum.go`, and their `*_test.go` |
| `area:cli` | `cmd/sqlite-migrate/` |
| `area:docs` | `README.md`, `site/`, `docs/internal/` (see note below) |
| `area:ci` | `.github/workflows/`, `Makefile`, `.golangci.yml` |
| `area:skill` | `skills/sqlite-migrate/` |

**`docs/internal/` is maintainer-facing** (this spec doc, decision records)
and is never the source for the published GitHub Pages site — that source
is `site/`. A `docs` issue touching `docs/internal/` and one touching
`site/` both carry `area:docs`; the scope note in the issue body says which
path, so `build` can still bound it precisely.

**Always-shared files** — any change touching them serializes against every
other change that does: `CLAUDE.md`, `Makefile`, `go.mod`, `go.sum`,
`.gitignore`, `LICENSE`, `docs/internal/sqlite-migrate — Spec MVP Doc.md`
(the spec itself — a change here is a design decision, not routine code).

## Spikes

A `spike` issue's deliverable is **recorded findings, not product code**: a
findings section added to the spec doc (or a new file under
`docs/internal/`) and the exact commands/outputs that support it. Use it
for anything the spec doc left an open question — e.g. verifying a sqldef
or `modernc.org/sqlite` API detail before Phase 4/0 depends on it.

## Conventions

- Go: `gofmt`, `go vet`, `golangci-lint`; errors wrapped with context (`fmt.Errorf("…: %w", err)`); `context.Context` first parameter on anything that does IO.
- No comments unless the *why* is non-obvious. No speculative abstraction. No half-finished work. No error handling for cases that cannot happen.
- Golden files (`testdata/golden/`) change only deliberately — a golden diff is explained in the commit message and goes through `make golden-update`, never a silent regeneration to make a test pass.
- Public root package (`sqlitemigrate`) vs. `internal/`: see the spec doc's "Split rule." A consumer embedding this library must never need anything under `internal/`.
