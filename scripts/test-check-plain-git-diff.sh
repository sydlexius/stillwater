#!/usr/bin/env bash
#
# test-check-plain-git-diff.sh -- tests for #3446: the gate's parsed `git diff`
# checks must not be changeable by the developer's diff tooling, and must fail
# closed when git itself fails.
#   A. the REAL raw-error-leak arm of pre-push-gate.sh (cut out by markers, run
#      with the gate's OWN lib source line) reports the leak under every variant.
#   B. the REAL changed-line extraction of stylelint-diff-gate.sh, same way
#      (stylelint itself is not needed for this step).
#   C. a git that fails (diff.orderFile naming a missing file) makes the leak
#      arm, the stylelint skip/extraction arms and check-generated's dirty
#      checks exit non-zero instead of reading as "no changes".
#   D. -M and "$@" are pinned (rename-only commit; pathspec with a space).
#   E. check-plain-git-diff.sh rejects raw parsed diffs and accepts the rest.
# GATE_SH / STYLELINT_SH / CHECKGEN_SH / LIB_SH override what is tested (used to
# show each case RED against an older copy). Run: bash scripts/test-check-plain-git-diff.sh
# shellcheck disable=SC2016,SC2015  # single-quoted fixtures; A && B || C is report-only
set -euo pipefail
REPO_ROOT=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
GATE_SH="${GATE_SH:-$REPO_ROOT/scripts/pre-push-gate.sh}"
STYLELINT_SH="${STYLELINT_SH:-$REPO_ROOT/scripts/stylelint-diff-gate.sh}"
CHECKGEN_SH="${CHECKGEN_SH:-$REPO_ROOT/scripts/check-generated.sh}"
LIB_SH="${LIB_SH:-$REPO_ROOT/scripts/lib/git-plain.sh}"
# shellcheck source=scripts/lib/git-clean-env.sh
. "$REPO_ROOT/scripts/lib/git-clean-env.sh"
git_clean_env_unset
export GIT_CONFIG_GLOBAL=/dev/null GIT_CONFIG_SYSTEM=/dev/null
W=$(mktemp -d); W=$(cd "$W" && pwd -P); trap 'rm -rf "$W"' EXIT
rc=0
pass() { echo "  PASS  $1"; }
fail() { echo "  FAIL  $1" >&2; rc=1; }

# The arms run with the SCRIPT_DIR the real scripts use, holding the lib under test.
mkdir -p "$W/sd/lib"; cp "$LIB_SH" "$W/sd/lib/git-plain.sh"
srcline() { grep -m1 '^\. "\$SCRIPT_DIR/lib/git-plain.sh"$' "$1" || true; }

# Fixture: base, then a head commit that leaks err.Error() in a handler, edits two
# NON-ADJACENT spots in x.css, adds a non-ASCII CSS file and a spaced directory.
R="$W/repo"; git init -q "$R"
git -C "$R" config user.name T; git -C "$R" config user.email t@localhost
mkdir -p "$R/internal/api" "$R/web/static/css" "$R/sp ace"
printf 'package api\n' > "$R/internal/api/handlers_x.go"
printf 'a {\n  color: red;\n}\nb {\n  top: 0;\n}\nc {\n  left: 0;\n}\n' > "$R/web/static/css/x.css"
printf '*.go diff=sw\n*.css diff=sw\n' > "$R/.gitattributes"
printf 'package api\n' > "$R/gen_templ.go"; printf 'x{}\n' > "$R/web/static/css/styles.css"
git -C "$R" add -A; git -C "$R" commit -qm base; BASE=$(git -C "$R" rev-parse HEAD)
printf 'package api\nfunc h() { w.Write(err.Error()) }\n' > "$R/internal/api/handlers_x.go"
printf 'a {\n  color: blue;\n  margin: 0;\n}\nb {\n  top: 0;\n}\nc {\n  left: 1px;\n}\n' > "$R/web/static/css/x.css"
printf 'b {}\n' > "$R/web/static/css/é.css"; printf 'z {}\n' > "$R/sp ace/y.css"
git -C "$R" add -A; git -C "$R" commit -qm head
printf '#!/bin/sh\necho "side-by-side $1"\n' > "$W/ext.sh"
printf '#!/bin/sh\necho converted\n' > "$W/tc.sh"; chmod +x "$W/ext.sh" "$W/tc.sh"

