---
name: build
description: Autonomously implement and verify sqlite-migrate — either the next unbuilt phase(s) of the Development Roadmap (default), or a specific GitHub issue/epic number — using sonnet execution agents in isolated scratch clones, one independent verifier per attempt, landing a PASS as a pull request against `main` (never a direct push, never auto-merged). Parallelizes phases/issues with disjoint file scope, serializes overlapping ones. Use when asked to "build sqlite-migrate", "run the next phase", "work on issue #n", "implement epic #n", or "run the build skill".
argument-hint: [phase-number | issue-number] (omit for "next unbuilt phase")
allowed-tools:
  - Read
  - Grep
  - Glob
  - Agent
  - AskUserQuestion
  - Bash
---

# build

Turns the sqlite-migrate Development Roadmap — or a specific triaged GitHub
issue/epic on `mdg-labs/sqlite-migrate` — into pull requests against `main`,
with no human in the loop except at a genuine blocker (a repeated
verification failure or an external unmet dependency) or at the PR itself,
which the maintainer always reads and merges by hand.

**This is the abbreviated sibling of Hoserva's `orchestrate` skill** —
same executor/verifier-in-isolated-clone pattern, same status-label
lifecycle, same escalation rules — with everything specific to Hoserva's
disk/VM hazards, its `beta`/`main` split, and its Discord notification
dropped, and the landing model changed to fit a public library with no
staging branch: **every landing is a pull request, never a direct push.**

**You (the current session) are the orchestrator.** You spawn
`task-executor` and `task-verifier` subagents and drive the loop yourself —
this skill is not itself a subagent. Follow the steps in order. A full
roadmap run takes a while; give the user a short progress update at the
start of each wave and after each opened PR, rather than going quiet.

## 0. Determine the target: roadmap mode or issue mode

- **No argument, or a phase number, or "next phase"** → **Roadmap mode**:
  build the Development Roadmap in the spec doc
  (`docs/internal/sqlite-migrate-spec-mvp-doc.md`), starting from the
  first phase with no open or merged PR against it (check
  `gh pr list --repo mdg-labs/sqlite-migrate --state all --search "Phase in:title"`
  and this repo's own `Phase N` commit trailers/tags). A phase number
  argument builds starting there instead, skipping the auto-detection —
  useful for a deliberate re-run.
- **An issue or epic number** → **Issue mode**, identical in shape to
  Hoserva's `orchestrate`: resolve sub-issues if it's an epic (`gh api
  repos/mdg-labs/sqlite-migrate/issues/<N>/sub_issues`), pull each issue's
  body/comments/relationships, and build only those. This is what a
  post-MVP bug or feature filed via `.claude/skills/triage` goes through.

Both modes share everything from step 3 on. Roadmap mode differs only in
step 1 (the "issues" are the phase epics, already known in advance rather
than discovered via `gh`) and step 2 (there is no `needs-sudo` concept —
skip it).

## 1. Roadmap mode: the phase dependency graph is static, not derived

Unlike Hoserva's issue-scope derivation (which has to work out dependencies
from `blockedBy` at runtime because any issue can exist), sqlite-migrate's
MVP roadmap is ten fixed phases with a known dependency shape from the spec
doc's Repository Architecture — file scope per phase barely overlaps except
where a phase explicitly builds on another's types. Use this wave plan
directly, cross-checked against each phase epic's native `blockedBy` (set
when the phase epics were seeded — see "Seeding the roadmap as issues"
below) rather than re-deriving it:

| Wave | Phases | Why parallel-safe |
|---|---|---|
| 1 | Phase 0 (Scaffolding) | Everything else needs the skeleton |
| 2 | Phase 1 (`internal/schemadiff`), Phase 4 (`internal/sqldefwrap`), Phase 5 (public runtime: `migration.go`/`runner.go`/`snapshot.go`/`drift.go`/`checksum.go`) | Disjoint file scope, none depends on another |
| 3 | Phase 2 (`internal/rebuild`), Phase 3 (`internal/rename`) | Both depend on Phase 1's structured-schema types; disjoint from each other |
| 4 | Phase 6 (`generate` command) | Wires Phases 1–4 together |
| 5 | Phase 7 (`apply`/`check`/`verify`/`status` commands) | Depends on Phase 5 (runtime) and Phase 6 (CLI wiring conventions) |
| 6 | Phase 8 (Hardening — fuzz/mutation on `schemadiff`+`rebuild`) | Depends on Phases 1–2 existing to harden |
| 7 | Phase 9 (Docs, GitHub Pages site, the published `skills/sqlite-migrate` agent skill, release) | Needs the whole tool working to document and demo |

