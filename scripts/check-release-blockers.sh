#!/bin/bash
# check-release-blockers.sh -- refuse a release while must-fix issues are open (#2905).
# Usage: bash scripts/check-release-blockers.sh [<version>]   (e.g. 1.7.2 or v1.7.2)
#
# The blockers milestone is a STANDING planning bucket (`v<MAJOR>.<MINOR>.x blockers - <theme>`,
# renamed at each minor cut), so it always holds open issues and is never waited on to empty.
# The label `release-blocker` marks the subset that MUST be fixed before tagging: only OPEN
# issues in the matching milestone that carry that label block the release. Other open issues
# in the milestone are ignored. The milestone is matched by title PREFIX (never a number); the
# legacy per-patch form `v<X.Y.Z> Release Blockers` is matched too.
#
# With no argument the target is derived as /push-release step 3 defaults it: a patch bump of
# the latest stable `v*` tag reachable from origin/main (exit 2 if none). This is what lets the
# script run as a `.claude/release.toml` pre_check, which never receives the version.
#
# Exit: 0 nothing labelled is open (or override) | 1 labelled blockers open, listed |
#   2 fail closed: no derivable/unparsable version, no/ambiguous matching milestone, gh failure.
# RELEASE_BLOCKERS_OVERRIDE='<reason>' turns 1 into 0 with a loud warning; an empty or
# whitespace-only value is rejected (exit 2) so the override cannot be a reflex.
set -uo pipefail

REPO="${RELEASE_BLOCKERS_REPO:-sydlexius/stillwater}"
LABEL="release-blocker"
die() { echo "check-release-blockers: $*" >&2; exit 2; }

[ $# -le 1 ] || die "usage: $0 [<version>]"
if [ $# -eq 1 ]; then
  arg="$1"
else
  last=$(git tag --list 'v[0-9]*' --merged origin/main --sort=-v:refname 2>/dev/null \
    | grep -E '^v[0-9]+\.[0-9]+\.[0-9]+$' | head -n 1) || true
  [ -n "$last" ] || die "no version given and no stable v* tag reachable from origin/main"
  IFS=. read -r a b c <<<"${last#v}"
  arg="$a.$b.$((c + 1))"
  echo "check-release-blockers: no version given; derived $arg (patch bump of $last)"
fi
ver="${arg#v}"
[[ "$ver" =~ ^([0-9]+)\.([0-9]+)\.([0-9]+)(-[0-9A-Za-z.-]+)?$ ]] || die "cannot parse version '$arg'"
major="${BASH_REMATCH[1]}"; minor="${BASH_REMATCH[2]}"

if [ "${RELEASE_BLOCKERS_OVERRIDE+set}" = set ] && [ -z "${RELEASE_BLOCKERS_OVERRIDE//[[:space:]]/}" ]; then
  die "RELEASE_BLOCKERS_OVERRIDE is set but empty; state the reason or unset it"
fi

rows=$(gh api "repos/$REPO/milestones?state=all&per_page=100" --paginate \
  --jq '.[] | "\(.number)\t\(.title)"') || die "gh failed listing milestones"
match=$(printf '%s\n' "$rows" | awk -F'\t' -v a="v$major.$minor.x blockers" -v b="v${ver%%-*} Release Blockers" \
  'index($2, a) == 1 || index($2, b) == 1')
[ -n "$match" ] || die "no milestone matches 'v$major.$minor.x blockers*' or 'v${ver%%-*} Release Blockers'; refusing to treat a missing milestone as clean"
[ "$(printf '%s\n' "$match" | wc -l)" -eq 1 ] || die "ambiguous: several milestones match: $(printf '%s' "$match" | tr '\n' ';')"
num="${match%%$'\t'*}"; title="${match#*$'\t'}"

# The issues API also returns pull requests; drop them.
open=$(gh api "repos/$REPO/issues?milestone=$num&state=open&labels=$LABEL&per_page=100" --paginate \
  --jq '.[] | select(has("pull_request") | not) | "  #\(.number) \(.title)"') \
  || die "gh failed listing issues for '$title'"
if [ -z "$open" ]; then
  echo "check-release-blockers: milestone '$title' has no open '$LABEL' issues"
  exit 0
fi

echo "check-release-blockers: milestone '$title' has open '$LABEL' issues:" >&2
printf '%s\n' "$open" >&2
if [ -n "${RELEASE_BLOCKERS_OVERRIDE:-}" ]; then
  echo "WARNING: SHIPPING $ver WITH OPEN BLOCKERS. Override reason: $RELEASE_BLOCKERS_OVERRIDE" >&2
  exit 0
fi
echo "Fix them, drop the label, or set RELEASE_BLOCKERS_OVERRIDE='<reason>' to ship knowingly." >&2
exit 1
