#!/usr/bin/env bash
#
# test-prefs-coverage.sh -- #3446: scripts/prefs-coverage.py's changed_files()
# must return the same list under the developer's diff tooling as in a clean
# environment, and raise GitError (never an empty list) when git fails.
# PREFS_PY overrides the module (to show a case RED on an older copy).
# Run: bash scripts/test-prefs-coverage.sh
# shellcheck disable=SC2015  # A && B || C is report-only
set -euo pipefail
REPO_ROOT=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
PY="${PREFS_PY:-$REPO_ROOT/scripts/prefs-coverage.py}"
# shellcheck source=scripts/lib/git-clean-env.sh
. "$REPO_ROOT/scripts/lib/git-clean-env.sh"
git_clean_env_unset
for v in $(env | sed -n -E 's/^(GIT_CONFIG_(COUNT|PARAMETERS|KEY_[0-9]+|VALUE_[0-9]+))=.*/\1/p'); do unset "$v"; done
export GIT_CONFIG_GLOBAL=/dev/null GIT_CONFIG_SYSTEM=/dev/null
command -v python3 >/dev/null || { echo "  FAIL  python3 not found" >&2; exit 1; }
python3 -c 'import tomllib' 2>/dev/null || { echo "SKIP: python3 lacks tomllib (needs 3.11+; CI still runs this)"; exit 0; }
W=$(mktemp -d); W=$(cd "$W" && pwd -P); trap 'rm -rf "$W"' EXIT
R="$W/repo"; git init -q "$R"; git -C "$R" config user.name T; git -C "$R" config user.email t@localhost
seq 1 30 > "$R/ren.txt"; echo a > "$R/mod.txt"; echo x > "$R/gone.txt"
git -C "$R" add -A; git -C "$R" commit -qm base; BASE=$(git -C "$R" rev-parse HEAD)
git -C "$R" mv ren.txt moved.txt; echo more >> "$R/moved.txt"; echo b >> "$R/mod.txt"; echo new > "$R/new.txt"
printf 'e\n' > "$R/é.txt"; git -C "$R" rm -q gone.txt; git -C "$R" add -A; git -C "$R" commit -qm head
printf '#!/bin/sh\necho side-by-side\n' > "$W/ext.sh"; chmod +x "$W/ext.sh"
printf '%s\n' 'import importlib.util, sys' \
  'spec = importlib.util.spec_from_file_location("pc", sys.argv[1])' \
  'm = importlib.util.module_from_spec(spec); spec.loader.exec_module(m)' \
  'try:' \
  '    entries, base_sha = m.changed_files(sys.argv[2])' \
  '    print("\n".join(sorted(f"{h}<-{b}" for h, b in entries)))' \
  '    print("base=" + str(base_sha))' \
  '    print("modbase=" + repr(m.base_content(base_sha, "mod.txt")))' \
  'except m.GitError:' \
  '    print("GITERROR")' > "$W/probe.py"
rc=0
# list <env...>: the sorted entries, or "GITERROR" when changed_files raises GitError.
list() { (cd "$R" && env "$@" python3 -I "$W/probe.py" "$PY" "$BASE"); }
WANT=$(list X=1)
[ "$(printf '%s\n' "$WANT" | wc -l | tr -d ' ')" = 6 ] \
  && echo "  PASS  clean list has the 4 expected entries (2 added, 1 modified, 1 renamed) and a merge-base" \
  || { echo "  FAIL  clean list wrong: $WANT" >&2; rc=1; }
check() { # <name> <env...>
  local name=$1; shift
  [ "$(list "$@")" = "$WANT" ] && echo "  PASS  same list: $name" || { echo "  FAIL  list changed: $name" >&2; rc=1; }
}
check "GIT_EXTERNAL_DIFF" GIT_EXTERNAL_DIFF="$W/ext.sh"
check "diff.noprefix" GIT_CONFIG_COUNT=1 GIT_CONFIG_KEY_0=diff.noprefix GIT_CONFIG_VALUE_0=true
check "diff.renames=false" GIT_CONFIG_COUNT=1 GIT_CONFIG_KEY_0=diff.renames GIT_CONFIG_VALUE_0=false
check "color.ui=always" GIT_CONFIG_COUNT=1 GIT_CONFIG_KEY_0=color.ui GIT_CONFIG_VALUE_0=always
check "core.quotePath=true" GIT_CONFIG_COUNT=1 GIT_CONFIG_KEY_0=core.quotePath GIT_CONFIG_VALUE_0=true
check "GIT_LITERAL_PATHSPECS" GIT_LITERAL_PATHSPECS=1
check "GIT_DIFF_OPTS" GIT_DIFF_OPTS=-u5
[ "$(list GIT_CONFIG_COUNT=1 GIT_CONFIG_KEY_0=diff.orderFile GIT_CONFIG_VALUE_0="$W/missing")" = GITERROR ] \
  && echo "  PASS  a failing git raises GitError (fails closed)" \
  || { echo "  FAIL  a failing git did not fail closed" >&2; rc=1; }
# A replace ref must not change the list OR lose the merge-base (the worktree matches the real
# HEAD here, so the list alone would coincide; base= is what a replaced HEAD breaks) (mirrors section B2 of test-check-plain-git-diff.sh).
HEADSHA=$(git -C "$R" rev-parse HEAD); git -C "$R" replace -f "$HEADSHA" "$BASE"
# ...and a replaced BASE commit with different content: base_content() must still read the real blob.
FTREE=$(printf '100644 blob %s\tmod.txt\n' "$(echo zzz | git -C "$R" hash-object -w --stdin)" | git -C "$R" mktree)
FAKE=$(git -C "$R" commit-tree -m fake "$FTREE"); git -C "$R" replace -f "$BASE" "$FAKE"
[ -z "$(git -C "$R" diff --name-only "$BASE" HEAD)" ] \
  && echo "  PASS  precondition: the replace ref empties raw git diff" || { echo "  FAIL  replace variant is vacuous" >&2; rc=1; }
check "a replace ref (git replace)" X=1
git -C "$R" replace -d "$HEADSHA" >/dev/null; git -C "$R" replace -d "$BASE" >/dev/null
exit $rc