If a phase epic's actual `blockedBy` edges (from seeding) disagree with this
table — someone re-scoped a phase by hand — the native edges win; say so and
continue.

## 2. Seeding the roadmap as issues (one-time, before the first `build` run)

If the phase epics don't exist yet (`gh issue list --repo
mdg-labs/sqlite-migrate --label epic` comes back empty or missing phases),
seed them via `.claude/skills/triage` before dispatching anything — don't
create issues from inside this skill; that's triage's job and it knows the
issue-body shape. Each phase becomes one epic (`Phase N — <deliverable>`,
labeled `epic` + the phase's dominant `area:*` + `feat`/`chore`/`docs` as
fits, body quoting that phase's Deliverable/Done-when from the spec doc
verbatim), `--add-blocked-by` the previous phase's epic (Phase 0 has none),
and `data-loss-risk` on Phases 1, 2, 5, and 8 specifically (classifier,
rebuild generator, runtime backup/restore, and hardening of both). Large
phases (2, 5, 7) may get real sub-issues under their epic for the distinct
pieces of work the Deliverable lists (e.g. Phase 5 could split into
`Migration`/`Runner`/`Snapshot`/`Drift+Checksum`); small phases (0, 4) stay
a single issue. This is exactly what `github-triage`'s "seed Phase N" mode
in Hoserva does — same shape, invoked here as `triage`.

## 3. Determine each unit's file scope

Same rule as Hoserva's step 3: backtick-quoted paths in the issue body,
falling back to `CLAUDE.md`'s area → paths table via its `area:*` label.
**Always-shared files** (`CLAUDE.md`, `Makefile`, `go.mod`, `go.sum`,
`.gitignore`, `LICENSE`, the spec doc itself) are their own scope entry
whenever a unit plausibly touches them — Phase 0 touches nearly all of
them, which is exactly why it runs alone in wave 1. Can't confidently bound
it → scope is the whole repo, which serializes it against everything in its
wave.

## 4. Batch into waves, bundles, lanes

**Roadmap mode:** the wave table in step 1 already gives you the waves;
each phase is its own unit (never bundled — a phase's Deliverable is
already the right size for one executor). Within a wave, phases run in
parallel lanes since their scopes are disjoint by construction.

**Issue mode:** identical to Hoserva's orchestrate — waves by same-run
`blockedBy`, bundle only when scopes are correlated or the items are small
and adjacent sharing an `area:*` (cap 3, never a `data-loss-risk` issue,
never a fix round, never a `spike`), lanes by first-fit non-overlapping
scope. Print the plan before dispatching; if the unit count exceeds ~12,
confirm via `AskUserQuestion` first.

## 5. Per dispatch unit: isolated scratch clone

Never work in the real repo; never share a clone between concurrent units.

```
mkdir -p <scratchpad dir>/build
git clone <real repo path> <scratchpad dir>/build/<unit-id>-a1
```

`<unit-id>` is the phase number (`phase-2-a1`) or issue number(s)
(`57-a1`, or `57+58-a1` for a bundle). Inside the clone, the executor works
on branch `phase-<n>` or `issue-<n>` (created from whatever the clone's
`HEAD` was cloned from — see step 8's stacking rule).

A **verification-FAIL retry** (step 9) reuses the same clone so the
executor can amend. Only a **rebase retry** (step 8) or a fix round after a
bundled attempt re-clones fresh.

## Status labels — exactly one, always

Every issue/epic carries exactly one `status:*` label, transitioned only
via `scripts/issue-status.sh <n> <status>` (agents run their clone's copy;
you run it from the real repo) and rolled up for an epic via
`scripts/epic-status.sh <epic>`. Same lifecycle table as Hoserva:
`new`→`ready` (triage) →`in-progress` (executor claims) →`in-review`
(executor commits) →`implemented` (verifier PASS) / back to `in-progress`
(verifier FAIL). **You never set `status:closed`** — that's the
`issue-status` workflow, stamped when a merged PR's `Fixes #` trailer
closes the issue, which for most work is later than this run's opened PR.
An issue leaving your hands still open (executor `blocked`, or escalated
after three FAILs) goes back to `status:ready`.

## 6. Dispatch `task-executor`

