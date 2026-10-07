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
# GATE_SH / STYLELINT_SH / CHECKGEN_SH / LIB_SH / GUARD_SH override what is tested (used to
# show each case RED against an older copy). Run: bash scripts/test-check-plain-git-diff.sh
# shellcheck disable=SC2016,SC2015  # single-quoted fixtures; A && B || C is report-only
set -euo pipefail
REPO_ROOT=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
GATE_SH="${GATE_SH:-$REPO_ROOT/scripts/pre-push-gate.sh}"
STYLELINT_SH="${STYLELINT_SH:-$REPO_ROOT/scripts/stylelint-diff-gate.sh}"
CHECKGEN_SH="${CHECKGEN_SH:-$REPO_ROOT/scripts/check-generated.sh}"
LIB_SH="${LIB_SH:-$REPO_ROOT/scripts/lib/git-plain.sh}"
GUARD_SH="${GUARD_SH:-$REPO_ROOT/scripts/check-plain-git-diff.sh}"
# shellcheck source=scripts/lib/git-clean-env.sh
. "$REPO_ROOT/scripts/lib/git-clean-env.sh"
git_clean_env_unset
# git_clean_env_unset keeps the config-injection variables by design; this suite
# takes a "clean" baseline, so a caller's `git -c k=v push` (which exports them)
# would make a variant equal the baseline. Strip them here.
for v in $(env | sed -n -E 's/^(GIT_CONFIG_(COUNT|PARAMETERS|KEY_[0-9]+|VALUE_[0-9]+))=.*/\1/p'); do unset "$v"; done
export GIT_CONFIG_GLOBAL=/dev/null GIT_CONFIG_SYSTEM=/dev/null
W=$(mktemp -d); W=$(cd "$W" && pwd -P); trap 'rm -rf "$W"' EXIT
rc=0
pass() { echo "  PASS  $1"; }
fail() { echo "  FAIL  $1" >&2; rc=1; }

# The arms run with the SCRIPT_DIR the real scripts use, holding the lib under test.
mkdir -p "$W/sd/lib"; cp "$LIB_SH" "$W/sd/lib/git-plain.sh"
srcline() { grep -m1 '^\. .*lib/git-plain\.sh"$' "$1" || true; }

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

echo "B2. a replace ref cannot hide the leak"
HEADSHA=$(git -C "$R" rev-parse HEAD); git -C "$R" replace -f "$HEADSHA" "$BASE"
[ -z "$(git -C "$R" diff "$BASE"..HEAD -- internal/api/handlers_x.go)" ] && pass "precondition: the replace ref empties raw git diff" || fail "replace variant is vacuous"
out=$(cd "$R" && bash -c 'SCRIPT_DIR=$1; BASE=$2; eval "$3"; . "$4"' _ "$W/sd" "$BASE" "$GATE_SRC" "$W/leak.sh" 2>&1) && s=0 || s=$?
git -C "$R" replace -d "$HEADSHA" >/dev/null
[ "$s" -eq 1 ] && grep -q CRITICAL <<<"$out" && pass "leak detected despite a replace ref" || fail "LEAK MISSED under a replace ref (exit $s)"

echo "C. a failing git fails closed (diff.orderFile names a missing file)"
# git only reads the order file when there is a diff to sort, so dirty tracked files.
echo more >> "$R/gen_templ.go"; echo more >> "$R/web/static/css/styles.css"
BROKEN=(env GIT_CONFIG_COUNT=1 GIT_CONFIG_KEY_0=diff.orderFile GIT_CONFIG_VALUE_0="$W/missing-orderfile")
# mkarm <script> <cut> <out> [prelude]: a runner holding the script's OWN lib source line.
mkarm() { { echo 'set -euo pipefail'; echo "SCRIPT_DIR=$W/sd BASE=$BASE CSS_GLOB='web/static/css/*.css' WORK_DIR=$W ADDED_LINES=$W/a"
  echo "${4:-:}"; srcline "$1"; cat "$2"; } > "$3"; }
