#!/usr/bin/env bash
#
# test-check-plain-git-diff.sh -- tests for #3446: the gate's parsed `git diff`
# checks must not be changeable by the developer's diff tooling.
#   A. the REAL raw-error-leak arm of pre-push-gate.sh (cut out by markers, not
#      reimplemented) must report the leak under every tooling variant.
#   B. the REAL changed-line extraction of stylelint-diff-gate.sh (the loop that
#      turns `git diff` into file:line; stylelint itself is not needed for it).
#   C. check-plain-git-diff.sh rejects a raw parsed diff and accepts the rest.
# GATE_SH / STYLELINT_SH override the scripts under test (used to show each case
# RED against main's copy). Run: bash scripts/test-check-plain-git-diff.sh
# shellcheck disable=SC2016,SC2015  # single-quoted fixtures; A && B || C is report-only
set -euo pipefail
REPO_ROOT=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
GATE_SH="${GATE_SH:-$REPO_ROOT/scripts/pre-push-gate.sh}"
STYLELINT_SH="${STYLELINT_SH:-$REPO_ROOT/scripts/stylelint-diff-gate.sh}"
# shellcheck source=scripts/lib/git-clean-env.sh
. "$REPO_ROOT/scripts/lib/git-clean-env.sh"
git_clean_env_unset
export GIT_CONFIG_GLOBAL=/dev/null GIT_CONFIG_SYSTEM=/dev/null
W=$(mktemp -d); W=$(cd "$W" && pwd -P); trap 'rm -rf "$W"' EXIT
rc=0
pass() { echo "  PASS  $1"; }
fail() { echo "  FAIL  $1" >&2; rc=1; }

# Fixture: base commit, then a head commit that leaks err.Error() in a handler,
# edits two CSS lines, and adds a non-ASCII-named CSS file.
R="$W/repo"; git init -q "$R"
git -C "$R" config user.name T; git -C "$R" config user.email t@localhost
mkdir -p "$R/internal/api" "$R/web/static/css"
printf 'package api\n' > "$R/internal/api/handlers_x.go"
printf 'a {\n  color: red;\n}\n' > "$R/web/static/css/x.css"
printf '*.go diff=sw\n' > "$R/.gitattributes"
git -C "$R" add -A; git -C "$R" commit -qm base; BASE=$(git -C "$R" rev-parse HEAD)
printf 'package api\nfunc h() { w.Write(err.Error()) }\n' > "$R/internal/api/handlers_x.go"
printf 'a {\n  color: blue;\n  margin: 0;\n}\n' > "$R/web/static/css/x.css"
printf 'b {}\n' > "$R/web/static/css/é.css"
git -C "$R" add -A; git -C "$R" commit -qm head
printf '#!/bin/sh\necho "side-by-side $1"\n' > "$W/ext.sh"
printf '#!/bin/sh\necho converted\n' > "$W/tc.sh"; chmod +x "$W/ext.sh" "$W/tc.sh"