Read `.claude/skills/build/templates/executor-prompt.md` and fill every
`{{…}}` token: the shared preamble once, then the per-issue block once per
issue in the unit — number, title, body, comment thread (empty/omitted in
roadmap mode where the "issue" is a phase epic with no separate thread
until triage created sub-issues), scope, and whether it carries
`data-loss-risk` or is a `spike`. On a fix round, the rejected SHA and the
verifier's findings verbatim.

```
Agent({
  subagent_type: "task-executor",
  model: "sonnet",
  description: "Implement <unit-id>",
  prompt: <the filled template>
})
```

**All lane-head dispatches for a wave go in one assistant message**, so
they run concurrently.

A bundle (issue mode only) produces **one commit per issue**, each with
only that issue's files and its own `Fixes #<n>` trailer. Blocked is per
issue: a bundle that committed issue 1 and blocked on issue 2 hands you a
real commit for issue 1.

If a unit comes back `blocked`: report the reason, put it back to
`status:ready`, leave it open, and continue with the rest that doesn't
depend on it.

### Waiting: end the turn, don't schedule anything

Subagents re-invoke you when they finish. Once everything dispatchable for
this wave is out the door, say in one line what you're waiting on and
**end your turn.** Never `ScheduleWakeup`, `Monitor`, or `sleep`; never
re-dispatch because you haven't heard back. When a notification arrives,
route its findings (step 11), verify that unit (step 7), and dispatch the
next unit in its lane.

## 7. Dispatch `task-verifier`

Read `.claude/skills/build/templates/verifier-prompt.md` and fill it: per
issue, its details, the same comment thread, its scope, its flags, and its
own commit SHA; once, the workspace, attempt number and epic. On a fix
round, fill the `FIX_ROUND` block too: the rejected SHA, where it can be
read, and the previous round's **blocking** findings verbatim.

```
Agent({
  subagent_type: "task-verifier",
  model: "opus",      // when any issue in the unit carries data-loss-risk
  model: "sonnet",    // otherwise
  description: "Verify <unit-id> attempt <n>",
  prompt: <the filled template>
})
```

**One verifier per unit per attempt**, verdicts **per issue**: all six
layers run against each commit separately, one comment and one label move
per issue. **Only blocking findings fail an issue**; notes are recorded in
the comment and go nowhere else. The verifier posts its own comments and
moves its own labels; read its returned verdicts rather than re-deriving
them from GitHub.

## 8. On PASS — open a pull request, never push directly, never auto-merge

This is the one step that genuinely differs from Hoserva's `orchestrate`,
which pushes straight to `beta`. sqlite-migrate has no staging branch —
`main` is what `go install` and every embedding consumer pulls — so
**nothing reaches it without the maintainer reading and merging a PR.**

- **Push the unit's branch**, from the scratch clone, to the real GitHub
  remote:
  ```
  git -C <clone> push origin <branch-name>
  ```
  (First time this run pushes anything: confirm `git -C <clone> remote get-url origin`
  actually points at `github.com/mdg-labs/sqlite-migrate`, not the local
  path it was cloned from — a scratch clone's default `origin` is the local
  repo, and you must add/point the real one before this push.)
