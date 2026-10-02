#!/bin/bash
# test-check-release-blockers.sh -- hermetic tests for check-release-blockers.sh (#2905).
# Run: bash scripts/test-check-release-blockers.sh
# A stub `gh` on PATH records its argv and answers from env vars; nothing touches the network.
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
GUARD="$SCRIPT_DIR/check-release-blockers.sh"
# Strip the inherited git environment before building the fixture repo (#3051).
# shellcheck source=scripts/lib/git-clean-env.sh
. "$SCRIPT_DIR/lib/git-clean-env.sh"
git_clean_env_unset
TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT
mkdir "$TMP/bin"
cat > "$TMP/bin/gh" <<'STUB'
#!/bin/bash
echo "$*" >> "$STUB_LOG"
case "$*" in
  *"/milestones"*) [ -z "${STUB_API_FAIL:-}" ] || exit 1; printf '%b' "${STUB_TITLES:-}" ;;
  *"/issues"*)
    [ -z "${STUB_ISSUE_FAIL:-}" ] || exit 1
    # Honour the label filter the way the real API does.
    case "$*" in *labels=release-blocker*) printf '%b' "${STUB_LABELED:-}" ;; *) printf '%b' "${STUB_ALL:-}" ;; esac ;;
esac
STUB
chmod +x "$TMP/bin/gh"

# Fixture repo for the zero-argument form: tags v1.6.9, v1.7.1, a prerelease, and v1.9.0
# which is NOT on origin/main -- the derivation must pick v1.7.1 -> 1.7.2.
# commit-tree makes unsigned fixture commits without touching signing config.
export GIT_AUTHOR_NAME=t GIT_AUTHOR_EMAIL=t@t GIT_COMMITTER_NAME=t GIT_COMMITTER_EMAIL=t@t
FIX="$TMP/repo"; EMPTY="$TMP/empty"
git init -q "$FIX"; git init -q "$EMPTY"
tree=$(git -C "$FIX" hash-object -t tree -w /dev/null)
c1=$(git -C "$FIX" commit-tree "$tree" -m a)
for t in v1.6.9 v1.7.1 v1.8.0-rc1; do git -C "$FIX" update-ref "refs/tags/$t" "$c1"; done
git -C "$FIX" update-ref refs/remotes/origin/main "$c1"
c2=$(git -C "$FIX" commit-tree "$tree" -p "$c1" -m b)
git -C "$FIX" update-ref refs/tags/v1.9.0 "$c2"

pass=0; fail=0
TITLES='1\tDead Code Deletion\n2\tv1.7.x blockers - Artwork Integrity Hardening\n3\tx v1.7.x blockers - decoy\n'
# expect <version|-> <want-exit> <name> <output must contain> [ENV=val...]   (runs in $FIX)
expect() {
  local ver="$1" want="$2" name="$3" needle="$4"; shift 4
  local out status args=()
  [ "$ver" = - ] || args=("$ver")
  : > "$TMP/gh.log"
  out=$(cd "${CWD:-$FIX}" && env PATH="$TMP/bin:$PATH" STUB_LOG="$TMP/gh.log" STUB_TITLES="$TITLES" "$@" \
    bash "$GUARD" ${args[@]+"${args[@]}"} 2>&1) && status=0 || status=$?
  if [ "$status" -eq "$want" ] && grep -qF -- "$needle" <<<"$out"; then
    pass=$((pass + 1)); echo "PASS: $name"
  else
    fail=$((fail + 1)); echo "FAIL: $name (want exit $want with '$needle', got $status)"
    printf '%s\n' "$out" | sed 's/^/      /'
  fi
}
# logged <name> <regex>: some recorded gh call must match the regex (so a flag is
# checked on the call that needs it, not just anywhere).
logged() {
  if grep -qE -- "$2" "$TMP/gh.log"; then pass=$((pass + 1)); echo "PASS: $1"
  else fail=$((fail + 1)); echo "FAIL: $1 (argv lacks '$2')"; sed 's/^/      /' "$TMP/gh.log"; fi
}

expect 1.7.2 0 "no labelled issues passes" "no open 'release-blocker' issues"
expect 1.7.2 0 "open UNLABELLED issues do not block" "no open 'release-blocker' issues" STUB_ALL='  #7 planning item\n'
expect 1.7.2 1 "labelled blocker fails and is named" "#2894 flow defect" STUB_LABELED='  #2894 flow defect\n' STUB_ALL='  #2894 flow defect\n  #7 other\n'
logged "milestones listed with --paginate" "milestones.* --paginate"
logged "issues listed with --paginate" "issues.* --paginate"
logged "milestones listed with state=all" "milestones\?state=all"
logged "issues filtered by the release-blocker label" "labels=release-blocker"
logged "issues scoped to the matched milestone number (2, not the decoy 3)" "milestone=2&"
expect 1.8.0 2 "missing milestone fails closed" "no milestone matches"
expect 1.7.2 2 "gh milestone-list error fails closed" "gh failed" STUB_API_FAIL=1
expect 1.7.2 2 "gh issue-list error fails closed" "gh failed" STUB_ISSUE_FAIL=1
expect 1.7.2 0 "override with reason passes loudly" "SHIPPING 1.7.2 WITH OPEN BLOCKERS" \
  STUB_LABELED='  #1 x\n' RELEASE_BLOCKERS_OVERRIDE="known flow defect"
expect 1.7.2 2 "empty override is rejected" "set but empty" STUB_LABELED='  #1 x\n' RELEASE_BLOCKERS_OVERRIDE=""
expect banana 2 "unparsable version fails closed" "cannot parse"
expect v1.7.2 0 "leading v accepted" "no open"
expect - 0 "no argument derives 1.7.2 from v1.7.1 on origin/main" "derived 1.7.2 (patch bump of v1.7.1)"
expect - 1 "derived version still enforces blockers" "#9 x" STUB_LABELED='  #9 x\n'
CWD=$EMPTY expect - 2 "no derivable version fails closed" "no stable v* tag"
# Prefix match must be anchored: with ONLY the decoy title present nothing matches.
TITLES='3\tx v1.7.x blockers - decoy\n' expect 1.7.2 2 "decoy title with the prefix mid-string is not matched" "no milestone matches"
TITLES='2\tv1.7.x blockers - a\n3\tv1.7.x blockers - b\n' expect 1.7.2 2 "ambiguous milestones fail closed" "ambiguous"

echo "$pass passed, $fail failed"
[ "$fail" -eq 0 ]
