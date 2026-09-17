---
name: triage
description: Enrich an existing GitHub issue or draft new ones from a raw report (including seeding a roadmap phase as an epic with sub-issues), using `gh` CLI only. Use when the user gives an issue number to clean up/enrich, or a raw bug/feature/spike report to turn into a well-structured issue. Never edits local files — read-only against the repo, all writes go through `gh issue edit`/`gh issue create`/`scripts/issue-status.sh`.
argument-hint: <issue-number> | <free-form report text>
allowed-tools:
  - Read
  - Grep
  - Glob
  - AskUserQuestion
  - Bash(gh issue view *)
  - Bash(gh issue list *)
  - Bash(gh issue edit *)
  - Bash(gh issue create *)
  - Bash(gh pr view *)
  - Bash(gh pr list *)
  - Bash(gh repo view *)
  - Bash(gh label list *)
  - Bash(gh api *)
  - Bash(git log *)
  - Bash(git show *)
  - Bash(git blame *)
  - Bash(git diff *)
  - Bash(git status)
  - Bash(git grep *)
  - Bash(git clone *)
  - Bash(scripts/issue-status.sh *)
---

# triage

Turns a rough issue into a well-structured one — either by enriching an
existing GitHub issue on `mdg-labs/sqlite-migrate` or drafting new ones from
a raw report — using the `gh` CLI for every GitHub-side effect.

**Hard constraint: this skill is read-only against the local repository.**
It never uses `Edit`, `Write`, or `NotebookEdit`, and never runs a `git`
command that mutates this repo's tracked content or state (no `commit`,
`add`, `push`, `branch`, `checkout`, `reset`, …) or a `gh` command that
touches anything but issues. The one exception is `git clone --depth 1` of
an external reference repo into `../reference/<name>`, *outside* this repo
(see "Consult external context"). Temp files for `--body-file` go in the
scratchpad directory. If a task seems to require touching code or docs,
stop and say so — that's the `build` skill's job, not this skill's.

## Determine the mode

Look at `$ARGUMENTS` (or the user's message):

- **A bare issue number, or "issue #N" / "issue N"** → **Enrich mode** on that issue.
- **Anything else** (free-form bug/feature/spike report, or "seed Phase N") → **Create mode** from that text.
- **Nothing usable in either shape** → ask once via `AskUserQuestion`: enrich an existing issue (get the number) or create from a report (get the text).

## Investigate (both modes)

This project is design-first: the spec doc is the ground truth for what
should exist and why. Ground every issue in it.

1. `gh repo view mdg-labs/sqlite-migrate --json nameWithOwner,defaultBranchRef` to confirm the target.
2. **Read the spec doc section the report touches** — `docs/internal/sqlite-migrate-spec-mvp-doc.md`. Note the exact section (`Core Design Principles §3`, `Safety Model`, the Development Roadmap phase) the issue implements or changes.
3. **Check the doc's own decisions before proposing a new one.** The spec doc's "Decisions on the Remaining Risks" and "Next Steps" sections are its decision log — a report that conflicts with a recorded decision there doesn't quietly reopen it; say so in `## Constraints`. A genuine new reason to reopen one becomes its own `docs` issue that updates the spec doc as part of its acceptance criteria.
4. Grep/Read any code, scripts or workflows the report mentions; `git log`/`git blame`/`git show` for recent history on them.
5. `gh issue list --repo mdg-labs/sqlite-migrate --state all --search ...` for related or duplicate issues.

Keep this proportional — a typo needs none of it; a classifier or rebuild-generator bug needs all of it.

## Consult external context (when relevant)

This project wraps sqldef's diff engine for the cases it already handles
correctly (Core Design Principle 4/5) rather than reimplementing them, so
upstream sqldef behavior is often the real answer for an `area:sqldefwrap`
report. Only reach for a source the issue actually touches; clone into
`../reference/<name>` with `git clone --depth 1 <url>` if missing, never
speculatively:

- **sqldef** (`github.com/sqldef/sqldef`) — idempotent-DDL generation semantics for `CREATE TABLE`/`ADD COLUMN`, what `schema.GenerateIdempotentDDLs` does and doesn't handle.
- **modernc.org/sqlite** (`gitlab.com/cznic/sqlite`, mirrored at `github.com/modernc-org/sqlite`) — driver-level behavior when a report suspects the driver rather than this project's own code.
- **SQLite itself** (`sqlite.org/lang_altertable.html`, `sqlite.org/stricttables.html`) — when a report claims SQLite's own `ALTER TABLE`/`STRICT` semantics differ from what the spec doc assumes.

Read for **behavior and intent**, never to transcribe code. When any of
these applies, the body gains a short `## Upstream / reference context`
section with what was found, at which version or commit.

## Open questions get a recommended default, not a stalled thread

Never leave an `## Open questions` item as a bare question. Investigate,
then commit to a recommended default: the question, the default, and a
one-line rationale. Where the spec doc already has a decision, cite the
section instead of re-deriving it. The issue still surfaces the question to
override later, but triage always lands on a concrete, sane default.
`AskUserQuestion` is available; prefer the default rule over asking.

## Fewer, complete issues

Every issue triage creates is work someone must finish. Keep the count down
and each issue whole:

- **Extend before you add.** If step 5 finds an open issue that covers the
  same work or paths and hasn't been started (`status:new` or
  `status:ready`), add the new material to that issue (enrich mode: a new
  acceptance criterion, a scope line) instead of creating another. Report
  which issue absorbed it.