- **Decide the PR's base branch — this is the stacking rule.** If the
  phase/issue this unit depended on has already been merged into the real
  `main` (check `gh pr list --repo mdg-labs/sqlite-migrate --state merged
  --search "Phase <n-1> in:title"` or the dependency's own status), base
  this PR on `main`. If it hasn't merged yet, base this PR on **that
  dependency's own PR branch** instead (a stacked PR) — say so plainly in
  the PR body ("Stacked on #<PR>; do not merge before it") so the
  maintainer merges in order. This is what lets roadmap mode keep moving
  through waves without stalling on human review turnaround.
- **Open the PR:**
  ```
  gh pr create --repo mdg-labs/sqlite-migrate \
    --base <main or the stacked branch> \
    --head <branch-name> \
    --title "<the executor's commit subject, or 'Phase N — <deliverable>' for a phase>" \
    --body-file <filled from the executor's report + verifier's PASS comment link>
  ```
  The PR body includes each commit's `Fixes #<n>` (GitHub only auto-closes
  on merge into the *default* branch, so a stacked PR's trailers take
  effect once it's merged up the stack — note that in the body too).
- **`data-loss-risk` PRs get a bold callout at the top of the body**:
  "Touches the destructive classifier / rebuild generator / backup-restore
  path — read line by line before merging." Never merged by you, ever —
  same rule as Hoserva's `safety-critical`, just enforced by "you never
  merge anything" instead of "you don't push this one."
- **Keep a local integration branch so dependent waves don't stall on
  review turnaround.** Maintain `build-integration` in the *real* repo
  (never pushed, never the branch a PR is opened from) as a running
  fast-forward of every unit that reached PASS this run, merged in
  dependency order. Clone the next wave's units from `build-integration`,
  not from `main` — this is the equivalent of Hoserva cloning from local
  `beta` before it's necessarily pushed. `build-integration` is scaffolding
  for this run only; it is never itself pushed or PR'd, and gets deleted at
  the end of a fully-landed run (step 12).
- **If the push is rejected** (`main` or the stacked base moved
  unexpectedly), don't force it — report it and stop touching that remote
  for the rest of this run.

Once every issue in the unit has a PR open (or is skipped as FAILed):
delete the scratch clone. There is no lab/VM teardown step here — nothing
in this project touches host state beyond the clone's own directory and
its `go`/`golangci-lint` runs.

## 9. On FAIL — fix, then re-verify, capped at 3 attempts

Identical to Hoserva's step 9: a fix round is always single-issue (a FAIL
dissolves its unit). A single-issue attempt gets a fresh `task-executor` in
the **same clone** (template branch `FIX_ROUND_SAME_WORKSPACE`, amends); a
bundled attempt re-clones fresh from current `build-integration` (template
branch `FIX_ROUND_FRESH_CLONE`). The attempt counter carries over; a fresh
verifier gets the `FIX_ROUND` block filled and re-runs every check,
confirming closure rather than restarting the review.

If attempt 3 also fails: stop. Put the issue back to `status:ready`, then
`AskUserQuestion` with the latest findings — keep trying / hand it to the
maintainer / skip for now. Delete its clone.

## 10. Repeat until the target set is empty

Move to the next wave once every unit in the current one has landed (PR
opened), been skipped as blocked, or been escalated — including anything
step 11 pulled in.

## 11. Route what the run surfaces — as it arrives, not at the end

Identical to Hoserva's step 11: for every finding outside a unit's own
issue(s), check it's real against the file/line/command it names, check
whether an open issue already covers it (extend rather than duplicate),
otherwise file it now via `.claude/skills/triage` (create mode) and decide
whether it belongs to this run (pull it into the target set, place it in
the waves, dispatch it like any other unit) or is deferred (attach to the
phase epic it belongs to, or ask the maintainer if none fits). A spec-doc
default that looks wrong is always a `docs` issue, routed the same way —
never a silent doc edit. **Growth guard:** more than three pulled-in issues
beyond the original target size → ask the maintainer before pulling in
more.

## 12. Compose the report

- What opened as a PR (unit → PR URL → stacked-on, if any), and its
  `data-loss-risk` status
- What's blocked and why (external dependency, escalated after 3 FAILs)
- Bundles and why (issue mode)
- What step 11 routed: pulled into this run, deferred, added to an
  existing issue, dropped (with why)
- **`build-integration`'s state**: still present with N unmerged commits
  (normal mid-run), or deleted because every PR from this run merged
  before the run ended (rare but possible for a fast solo review cycle)
- **What's waiting on the maintainer**: every open PR from this run, in
  merge order, with the `data-loss-risk` ones called out first

This is your final message to the user — there is no Discord webhook or
other notification step here; the PR list and this report are the
handoff.

## Non-negotiables

- **Nothing reaches `main` except through a pull request the maintainer merges by hand.** No agent, including you, ever runs `gh pr merge`.
- **No agent ever runs `gh issue close`.** Closing happens via a merged PR's trailer.
- **Every lane clones fresh from `build-integration` (roadmap mode) or the real repo (issue mode)**; parallel lanes never share a scratch clone.
- **Never poll or self-schedule while agents run.**
- **Exactly one `status:*` label per issue**, only via `scripts/issue-status.sh` / `scripts/epic-status.sh`.
- **Never invent an issue number** in a trailer.
- **One commit per issue, always**; a bundle's PR can hold several commits, never a merged diff.
- **A fix round is never bundled; a `data-loss-risk` issue is never bundled.**
- **Only blocking findings fail an issue or reach a fix round**; a fix round's verifier checks closure and the change, not the whole issue afresh.
- **Surfaced findings are filed and routed as they arrive.**
- **Every written artifact uses its template** — dispatch prompts, the executor's report, the verifier's comment.
- **`data-loss-risk` PRs carry a bold callout and are never assumed safe to merge quickly** — that judgment is the maintainer's alone.
