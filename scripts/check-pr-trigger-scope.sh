#!/bin/bash
# check-pr-trigger-scope.sh -- assert workflows run on pull requests to ANY base
# branch (#3002).
#
# THE FAILURE: with `pull_request: branches: [main]`, a stacked PR (base is
# another feature branch) runs none of the required checks. The merge oracle
# reads the empty rollup as green, so an untested PR looks mergeable.
#
# WHAT IT CHECKS (default-deny, so a new workflow is covered automatically):
#   1. EVERY workflow (*.yml and *.yaml, both load in Actions) in the dir with a 2-space-indented `pull_request:` key
#      must carry no `branches:` / `branches-ignore:` under it (block form) and
#      none on the key line (flow form `pull_request: {branches: [main]}`),
#      unless the file is on EXEMPT below. `pull_request_target` is a different
#      key and out of scope. Only the 2-space layout is parsed (repo standard).
#   2. MUST_TRIGGER files (the ones producing required `Protect main` contexts)
#      must still HAVE a `pull_request:` trigger; losing it means no check runs.
#
# Exit: 0 = ok, 1 = violation, 2 = setup error.
# Usage: bash scripts/check-pr-trigger-scope.sh [workflow-dir]
set -euo pipefail

DIR="${1:-$(cd "$(dirname "$0")/.." && pwd)/.github/workflows}"
MUST_TRIGGER="ci.yml bruno-ci.yml security.yml codeql.yml gate.yml signed-commits.yml"
# EXEMPT: files allowed to restrict the base branch, one reason each.
#   dependabot-auto-approve.yml -- approval path; must only act on PRs to main.
#   pages.yml -- docs-site build; its checks are not in the ruleset.
EXEMPT=" dependabot-auto-approve.yml pages.yml "

for w in $MUST_TRIGGER; do
  if [ ! -f "$DIR/$w" ]; then
    echo "FAIL: workflow not found: $DIR/$w" >&2
    exit 2
  fi
done

status=0
# GitHub loads both .yml and .yaml; scanning only one would let the other escape.
# An unmatched glob stays literal in bash, so skip non-files.
for f in "$DIR"/*.yml "$DIR"/*.yaml; do
  [ -f "$f" ] || continue
  w=$(basename "$f")
  if ! grep -qE '^  pull_request:' "$f"; then
    case " $MUST_TRIGGER " in
      *" $w "*)
        echo "FAIL: $w has no pull_request trigger but produces a required check (#3002)"
        status=1 ;;
    esac
    continue
  fi
  case "$EXEMPT" in *" $w "*) continue ;; esac
  bad=$(awk '
    /^  pull_request:/ { inpr = 1; if ($0 ~ /branches/) print NR ": " $0; next }
    /^  [A-Za-z_]/ || /^[A-Za-z]/ { inpr = 0 }
    inpr && /^[[:space:]]+branches(-ignore)?:/ { print NR ": " $0 }
  ' "$f")
  if [ -n "$bad" ]; then
    echo "FAIL: $w restricts pull_request to specific base branches (#3002):"
    printf '%s\n' "$bad" | sed 's/^/  /'
    status=1
  fi
done

if [ "$status" -ne 0 ]; then
  echo "  HINT: remove the branches filter (or add a justified EXEMPT entry); a stacked PR would run no required checks."
  exit 1
fi
echo "OK: pull_request workflows run on any base branch (except EXEMPT)"