- **A decision and its code are one issue.** When an open question's
  default implies code, the issue that builds the code records the default
  and updates the spec doc as one of its acceptance criteria. Create a
  standalone `docs` issue only when no code follows, or when the maintainer
  must decide before the code can be specified. In that case, create the
  code issue together with it, blocked by it; `build` pulls it into the
  same run.
- **Acceptance criteria are the whole definition of done.** A verifier
  fails an issue only on an unmet criterion or a real defect. So write
  criteria that state the bar ("refuses X; Y stays allowed"), citing the
  relevant testdata scenario number where one exists (spec doc, Testing &
  Verification Strategy), and list adjacent work the issue deliberately
  leaves alone under `## Out of scope`, with the issue number where that
  work lives, if any.
- **Every non-epic issue has a home.** Attach it to the open epic whose
  scope it falls under (usually the roadmap phase it belongs to). When
  `build` files a finding, it tells you whether the issue joins its current
  run or is deferred to a named phase epic. If no open epic fits, say so in
  your report rather than guessing.

This skill is **invoke-only** — no workflow triggers it. It runs here when
the maintainer wants it.

## Labels

Every issue gets, per `CLAUDE.md` ("Label set"):

- **Exactly one type**: `feat`, `bug`, `chore`, `docs`, `spike`
- **One `area:*`** where one applies (see `CLAUDE.md`'s area → paths table)
- **Extras when true:**
  - `epic` on an epic (a roadmap phase, or any tracking issue with sub-issues)
  - `data-loss-risk` when the work touches the destructive-vs-safe classifier, the rebuild generator, the `VACUUM INTO` backup/restore path, checksum verification, or the drift check — anywhere a plausible bug silently loses or corrupts a user's database
  - `blocked` only for an external blocker that isn't expressible as a native blocked-by relationship

There is no `needs-sudo` label and no hardware tier — nothing in this
project needs root or touches real infrastructure; acceptance criteria for
even `data-loss-risk` work are provable against on-disk SQLite files in a
scratch clone (spec doc, Phase 5's "Done when").

`gh label list --repo mdg-labs/sqlite-migrate` shows what exists. Never
apply a `status:*` label directly — see below.

## Issue body shape

- `## Original report` — the reporter's text, verbatim (enrich mode: the existing body and relevant comments; create mode: the user's text).
- `## Summary` — one or two sentences on what this actually is, once investigated.
- `## Spec references` — the spec doc section(s) it implements or touches (e.g. "Core Design Principle 3", "Phase 2 — Rebuild generator", "testdata scenario 07").
- Then, as warranted: `## Reproduction`, `## Root cause / relevant code`, `## Upstream / reference context`, `## Proposed approach`, `## Constraints`, `## Acceptance criteria`, `## Out of scope`, `## Open questions`. Don't force sections that don't apply — except `## Out of scope`, which every `feat`, `bug` and `chore` issue carries.
- **Acceptance criteria are checkable and complete** — nothing beyond them is required to close the issue. Cite the exact testdata scenario(s) it must pass where the spec doc's matrix has one; for `data-loss-risk` work, name the on-disk-file test that proves no data was lost. For a `spike`, the deliverable is recorded findings (`CLAUDE.md`, "Spikes"): what is measured, the pass/fail criterion, and which spec doc section it confirms or overturns.
- **Scope hint.** Name the top-level paths the work will touch in backticks (`internal/rebuild/`, `docs/internal/`), so `build` can bound it.

## Epic/sub-issue structure and dependencies — native relationships, never body prose

When triage produces more than one issue — an epic with sub-issues, or an
issue that depends on another tracked issue — wire the relationship through
GitHub's native fields. **Never** as body prose ("Part of #N", "Depends on
#N"): that is a second, driftable copy of a fact GitHub already tracks.

- **Epic → sub-issue:** `gh issue edit <epic> --repo mdg-labs/sqlite-migrate --add-sub-issue <n>` (or `--parent <epic>` on the child). Verify with `gh issue view <epic> --json subIssuesSummary`.
- **Blocking dependency:** `gh issue edit <n> --repo mdg-labs/sqlite-migrate --add-blocked-by <dep>`. Verify with `gh issue view <n> --json blockedBy,blocking`.
- Needs `gh` ≥ 2.100.
- The body may *explain* why an ordering exists; it is never the only record that it does.
- These calls happen **after** every issue in the relationship exists — create first, link second.

## Mode 1 — Enrich an existing issue

1. `gh issue view <n> --repo mdg-labs/sqlite-migrate --json number,title,body,labels,comments,url,state,assignees` and `gh issue view <n> --repo mdg-labs/sqlite-migrate --comments`.
2. Investigate as above, starting from the body and comments (comments override the body where they disagree).
3. Rewrite title and body in the shape above. If this is really an epic, or depends on / blocks another issue, decide that now; wire it in 4a.
4. `gh issue edit <n> --repo mdg-labs/sqlite-migrate --title "..." --body-file <tmpfile>`, plus `--add-label`/`--remove-label` for type, area and extras (never `status:*`).
   - **4a.** Wire any epic/sub-issue or blocking relationship via the native flags, once every issue involved exists.
5. `scripts/issue-status.sh <n> ready` — an enriched issue can be picked up. Skip for a closed issue.
6. Report the issue URL and a short summary of what was added, including any spec doc defaults applied or challenged.

## Mode 2 — Create issues from a report

1. Investigate as above, starting from the raw report.
2. Draft title + body in the shape above. If the report is more than one piece of work, decide the epic/sub-issue split here.
   - **Seeding a roadmap phase** ("seed Phase 5"): create the phase's epic first (title `Phase N — <deliverable>`, body quoting that phase's "Deliverable"/"Done when" from the spec doc verbatim), then each sub-issue for the distinct pieces of work inside it, then wire parent and blocked-by natively — including a `blocked-by` edge to the epic of the previous phase, since the roadmap is sequential (spec doc: "an agent building this should not move to the next phase until the current one's acceptance criteria pass"). The mass-creation guard below applies.
   - **Mass-creation guard:** if this would create more than ~12 issues, state the count and list the titles, and confirm via `AskUserQuestion` before creating anything.
