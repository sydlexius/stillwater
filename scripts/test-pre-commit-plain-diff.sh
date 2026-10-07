#!/usr/bin/env bash
#
# test-pre-commit-plain-diff.sh -- tests for the diff handling in
# .githooks/pre-commit (#3446): the conflict-marker scan must not read binaries
# as text, and the stale-generated-file checks must not be skippable by diff
# config or a failing git. The hook's sections are cut out by marker and run in
# a fixture repo (HOOK_SH overrides the hook, to show a case RED on an older one).
# Run: bash scripts/test-pre-commit-plain-diff.sh
# shellcheck disable=SC2015,SC2016  # A && B || C is report-only; single-quoted fixtures
set -euo pipefail
REPO_ROOT=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
HOOK_SH="${HOOK_SH:-$REPO_ROOT/.githooks/pre-commit}"
# shellcheck source=scripts/lib/git-clean-env.sh
. "$REPO_ROOT/scripts/lib/git-clean-env.sh"
git_clean_env_unset
for v in $(env | sed -n -E 's/^(GIT_CONFIG_(COUNT|PARAMETERS|KEY_[0-9]+|VALUE_[0-9]+))=.*/\1/p'); do unset "$v"; done
export GIT_CONFIG_GLOBAL=/dev/null GIT_CONFIG_SYSTEM=/dev/null
W=$(mktemp -d); W=$(cd "$W" && pwd -P); trap 'rm -rf "$W"' EXIT
rc=0
pass() { echo "  PASS  $1"; }
fail() { echo "  FAIL  $1" >&2; rc=1; }

