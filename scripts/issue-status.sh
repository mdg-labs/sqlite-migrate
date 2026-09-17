#!/usr/bin/env bash
# Set an issue's single status:* label.
#
# The invariant this exists to hold: an issue carries exactly one status:*
# label at a time. Doing that as `gh issue edit --add-label X --remove-label Y`
# is two API calls with a window between them where the issue has two status
# labels or none, and it needs the caller to already know which one to remove.
# This replaces the whole label set in one PATCH instead: non-status labels are
# preserved, every status:* is dropped, the requested one is added.
#
# Callers are agents running inside scratch clones, whose `origin` is a local
# path — `gh` cannot infer the repo there, so it is always passed explicitly.
set -euo pipefail

REPO=${GH_REPO:-mdg-labs/sqlite-migrate}
STATUSES=(new ready in-progress in-review implemented closed cancelled)
RETRIES=${ISSUE_STATUS_RETRIES:-4}

die() { printf '%s\n' "$*" >&2; exit 1; }

# A build run has several agents calling this at once, so a transient 5xx or a
# secondary rate limit is routine rather than exceptional. Retry with backoff
# and let the caller see a hard failure only once it is really stuck. stdout is
# captured and replayed on success; stderr is left to flow so gh's own
# diagnostics reach the journal.
gh_retry() {
  local attempt=1 delay=2 out rc
  while :; do
    # rc must be captured in the else branch, not after fi: `$?` there is the
    # status of the *if statement*, and an if whose branch didn't run is 0 —
    # so reading it after fi reports success for a call that just failed.
    if out=$(gh "$@"); then
      printf '%s\n' "$out"
      return 0
    else
      rc=$?
    fi
    (( attempt >= RETRIES )) && return "$rc"
    printf 'issue-status: gh %s failed (attempt %d/%d), retrying in %ds\n' \
      "${1:-?}" "$attempt" "$RETRIES" "$delay" >&2
    sleep "$delay"
    delay=$(( delay * 2 ))
    attempt=$(( attempt + 1 ))
  done
}

[[ $# -eq 2 ]] || die "usage: issue-status.sh <issue-number> <status>
statuses: ${STATUSES[*]} (with or without the 'status:' prefix)"

issue=$1
status=${2#status:}

[[ $issue =~ ^[0-9]+$ ]] || die "issue must be a number, got '$issue'"
printf '%s\n' "${STATUSES[@]}" | grep -qx -- "$status" \
  || die "unknown status '$status' — want one of: ${STATUSES[*]}"

# The read below decides which labels SURVIVE the PATCH, so a failed read is
# not a missing status — it is silent data loss. Read into a checked
# assignment rather than `mapfile < <(...)`: a process substitution's exit
# status is not propagated and `set -e` cannot see it, so a transient gh
# failure used to yield an empty `keep` and the PATCH then replaced the whole
# label set with just `status:$status`, dropping the issue's type and area:*
# labels. Nothing reported an error, because as far as bash was concerned
# nothing had failed.
keep_raw=$(gh_retry issue view "$issue" --repo "$REPO" --json labels \
  --jq '.labels[].name | select(startswith("status:") | not)') \
  || die "could not read #$issue's labels ($RETRIES attempts) — refusing to PATCH, because writing now would drop every non-status label on the issue"

# `mapfile <<< ""` yields one empty element, not none — which would send a
# bogus empty label. An issue whose only label is its status is normal.
if [[ -n $keep_raw ]]; then mapfile -t keep <<<"$keep_raw"; else keep=(); fi

args=(--method PATCH "repos/$REPO/issues/$issue" -f "labels[]=status:$status")
for label in ${keep[@]+"${keep[@]}"}; do
  args+=(-f "labels[]=$label")
done

final=$(gh_retry api "${args[@]}" --jq '[.labels[].name] | join(",")') \
  || die "could not set #$issue to status:$status ($RETRIES attempts)"

# Confirm from the PATCH's own response rather than assuming a 200 means the
# invariant holds — this is the only place that can still catch a concurrent
# writer having raced us.
got=$(printf '%s' "$final" | tr ',' '\n' | grep '^status:' || true)
[[ $got == "status:$status" ]] \
  || die "#$issue: expected exactly 'status:$status' after the write, got '${got:-none}'"

printf '#%s %s\n' "$issue" "$got"