3. `gh issue create --repo mdg-labs/sqlite-migrate --title "..." --body-file <tmpfile> --label ...` — epic first, then each sub-issue, so every number a relationship needs exists.
4. Wire relationships via the native flags. If a body referenced another issue's number before it existed, patch it in with `gh issue edit --body-file` now — no placeholders left behind.
5. `scripts/issue-status.sh <number> ready` for every issue created. The `issue-status` workflow stamps new issues `status:new`; one this skill created was enriched at birth.
6. Report every new issue's URL.

## Non-negotiables

- Original report text is never paraphrased away — it is preserved verbatim in its own section.
- No local file in this repo is created, modified, or deleted. Temp files go in the scratchpad directory.
- No git commits, branches, stashes, or pushes.
- Every GitHub-side write goes through `gh issue edit`, `gh issue create`, or `scripts/issue-status.sh`, always with `--repo mdg-labs/sqlite-migrate` where `gh` takes it.
- **Exactly one `status:*` label per issue, always**, set only by `scripts/issue-status.sh`.
- **Epic/sub-issue and blocking relationships are native GitHub fields, never body prose.**
- **Every open question lands on a recommended default**, citing the spec doc section where one exists.
- **An issue never silently contradicts a spec doc decision** — it names the conflict and, for a default, includes updating the spec doc in its acceptance criteria.
- **Extend an open, unstarted issue before creating a new one; never split a decision from the code it implies** unless the maintainer must decide first, and then file both together.
- **Every non-epic issue gets an open epic**, and every `feat`/`bug`/`chore` issue gets an `## Out of scope` section.