hln() { grep -n "$1" "$HOOK_SH" | head -1 | cut -d: -f1; }
pre=$(( $(hln '^# 0\. conflict markers') - 2 ))
src=$(grep -m1 'lib/git-plain\.sh"$' "$HOOK_SH")
hcut() { sed -n "${1},${2}p" "$HOOK_SH"; }
mksec() { { hcut 1 "$pre"; echo "$src"; hcut "$2" "$3"; echo 'echo SECTION-DONE'; } > "$W/$1.sh"; }
s0a=$(( $(hln '^# 0a\. signed commits') - 2 ))
mksec sec0 "$(( pre + 1 ))" "$s0a"
mksec sec3 "$(hln '^STAGED_TEMPL=')" "$(( $(hln 'warn "templ (no staged') + 1 ))"
mksec sec3a "$(hln '^STAGED_TAILWIND_SRC=')" "$(( $(hln 'warn "tailwind (no staged') + 1 ))"

R="$W/repo"; git init -q "$R"; git -C "$R" config user.name T; git -C "$R" config user.email t@localhost
mkdir -p "$R/scripts/lib" "$R/web/pages" "$R/web/static/css" "$W/bin"
cp "$REPO_ROOT/scripts/lib/git-plain.sh" "$R/scripts/lib/"
printf 'templ v1\n' > "$R/web/pages/page.templ"; printf 'package pages // v1\n' > "$R/web/pages/page_templ.go"
printf '@tw;\n' > "$R/web/static/css/input.css"; printf '.a{}\n' > "$R/web/static/css/styles.css"
git -C "$R" add -A; git -C "$R" commit -qm base
# Fake generators: STALE_GEN=1 leaves the generated files different from the committed ones.
printf '#!/bin/sh\n[ -z "$STALE_GEN" ] || printf "package pages // v2\\n" > "%s/web/pages/page_templ.go"\n' "$R" > "$W/bin/go"
printf '#!/bin/sh\n[ -z "$STALE_GEN" ] || printf ".a{}.b{}\\n" > "%s/web/static/css/styles.css"\n' "$R" > "$W/bin/tailwindcss"
chmod +x "$W/bin/go" "$W/bin/tailwindcss"

BROKEN=(GIT_CONFIG_COUNT=1 GIT_CONFIG_KEY_0=diff.orderFile GIT_CONFIG_VALUE_0="$W/missing-order")
LITERAL=(GIT_LITERAL_PATHSPECS=1)
# run <section> <expected rc> <text> <name> [ENV=VAL ...]; staging is done by the caller.
run() {
  local sec=$1 want=$2 text=$3 name=$4; shift 4
  local out s=0
  out=$(cd "$R" && env PATH="$W/bin:$PATH" "$@" bash "$W/$sec.sh" 2>&1) || s=$?
  if [ "$s" -eq "$want" ] && grep -q "$text" <<<"$out"; then pass "$name"; else fail "$name (exit $s, want $want /$text/): $(head -3 <<<"$out")"; fi
}
reset() { git -C "$R" reset -q --hard; rm -f "$R/.git/info/attributes" "$R"/*.bin "$R"/*.png "$R"/*.txt; }

echo "0. conflict-marker scan"
reset; printf '*.bin binary\n' > "$R/.git/info/attributes"
printf 'data\n=======\nmore\n' > "$R/x.bin"; git -C "$R" add x.bin
run sec0 0 SECTION-DONE "a binary-attributed file holding a ======= line is allowed"
reset; printf 'a\n<<<<<<< ours\nb\n' > "$R/c.txt"; git -C "$R" add c.txt
run sec0 1 "conflict-markers" "a text file with a conflict marker blocks"
reset; printf 'a\n' > "$R/a.txt"; printf 'b\n' > "$R/b.txt"; git -C "$R" add a.txt b.txt
run sec0 1 "could not be read" "a failing git blocks the commit" "${BROKEN[@]}"
reset; { printf '\211PNG\r\n\032\n\0\0\0'; head -c 100000 /dev/zero; } > "$R/p.png"; git -C "$R" add p.png
run sec0 0 "^PASS conflict-markers" "a staged PNG passes"
out=$(cd "$R" && bash "$W/sec0.sh" 2>&1) || true
grep -qi 'null byte' <<<"$out" && fail "staging a binary prints a null-byte warning" || pass "staging a binary is quiet"

echo "3. stale generated files cannot be skipped"
stage_templ() { reset; printf 'templ v2\n' > "$R/web/pages/page.templ"; git -C "$R" add web/pages/page.templ; }
stage_css() { reset; printf '@tw; /* v2 */\n' > "$R/web/static/css/input.css"; git -C "$R" add web/static/css/input.css; }
stage_templ; run sec3 0 "^PASS templ generate" "templ: up-to-date generated file passes"
stage_templ; run sec3 1 "out of date" "templ: stale file blocks" STALE_GEN=1
stage_templ; run sec3 1 "out of date" "templ: stale file blocks under GIT_LITERAL_PATHSPECS=1" STALE_GEN=1 "${LITERAL[@]}"
stage_templ; out=$(cd "$R" && env PATH="$W/bin:$PATH" STALE_GEN=1 "${BROKEN[@]}" bash "$W/sec3.sh" 2>&1) && fail "templ: stale file committed with a failing git" || pass "templ: a failing git blocks"
stage_css; run sec3a 0 "^PASS tailwind" "tailwind: up-to-date styles.css passes"
stage_css; run sec3a 1 "out of date" "tailwind: stale styles.css blocks" STALE_GEN=1
stage_css; run sec3a 1 "out of date" "tailwind: stale styles.css blocks under GIT_LITERAL_PATHSPECS=1" STALE_GEN=1 "${LITERAL[@]}"
stage_css; out=$(cd "$R" && env PATH="$W/bin:$PATH" STALE_GEN=1 "${BROKEN[@]}" bash "$W/sec3a.sh" 2>&1) && fail "tailwind: stale file committed with a failing git" || pass "tailwind: a failing git blocks"
# A git whose ONLY failure is the unstaged `diff` (the staged --cached diff works),
# so the hook reaches its dirty-file checks before failing.
mkdir -p "$W/shim"; REALGIT=$(command -v git)
printf '%s\n' '#!/bin/sh' 'd=0; c=0; for a in "$@"; do [ "$a" = diff ] && d=1; [ "$a" = --cached ] && c=1; done' \
  "[ \$d = 1 ] && [ \$c = 0 ] && exit 1" "exec $REALGIT \"\$@\"" > "$W/shim/git"; chmod +x "$W/shim/git"
SHIM_PATH="$W/shim:$W/bin:$PATH"
stage_templ; run sec3 1 "could not list" "templ: failing unstaged diff blocks (not read as clean)" STALE_GEN=1 PATH="$SHIM_PATH"
stage_css; run sec3a 1 "could not list" "tailwind: failing unstaged diff blocks (not read as clean)" STALE_GEN=1 PATH="$SHIM_PATH"
exit $rc
