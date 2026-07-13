#!/usr/bin/env bash
# check-action-pins.sh -- assert that every sub-action of a single GitHub Action
# repository is pinned to the SAME commit SHA.
#
# The sub-actions of one action repo (e.g. github/codeql-action/init,
# /analyze, /upload-sarif) are shipped from that ONE repo and therefore share a
# commit SHA. Dependabot, however, names each subpath as its OWN dependency, so
# it will happily bump one and leave the others behind.
#
# That is not hypothetical. In #2490, Dependabot bumped codeql-action/init
# (#2484) and /upload-sarif (#2486) to v4.36.3 and never opened a PR for
# /analyze, which stayed on v4.36.2. `init` writes a config file stamped with
# its own version, and the older `analyze` then refuses to load it:
#
#   Loaded a configuration file for version '4.36.3', but running version '4.36.2'
#
# Both CodeQL jobs failed on every PR until the pins were realigned.
#
# .github/dependabot.yml now GROUPS these families so Dependabot moves them as a
# unit, which prevents that specific mechanism. But a group is a POLICY: it does
# not ASSERT the invariant, and it cannot catch a skew introduced by hand, by a
# bad merge, or in a family nobody remembered to group. This script asserts it.
#
# The check is deliberately GENERIC -- it groups by owner/repo across every
# SHA-pinned `uses:` in the workflows, rather than hard-coding the families that
# happen to use subpaths today (currently github/codeql-action and
# actions/cache). A family added tomorrow is covered the day it lands.
#
# Usage:
#   check-action-pins.sh    verify (the only mode)
#
# There is deliberately no --fix: when two pins of one family disagree, there is
# no way to know which SHA is the intended one. That is a human decision.
#
# Exit: 0 = every action repo resolves to exactly one SHA,
#       1 = drift detected, or no pins were found at all (a setup error --
#           a workflow tree with zero SHA-pinned actions means this check is
#           silently inspecting nothing, which must not read as a pass).

set -euo pipefail

cd "$(git rev-parse --show-toplevel 2>/dev/null || pwd)"

WORKFLOW_DIR=".github/workflows"

if [ ! -d "$WORKFLOW_DIR" ]; then
  echo "check-action-pins: $WORKFLOW_DIR not found" >&2
  exit 1
fi

# Collect every SHA-pinned remote action reference as: owner/repo <sha> <file:line>
#
# The 40-hex anchor is what keeps local composite references (`uses: ./.github/
# actions/setup-tailwind`) and floating tags out: neither can match it. Grouping
# strips the subpath, so `github/codeql-action/init` and `github/codeql-action/
# analyze` both collapse to the key `github/codeql-action`.
pins=$(
  grep -rnoE "uses:[[:space:]]*[A-Za-z0-9_.-]+/[A-Za-z0-9_./-]+@[0-9a-f]{40}" \
    --include='*.yml' --include='*.yaml' "$WORKFLOW_DIR" 2>/dev/null \
  | sed -E 's#^([^:]+):([0-9]+):uses:[[:space:]]*([^@]+)@([0-9a-f]{40})$#\3 \4 \1:\2#' \
  | awk '{
      split($1, parts, "/")
      print parts[1] "/" parts[2], $2, $3
    }' \
  || true
)

if [ -z "$pins" ]; then
  echo "check-action-pins: found no SHA-pinned actions under $WORKFLOW_DIR" >&2
  echo "check-action-pins: this check would be inspecting nothing -- treating as a setup error, not a pass" >&2
  exit 1
fi

# Group by owner/repo; a family with more than one distinct SHA is drift.
# awk does the grouping (no `declare -A`, matching coverage-floor.sh's pipeline).
report=$(
  printf '%s\n' "$pins" | awk '
    {
      repo = $1; sha = $2; loc = $3
      if (!((repo SUBSEP sha) in seen_sha)) {
        seen_sha[repo SUBSEP sha] = 1
        nsha[repo]++
        shas[repo] = shas[repo] " " sha
      }
      locs[repo] = locs[repo] "\n      " loc "  " substr(sha, 1, 10)
    }
    END {
      bad = 0
      for (repo in nsha) {
        if (nsha[repo] > 1) {
          bad++
          printf "FAIL: %s is pinned to %d different SHAs:%s\n", repo, nsha[repo], shas[repo]
          printf "    pins:%s\n", locs[repo]
        }
      }
      exit (bad > 0 ? 1 : 0)
    }
  '
) && rc=0 || rc=$?

families=$(printf '%s\n' "$pins" | awk '{print $1}' | sort -u | wc -l | tr -d ' ')
total=$(printf '%s\n' "$pins" | wc -l | tr -d ' ')

if [ "$rc" -ne 0 ]; then
  printf '%s\n' "$report"
  echo ""
  echo "check-action-pins: FAIL -- sub-actions of one action repo must share a commit SHA."
  echo "They ship from the same repository, and a version-mismatched pair breaks at runtime"
  echo "(see #2490). Align every pin in the family above onto one SHA."
  exit 1
fi

echo "OK: $total SHA-pinned action references across $families action repos; every repo resolves to a single SHA."