# Each variant is env assignments separated by US (0x1f), run via `env`. Config
# goes through GIT_CONFIG_COUNT so no developer file is needed.
US=$'\037'
cfg() { printf 'GIT_CONFIG_COUNT=%s' "$(($# / 2))"; local i=0
        while [ $# -gt 0 ]; do printf '%sGIT_CONFIG_KEY_%s=%s%sGIT_CONFIG_VALUE_%s=%s' "$US" "$i" "$1" "$US" "$i" "$2"; shift 2; i=$((i+1)); done; }
VARIANTS=(
  "clean|X=1"
  "GIT_EXTERNAL_DIFF|GIT_EXTERNAL_DIFF=$W/ext.sh"
  "diff.external via GIT_CONFIG_COUNT|$(cfg diff.external "$W/ext.sh")"
  "diff.external via GIT_CONFIG_PARAMETERS|GIT_CONFIG_PARAMETERS='diff.external'='$W/ext.sh'"
  "color.ui=always|$(cfg color.ui always)"
  "color.diff=always|$(cfg color.diff always)"
  "textconv driver|$(cfg diff.sw.textconv "$W/tc.sh")"
  "diff.noprefix|$(cfg diff.noprefix true)"
  "diff.mnemonicPrefix|$(cfg diff.mnemonicPrefix true)"
  "GIT_DIFF_OPTS=-u5|GIT_DIFF_OPTS=-u5"
)

echo "A. raw-error-leak arm detects the leak under every variant"
sed -n '/^error_leaks=\$(/,/^echo "OK"$/p' "$GATE_SH" > "$W/leak.sh"
grep -q 'exit 1' "$W/leak.sh" || { echo "FAIL: leak arm markers not found in $GATE_SH" >&2; exit 1; }
for v in "${VARIANTS[@]}"; do
  name=${v%%|*}; envs=${v#*|}
  IFS=$US read -ra E <<<"$envs"
  out=$(cd "$R" && env "${E[@]}" bash -c '. "$1"; BASE=$2; . "$3"' _ "$REPO_ROOT/scripts/lib/git-plain.sh" "$BASE" "$W/leak.sh" 2>&1) && s=0 || s=$?
  if [ "$s" -eq 1 ] && grep -q 'CRITICAL' <<<"$out"; then pass "leak detected: $name"
  else fail "LEAK MISSED (exit $s): $name"; fi
done

echo "B. stylelint changed-line extraction is the same under every variant"
sed -n '/^current_file=""$/,/^done < <(/p' "$STYLELINT_SH" > "$W/lines.sh"
grep -q '^done < <(' "$W/lines.sh" || { echo "FAIL: extraction markers not found in $STYLELINT_SH" >&2; exit 1; }
WANT=$(printf 'web/static/css/x.css:2\nweb/static/css/x.css:3\nweb/static/css/é.css:1')
for v in "${VARIANTS[@]}"; do
  name=${v%%|*}; envs=${v#*|}; : > "$W/added.txt"
  IFS=$US read -ra E <<<"$envs"
  (cd "$R" && env "${E[@]}" bash -c '. "$1"; BASE=$2; CSS_GLOB="web/static/css/*.css"; ADDED_LINES=$3; . "$4"' \
    _ "$REPO_ROOT/scripts/lib/git-plain.sh" "$BASE" "$W/added.txt" "$W/lines.sh") >/dev/null 2>&1 || true
  if [ "$(cat "$W/added.txt")" = "$WANT" ]; then pass "added lines intact: $name"
  else fail "ADDED LINES WRONG: $name -> $(tr '\n' ' ' < "$W/added.txt")"; fi
done

echo "C. check-plain-git-diff.sh guard"
guard() { bash "$REPO_ROOT/scripts/check-plain-git-diff.sh" "$1" >"$W/g.out" 2>&1 && return 0 || return 1; }
mk() { rm -rf "$W/t"; mkdir -p "$W/t/scripts"; printf '%s\n' "$1" > "$W/t/scripts/$2"; }
mk 'x=$(git_plain_diff --name-only "$B")
git show main:a/b.yaml > f
git diff --no-ext-diff --no-textconv --no-color "$B"
y=$(git diff "$B") # plain-git-exempt: names only, reason
# git diff in a comment' ok.sh
guard "$W/t" && pass "accepts helper, blob show, inline flags, exemption, comment" || fail "rejected a clean tree"
for bad in 'x=$(git diff "$BASE"..HEAD)' 'x=$(git -c a=b diff --unified=0 "$B")' 'git log -p -1' 'git show HEAD' 'git diff --name-only'; do
  mk "$bad" bad.sh
  guard "$W/t" && fail "accepted: $bad" || { grep -q 'bad.sh' "$W/g.out" && pass "rejects: $bad" || fail "no location for: $bad"; }
done
mk 'r = sh(["git", "diff", "--name-status", rng])' bad.py
guard "$W/t" && fail "accepted raw python diff" || pass "rejects raw python diff argv"
mk 'GIT_PLAIN_DIFF = ["git", "diff"]
r = sh(GIT_PLAIN_DIFF + ["--name-status"])
s = subprocess.run(["git", "show", f"{b}:{p}"])' ok.py
guard "$W/t" && pass "accepts GIT_PLAIN_DIFF and blob show in python" || fail "rejected clean python"
guard "$REPO_ROOT" && pass "repo scripts/ is clean" || fail "repo scripts/ has a raw parsed git diff"
exit $rc