# failclosed <name> <script> <cut> [prelude]: broken git -> exit 2 with a FAIL line; healthy git -> not 2.
failclosed() {
  local out s=0 h=0 broken=("${BROKEN[@]}")
  [ -z "${4:-}" ] || broken=(env X=1)
  mkarm "$2" "$3" "$W/sd/arm-b.sh" "${4:-}"; mkarm "$2" "$3" "$W/sd/arm-h.sh"
  out=$(cd "$R" && "${broken[@]}" bash "$W/sd/arm-b.sh" 2>&1) || s=$?
  if [ "$s" -eq 2 ] && grep -q 'FAIL' <<<"$out"; then pass "fails closed (exit 2 + FAIL line): $1"; else fail "READ A FAILED GIT AS NO CHANGES (exit $s): $1"; fi
  (cd "$R" && bash "$W/sd/arm-h.sh" >"$W/h.out" 2>&1) || h=$?
  [ "$h" -ne 2 ] && pass "healthy control does not exit 2: $1" || fail "healthy git exited 2: $1: $(head -2 "$W/h.out")"
}
failclosed "leak arm" "$GATE_SH" "$W/leak.sh"
sed -nE '/^changed_css=/,/^fi$/p' "$STYLELINT_SH" > "$W/c-skip.sh"; failclosed "stylelint skip decision" "$STYLELINT_SH" "$W/c-skip.sh"
failclosed "stylelint extraction" "$STYLELINT_SH" "$W/lines.sh"
sed -n '/^dirty_templ=/p' "$CHECKGEN_SH" > "$W/c-gen1.sh"; failclosed "check-generated templ check" "$CHECKGEN_SH" "$W/c-gen1.sh"
sed -n '/^  dirty_css=/p' "$CHECKGEN_SH" > "$W/c-gen3.sh"; failclosed "check-generated styles.css check" "$CHECKGEN_SH" "$W/c-gen3.sh"
sed -n '/^dirty_tracked=/,/^wholesale_dirty=/p' "$CHECKGEN_SH" > "$W/c-gen2.sh"; failclosed "check-generated wholesale diff" "$CHECKGEN_SH" "$W/c-gen2.sh"
sed -n '/^dirty_untracked=/p' "$CHECKGEN_SH" > "$W/c-gen4.sh"
failclosed "check-generated ls-files" "$CHECKGEN_SH" "$W/c-gen4.sh" 'git() { case "$1" in ls-files) return 1;; *) command git "$@";; esac; }'
sed -nE '/^changed_go_raw=/,/^MODIFIED_GO_FILES=/p' "$GATE_SH" > "$W/c-go.sh"; failclosed "gate changed-Go list" "$GATE_SH" "$W/c-go.sh"

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

got=$(cd "$R" && bash -c 'git() { echo HIJACK; return 3; }; . "$1"; git_plain_diff --name-only "$2" -- "*.go"' _ "$W/sd/lib/git-plain.sh" "$BASE")
[ "$got" = "internal/api/handlers_x.go" ] && pass "an exported/defined git function cannot hijack the helper" || fail "git function hijacked the helper: [$got]"

