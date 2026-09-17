## Execution report — {{UNIT_ID}}

**Workspace:** `{{WORKSPACE_PATH}}`
**Items in this dispatch:** {{#<n>, #<n>, … or "Phase <n>" in the order you worked them}}

{{FOR EACH ISSUE — one block per item in this dispatch, including any you
had to report blocked:}}

### #{{ISSUE_NUMBER}} — {{ISSUE_TITLE}}

**Status:** {{done|blocked}}
**Commit:** `{{SHA}}` {{or "none — blocked"}}

**Files touched:**
- {{path}}

**Summary:** {{2-5 sentences: what was implemented and how}}

**Checks run:** {{every check actually run inside WORKSPACE for this item,
with its result — and every applicable check that could NOT run on this
machine, with why (not installed)}}

**Generated output changed:** {{testdata/golden/ changes and why — or "none"}}

**Destructive/data-safety code paths touched:** {{for schemadiff/rebuild/
runtime work: each classification, rebuild, backup, or drift-check path
changed, and the test covering it — or "none"}}

**Spec doc references:** {{sections implemented; any spec doc decision this
relies on; any spec doc statement you believe is wrong, and why — or "none"}}

**Deviations from the item text:** {{where and why — or "none"}}

**Left undone / blocked:** {{what and why — or "none"}}

{{END FOR}}

### Findings outside these items
{{only real problems none of the items above cover and you did not fix: a
pre-existing defect with a concrete scenario, a doc or spec doc statement
that is untrue, or work a planned feature cannot do without. One line each —
what, where (`file:line` or the command that shows it), the scenario, and
why it isn't yours. Not findings: style, hardening you would like, ideas,
and tooling missing on this machine (the orchestrator already knows). The
orchestrator files or routes each one; you never open an issue. "none" is
the normal answer.}}