# Variants: name|env assignments separated by US (0x1f)|setup. Config goes through
# GIT_CONFIG_COUNT so no developer file is needed; setup "attr" marks files binary.
US=$'\037'
cfg() { printf 'GIT_CONFIG_COUNT=%s' "$(($# / 2))"; local i=0
        while [ $# -gt 0 ]; do printf '%sGIT_CONFIG_KEY_%s=%s%sGIT_CONFIG_VALUE_%s=%s' "$US" "$i" "$1" "$US" "$i" "$2"; shift 2; i=$((i+1)); done; }
VARIANTS=(
  "clean|X=1|"
  "GIT_EXTERNAL_DIFF|GIT_EXTERNAL_DIFF=$W/ext.sh|"
  "diff.external via GIT_CONFIG_COUNT|$(cfg diff.external "$W/ext.sh")|"
  "diff.external via GIT_CONFIG_PARAMETERS|GIT_CONFIG_PARAMETERS='diff.external'='$W/ext.sh'|"
  "color.ui=always|$(cfg color.ui always)|"
  "color.diff=always|$(cfg color.diff always)|"
  "textconv driver|$(cfg diff.sw.textconv "$W/tc.sh")|"
  "diff.noprefix|$(cfg diff.noprefix true)|"
  "diff.mnemonicPrefix|$(cfg diff.mnemonicPrefix true)|"
  "GIT_DIFF_OPTS=-u5|GIT_DIFF_OPTS=-u5|"
  "binary attribute|X=1|attr"
  "diff.interHunkContext|$(cfg diff.interHunkContext 9)|"
  "GIT_LITERAL_PATHSPECS|GIT_LITERAL_PATHSPECS=1|"
  "GIT_NOGLOB_PATHSPECS|GIT_NOGLOB_PATHSPECS=1|"
)
setup() { rm -f "$R/.git/info/attributes"; [ "$1" != attr ] || printf '* -diff\n' > "$R/.git/info/attributes"; }
RAWSPEC=('internal/api/handlers_*.go' 'web/static/css/*.css')
rawdiff() { (cd "$R" && env "${E[@]}" git diff --unified=0 "$BASE" -- "${RAWSPEC[@]}"); }

echo "0. each hostile variant really changes raw git diff (and does not break git)"
setup ""; E=(X=1); CLEAN_RAW=$(rawdiff)
for v in "${VARIANTS[@]}"; do
  IFS='|' read -r name envs set <<<"$v"; [ "$name" != clean ] || continue
  setup "$set"; IFS=$US read -ra E <<<"$envs"
  if ! got=$(rawdiff 2>/dev/null); then fail "variant breaks git itself, not a tooling variant: $name"
  elif [ "$got" = "$CLEAN_RAW" ]; then fail "variant does not alter raw git diff (vacuous): $name"
  else pass "raw diff differs from clean: $name"; fi
done

echo "A. raw-error-leak arm detects the leak under every variant"
sed -nE '/^(leak_diff|error_leaks)=\$\(/,/^echo "OK"$/p' "$GATE_SH" > "$W/leak.sh"
grep -q 'exit 1' "$W/leak.sh" || { echo "FAIL: leak arm markers not found in $GATE_SH" >&2; exit 1; }
GATE_SRC=$(srcline "$GATE_SH"); [ -n "$GATE_SRC" ] || fail "gate has no git-plain.sh source line"
for v in "${VARIANTS[@]}"; do
  IFS='|' read -r name envs set <<<"$v"; setup "$set"; IFS=$US read -ra E <<<"$envs"
  out=$(cd "$R" && env "${E[@]}" bash -c 'SCRIPT_DIR=$1; BASE=$2; eval "$3"; . "$4"' _ "$W/sd" "$BASE" "$GATE_SRC" "$W/leak.sh" 2>&1) && s=0 || s=$?
  if [ "$s" -eq 1 ] && grep -q 'CRITICAL' <<<"$out"; then pass "leak detected: $name"
  else fail "LEAK MISSED (exit $s): $name"; fi
done

echo "B. stylelint changed-line extraction is the same under every variant"
sed -nE '/^current_file=""$/,/^done < /p' "$STYLELINT_SH" > "$W/lines.sh"
grep -q '^done < ' "$W/lines.sh" || { echo "FAIL: extraction markers not found in $STYLELINT_SH" >&2; exit 1; }
SL_SRC=$(srcline "$STYLELINT_SH"); [ -n "$SL_SRC" ] || fail "stylelint script has no git-plain.sh source line"
WANT=$(printf 'web/static/css/x.css:2\nweb/static/css/x.css:3\nweb/static/css/x.css:9\nweb/static/css/é.css:1')
for v in "${VARIANTS[@]}"; do
  IFS='|' read -r name envs set <<<"$v"; setup "$set"; IFS=$US read -ra E <<<"$envs"; : > "$W/added.txt"
  (cd "$R" && env "${E[@]}" bash -c 'SCRIPT_DIR=$1; BASE=$2; CSS_GLOB="web/static/css/*.css"; ADDED_LINES=$3; WORK_DIR=$5; eval "$6"; . "$4"' \
    _ "$W/sd" "$BASE" "$W/added.txt" "$W/lines.sh" "$W" "$SL_SRC") >/dev/null 2>&1 || true
  if [ "$(cat "$W/added.txt")" = "$WANT" ]; then pass "added lines intact: $name"
  else fail "ADDED LINES WRONG: $name -> $(tr '\n' ' ' < "$W/added.txt")"; fi
done
setup ""

echo "C. a failing git fails closed (diff.orderFile names a missing file)"
# git only reads the order file when there is a diff to sort, so dirty tracked files.
echo more >> "$R/gen_templ.go"; echo more >> "$R/web/static/css/styles.css"
BROKEN=(env GIT_CONFIG_COUNT=1 GIT_CONFIG_KEY_0=diff.orderFile GIT_CONFIG_VALUE_0="$W/missing-orderfile")
failclosed() { # <name> <script file> -- the script must exit non-zero and not print OK
  local out s=0
  out=$(cd "$R" && "${BROKEN[@]}" bash -c 'set -euo pipefail; SCRIPT_DIR=$1; BASE=$2; CSS_GLOB="web/static/css/*.css"; WORK_DIR=$4; ADDED_LINES=$4/a; . "$1/lib/git-plain.sh"; . "$3"' _ "$W/sd" "$BASE" "$2" "$W" 2>&1) || s=$?
  if [ "$s" -ne 0 ] && ! grep -qx 'OK' <<<"$out"; then pass "fails closed (exit $s): $1"; else fail "READ A FAILED GIT AS NO CHANGES (exit $s): $1"; fi
}
cp "$W/leak.sh" "$W/c-leak.sh"; failclosed "leak arm" "$W/c-leak.sh"
sed -nE '/^(changed_css=|if \[ -z "\$\(git_plain_diff)/,/^fi$/p' "$STYLELINT_SH" > "$W/c-skip.sh"
failclosed "stylelint skip decision" "$W/c-skip.sh"
cp "$W/lines.sh" "$W/c-lines.sh"; failclosed "stylelint extraction" "$W/c-lines.sh"
sed -n '/^dirty_templ=/p' "$CHECKGEN_SH" > "$W/c-gen1.sh"; failclosed "check-generated templ check" "$W/c-gen1.sh"
sed -nE '/^(dirty_tracked=|wholesale_dirty=\$\($)/,/^(wholesale_dirty=.*sort -u|\))$/p' "$CHECKGEN_SH" > "$W/c-gen2.sh"
failclosed "check-generated wholesale check" "$W/c-gen2.sh"
sed -nE '/^changed_go_raw=/,/^MODIFIED_GO_FILES=/p; /^MODIFIED_GO_FILES=\$\(git_plain_diff/,/templ/p' "$GATE_SH" > "$W/c-go.sh"
failclosed "gate changed-Go list" "$W/c-go.sh"

git -C "$R" checkout -q -- gen_templ.go web/static/css/styles.css

echo "D. -M and \"\$@\" are pinned"
R2="$W/repo2"; git init -q "$R2"; git -C "$R2" config user.name T; git -C "$R2" config user.email t@localhost
mkdir -p "$R2/internal/api"; { seq 1 40; echo 'w.Write(err.Error())'; } > "$R2/internal/api/handlers_old.go"
git -C "$R2" add -A; git -C "$R2" commit -qm b; B2=$(git -C "$R2" rev-parse HEAD)
git -C "$R2" mv internal/api/handlers_old.go internal/api/handlers_new.go; echo more >> "$R2/internal/api/handlers_new.go"
git -C "$R2" add -A; git -C "$R2" commit -qm rename
out=$(cd "$R2" && env GIT_CONFIG_COUNT=1 GIT_CONFIG_KEY_0=diff.renames GIT_CONFIG_VALUE_0=false \
  bash -c 'SCRIPT_DIR=$1; BASE=$2; eval "$3"; . "$4"' _ "$W/sd" "$B2" "$GATE_SRC" "$W/leak.sh" 2>&1) && s=0 || s=$?
[ "$s" -eq 0 ] && pass "a renamed file's old leak is not new (rename detection pinned)" || fail "rename read as a new file under diff.renames=false (exit $s)"
got=$(cd "$R" && bash -c '. "$1"; git_plain_diff --name-only "$2" HEAD -- "sp ace/*.css"' _ "$W/sd/lib/git-plain.sh" "$BASE")
[ "$got" = "sp ace/y.css" ] && pass "pathspec containing a space reaches git as one argument" || fail "spaced pathspec lost: [$got]"

got=$(cd "$R" && env GIT_GLOB_PATHSPECS=1 git diff --name-only "$BASE" -- '*.go')
want=$(cd "$R" && env GIT_GLOB_PATHSPECS=0 git diff --name-only "$BASE" -- '*.go')
[ "$got" != "$want" ] && pass "precondition: GIT_GLOB_PATHSPECS=1 changes what '*.go' matches" || fail "GIT_GLOB_PATHSPECS variant is vacuous"
got=$(cd "$R" && env GIT_GLOB_PATHSPECS=1 bash -c '. "$1"; git_plain_diff --name-only "$2" -- "*.go"' _ "$W/sd/lib/git-plain.sh" "$BASE")
[ "$got" = "$want" ] && pass "git_plain_diff ignores GIT_GLOB_PATHSPECS" || fail "GIT_GLOB_PATHSPECS changed the changed-file list: [$got]"

echo "E. check-plain-git-diff.sh guard"
guard() { bash "$REPO_ROOT/scripts/check-plain-git-diff.sh" "$1" >"$W/g.out" 2>&1 && return 0 || return 1; }
mk() { rm -rf "$W/t"; mkdir -p "$W/t/scripts/lib" "$W/t/.githooks"; printf '%s\n' "$1" > "$W/t/$2"; }
mk 'x=$(git_plain_diff --name-only "$B")
git show main:a/b.yaml > f
git show ":$f" | grep x
git --no-pager show --format=%H:%s HEAD:f
y=$(git diff "$B") # plain-git-exempt: names only, reason
# git diff in a comment' scripts/ok.sh
guard "$W/t" && pass "accepts helper, blob shows, exemption with reason, comment" || fail "rejected a clean tree"
mk 'git diff --name-only -z' .githooks/pre-commit
guard "$W/t" && pass "accepts a hook --name-only listing" || fail "rejected hook name listing"
for bad in 'x=$(git diff "$BASE"..HEAD)' 'x=$(git -c a=b diff --unified=0 "$B")' 'git log -p -1' 'git show HEAD' \
  'x=$(git diff|grep "^+")' 'git diff>"$o"' 'git diff;' 'x=`git diff`' 'x=$(git --no-pager diff "$B")' 'x=$(git -C "$my dir" diff "$B")' \
  'x=$(git diff-tree -p "$B" HEAD)' 'x=$(git diff-index -p "$B")' 'x=$(git diff-files -p)' \
  'x=$(git show --format=%H:%s HEAD | grep x)' 'x=$(git diff "$B") # plain-git-exempt:' 'x=$(git diff "$B"); echo "plain-git-exempt: r"' \
  'x=$(git diff --no-ext-diff --no-textconv --no-color -U0 "$B")'; do
  for where in scripts/probe.sh scripts/lib/probe.sh .githooks/probe; do
    mk "$bad" "$where"
    guard "$W/t" && fail "accepted in $where: $bad" || { grep -q 'probe' "$W/g.out" || fail "no location in $where: $bad"; }
  done
  pass "rejects in scripts, scripts/lib and .githooks: $bad"
done
mk 'r = sh(["git", "diff", "--name-status", rng])' scripts/bad.py
guard "$W/t" && fail "accepted raw python diff (double quotes)" || pass "rejects raw python diff argv"
mk "r = sh(['git', '-c', 'x=y', 'log', '-p'])" scripts/bad.py
guard "$W/t" && fail "accepted raw python log (single quotes)" || pass "rejects single-quoted python argv"
mk 'GIT_PLAIN_DIFF = ["git", "diff", "-M"]' scripts/bad.py
guard "$W/t" && fail "accepted a GIT_PLAIN_DIFF without the hardening flags" || pass "rejects a weakened GIT_PLAIN_DIFF"
mk 'GIT_PLAIN_DIFF = ["git", "diff", "--no-ext-diff", "--no-textconv", "--no-color", "--text", "--inter-hunk-context=0", "-M"]
r = sh(GIT_PLAIN_DIFF + ["--name-status"])
d = {"diff": 1}
s = subprocess.run(["git", "show", f"{b}:{p}"])' scripts/ok.py
guard "$W/t" && pass "accepts GIT_PLAIN_DIFF, a dict key and blob show in python" || fail "rejected clean python"
guard "$REPO_ROOT" && pass "repo scripts/ and .githooks/ are clean" || { fail "repo has a raw parsed git diff"; cat "$W/g.out" >&2; }
exit $rc