echo "F. OpenAPI base read: skip only on a positive 'absent'"
sed -n '/^  if GIT_NO_REPLACE_OBJECTS=1 git show main:internal\/api\/openapi.yaml/,/^  fi$/p' "$GATE_SH" > "$W/oa.sh"
grep -q 'ls-tree' "$W/oa.sh" || fail "openapi arm cut is wrong"
printf '#!/bin/sh\nexit 0\n' > "$W/oasdiff"; chmod +x "$W/oasdiff"
R3="$W/repo3"; git -c init.defaultBranch=main init -q "$R3"; git -C "$R3" config user.name T; git -C "$R3" config user.email t@localhost
oarun() { # <expect-rc> <expect-text> <name> <prelude>
  { echo 'set -euo pipefail'; echo "tmp_openapi=$W/o.yaml oasdiff_err=$W/oerr oasdiff_bin=$W/oasdiff"; echo "$4"; cat "$W/oa.sh"; } > "$W/oa-run.sh"
  local o r=0; o=$(cd "$R3" && bash "$W/oa-run.sh" 2>&1) || r=$?
  [ "$r" -eq "$1" ] && grep -q "$2" <<<"$o" && pass "$3" || fail "$3 (exit $r): $o"
}
git -C "$R3" commit -q --allow-empty -m none
oarun 0 "Skipped" "absent on main: skipped" ':'
mkdir -p "$R3/internal/api"; echo 'openapi: 3.1.0' > "$R3/internal/api/openapi.yaml"; git -C "$R3" add -A; git -C "$R3" commit -qm spec
oarun 0 "No breaking" "present on main: compared" ':'
oarun 1 "FAIL" "git cannot answer: FAIL, not skip" 'git() { case "$1" in show|ls-tree) return 128;; *) command git "$@";; esac; }'
oarun 1 "although the file exists" "present but unreadable: FAIL" 'git() { case "$1" in show) return 128;; *) command git "$@";; esac; }'
# Replace main's tip with the spec-less commit: the real tree still holds the spec, so the arm must
# still see it (show -> compared; show failing -> ls-tree says present -> FAIL, never "Skipped").
git -C "$R3" replace -f main "$(git -C "$R3" rev-parse main~1)"
oarun 0 "No breaking" "replace ref on main: show still reads the real spec" ':'
oarun 1 "although the file exists" "replace ref on main: ls-tree still says present" 'git() { case "$1" in show) return 128;; *) command git "$@";; esac; }'
git -C "$R3" replace -d "$(git -C "$R3" rev-parse main)" >/dev/null 2>&1 || true

