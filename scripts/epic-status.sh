#!/usr/bin/env bash
# Roll an epic's status up from its sub-issues.
#
# Called by an executor after it claims a sub-issue, and by a verifier after it
# passes one. Both need the epic to follow along: the epic goes in-progress the
# moment the first sub-issue is picked up, and implemented once the last one
# passes verification.
#
# It is computed, not incremental, and that is the point. Parallel waves mean
# "am I the first?" and "am I the last?" are races an agent cannot answer
# about itself — but the epic's status is a pure function of its sub-issues'
# statuses, so every agent that asks gets the same answer no matter what order
# they ask in, and asking twice is harmless.
#
#   any sub-issue being worked on   → status:in-progress
#   every sub-issue done            → status:implemented
#   nothing started yet             → leave the epic alone
#
# It never sets closed/cancelled — .github/workflows/issue-status.yml owns
# those, when the epic's own last trailer closes it.
set -euo pipefail

REPO=${GH_REPO:-mdg-labs/sqlite-migrate}
HERE=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)

die() { printf '%s\n' "$*" >&2; exit 1; }

[[ $# -eq 1 ]] || die "usage: epic-status.sh <epic-issue-number>"
epic=$1
[[ $epic =~ ^[0-9]+$ ]] || die "epic must be a number, got '$epic'"

# "<state> <status label or ->" per sub-issue. State is downcased because this
# REST endpoint spells it "open"/"closed" while `gh issue view --json state`
# spells the same thing "OPEN"/"CLOSED" — a comparison written against the
# wrong one of those matches nothing and fails silently.
mapfile -t subs < <(
  gh api "repos/$REPO/issues/$epic/sub_issues" --paginate \
    --jq '.[] | "\(.state | ascii_downcase) \([.labels[].name | select(startswith("status:"))] | first // "-")"'
)

if [[ ${#subs[@]} -eq 0 ]]; then
  echo "#$epic has no sub-issues — nothing to roll up"
  exit 0
fi

done_count=0
active=0
for sub in "${subs[@]}"; do
  state=${sub%% *}
  status=${sub##* }
  if [[ $state == closed || $status == status:implemented ]]; then
    (( ++done_count ))
  elif [[ $status == status:in-progress || $status == status:in-review ]]; then
    (( ++active ))
  fi
done

if (( done_count == ${#subs[@]} )); then
  want=implemented
elif (( active > 0 || done_count > 0 )); then
  want=in-progress
else
  echo "#$epic: no sub-issue started yet — leaving its status alone"
  exit 0
fi

"$HERE/issue-status.sh" "$epic" "$want"