echo "E. check-plain-git-diff.sh guard"
guard() { bash "$GUARD_SH" "$1" >"$W/g.out" 2>&1 && return 0 || return 1; }
mk() { rm -rf "$W/t"; mkdir -p "$W/t/scripts/lib" "$W/t/.githooks"; printf '%s\n' "$1" > "$W/t/$2"; }
mk 'x=$(git_plain_diff --name-only "$B")
git show main:a/b.yaml > f
git show ":$f" | grep x
git --no-pager show --format=%H:%s HEAD:f
y=$(git diff "$B") # plain-git-exempt: names only, reason
# git diff in a comment' scripts/ok.sh
guard "$W/t" && pass "accepts helper, blob shows, exemption with reason, comment" || fail "rejected a clean tree"
mk 'git diff --name-only -z' .githooks/pre-commit
guard "$W/t" && fail "accepted a raw hook name listing" || pass "rejects a raw hook --name-only call (no special case)"
mk 'x=$(git diff "$B") # not --name-only' scripts/bad.sh
guard "$W/t" && fail "accepted a diff with --name-only in a comment" || pass "rejects --name-only mentioned in a comment"
for bad in 'x=$(git diff "$BASE"..HEAD)' 'x=$(git -c a=b diff --unified=0 "$B")' 'git log -p -1' 'git show HEAD' \
  'x=$(git diff|grep "^+")' 'git diff>"$o"' 'git diff;' 'x=`git diff`' 'x=$(git --no-pager diff "$B")' 'x=$(git -C "$my dir" diff "$B")' \
  'x=$(git diff-tree -p "$B" HEAD)' 'x=$(git diff-index -p "$B")' 'x=$(git diff-files -p)' \
  'x=$(git show --format=%H:%s HEAD | grep x)' 'x=$(git diff "$B") # plain-git-exempt:' 'x=$(git diff "$B"); echo "plain-git-exempt: r"' 'x=$(git diff "$B"); echo "# plain-git-exempt: r"' \
  'x=$(git diff --no-ext-diff --no-textconv --no-color -U0 "$B")'; do
  okk=1
  for where in scripts/probe.sh scripts/lib/probe.sh .githooks/probe; do
    mk "$bad" "$where"
    if guard "$W/t"; then okk=0; fail "accepted in $where: $bad"; elif ! grep -q 'probe' "$W/g.out"; then okk=0; fail "no location in $where: $bad"; fi
  done
  [ "$okk" -eq 1 ] && pass "rejects in scripts, scripts/lib and .githooks: $bad"
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
mk 'r = sh(["git", "diff", "--x"], note="plain-git-exempt: r")' scripts/bad.py
guard "$W/t" && fail "accepted a python string carrying the exempt marker" || pass "rejects the exempt marker inside a python string"
mk 'r = ["git", "diff"]  # plain-git-exempt: names only' scripts/ok.py
guard "$W/t" && pass "accepts a python trailing-comment exemption with a reason" || fail "rejected a valid python exemption"
mk 'x = (' scripts/bad.py
guard "$W/t" && fail "accepted an unparsable python file" || { grep -q 'cannot parse' "$W/g.out" && pass "names the interpreter when python cannot parse" || fail "unparsable python reported badly"; }
rm -rf "$W/t"; mkdir "$W/t"
guard "$W/t" && fail "guard exits 0 having scanned nothing" || pass "scanning 0 files is a failure"
mk 'x=1' scripts/ok.sh; mkdir -p "$W/empty"
s=0; out=$(env PATH="$W/empty" "$BASH" "$GUARD_SH" "$W/t" 2>&1) || s=$?
[ "$s" -eq 2 ] && grep -q 'needs python3' <<<"$out" && pass "missing python3 FAILs naming the interpreter" || fail "missing python3 not reported (exit $s)"
guard "$REPO_ROOT" && pass "repo scripts/ and .githooks/ are clean" || { fail "repo has a raw parsed git diff"; cat "$W/g.out" >&2; }
echo "P. every shell file parses under stock macOS bash 3.2, and the guard runs there"
if [ -x /bin/bash ]; then
  for f in "$REPO_ROOT"/scripts/*.sh "$REPO_ROOT"/scripts/lib/*.sh "$REPO_ROOT"/.githooks/* "$GUARD_SH"; do
    [ -f "$f" ] || continue
    head -1 "$f" | grep -q 'sh' || continue
    /bin/bash -n "$f" 2>"$W/n.err" || fail "/bin/bash -n fails on ${f#"$REPO_ROOT"/}: $(head -1 "$W/n.err")"
  done
  /bin/bash "$GUARD_SH" "$REPO_ROOT" >"$W/g32.out" 2>&1 && pass "all shell files parse under /bin/bash; guard passes there" || fail "guard fails under /bin/bash: $(head -2 "$W/g32.out")"
else
  echo "  SKIP  /bin/bash does not exist on this host: the bash 3.2 parse check did NOT run"
fi

# S. a caller exporting diff config (git -c k=v push does) must not change the verdict.
if [ -z "${SW_PLAIN_SELFCHECK:-}" ]; then
  echo "S. the suite passes when the CALLER injects diff config"
  selfcheck() { local name=$1; shift
    if env SW_PLAIN_SELFCHECK=1 "$@" "$BASH" "${BASH_SOURCE[0]}" >"$W/self.out" 2>&1; then pass "suite green with caller config: $name"
    else fail "suite red with caller config: $name"; grep -E '^\s+FAIL' "$W/self.out" | head -3 >&2; fi; }
  # Both injection channels at once: one rerun, and unsetting either variable family goes red.
  selfcheck "GIT_CONFIG_COUNT (color.ui, interHunkContext) + GIT_CONFIG_PARAMETERS (noprefix)" GIT_CONFIG_COUNT=2 GIT_CONFIG_KEY_0=color.ui GIT_CONFIG_VALUE_0=always GIT_CONFIG_KEY_1=diff.interHunkContext GIT_CONFIG_VALUE_1=3 "GIT_CONFIG_PARAMETERS='diff.noprefix'='true'"
fi
exit $rc
