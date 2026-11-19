#!/bin/bash
# test-a11y-select.sh -- tests for RUN_A11Y=changed (#3608): scripts/a11y-select.sh
# (FULL/EMPTY classifier + map checker), the extra-token form of
# resolve_run_flag, and the gate's `changed)` arm, executed with a stub make.
# Run: bash scripts/test-a11y-select.sh
set -euo pipefail
D="$(cd "$(dirname "$0")" && pwd)"
REAL="$(cd "$D/.." && pwd)"
# Strip an inherited git environment (the pre-push hook exports GIT_DIR) BEFORE
# any git call: otherwise the fixture `git init` below re-initializes the repo
# GIT_DIR names instead of the fixture directory (#3051).
# shellcheck source=scripts/lib/git-clean-env.sh
. "$REAL/scripts/lib/git-clean-env.sh"
git_clean_env_unset
T="$(mktemp -d)"
trap 'rm -rf "$T"' EXIT
rc=0
ok() { echo "PASS: $1"; }
bad() { echo "FAIL: $1"; [ -n "${2:-}" ] && printf '%s\n' "$2" | sed 's/^/      /'; rc=1; }
# expect <name> <want-status> <want-grep-or-empty> -- uses ST and OUT
expect() {
  if [ "$ST" -eq "$2" ] && { [ -z "$3" ] || grep -q -- "$3" <<<"$OUT"; }; then ok "$1"; else bad "$1 (st=$ST, want $2 /$3/)" "$OUT"; fi
}
TAB=$'\t'
GC=(-c user.name=t -c user.email=t@t -c commit.gpgsign=false)

# --- A. resolve_run_flag extra tokens -----------------------------------------
# shellcheck source=scripts/lib/run-flags.sh
res() { ST=0; OUT=$( (. "$REAL/scripts/lib/run-flags.sh"; resolve_run_flag RUN_A11Y "$1" "${@:2}"; echo "$RESOLVED_RUN_FLAG") 2>&1) || ST=$?; }
for v in changed CHANGED " changed "; do res "$v" changed; expect "extra token '$v' resolves to changed" 0 '^changed$'; done
res "changedd" changed; expect "near-miss changedd with the token allowed exits 2" 2 "RUN_A11Y='changedd'"
res "changed"; expect "changed without the extra token exits 2 (other tiers)" 2 "RUN_A11Y='changed'"
res "on" changed; expect "on still resolves to on" 0 '^on$'
res "" changed; expect "unset still resolves to default" 0 '^default$'
res "bogus" changed; expect "garbage still exits 2 and names the token" 2 'changed'

# --- B. classifier, on a throwaway repo ---------------------------------------
mk() { # mk <dir>: fake repo with the selector, a small map and files
  local r="$1"
  mkdir -p "$r/scripts/lib" "$r/tests/a11y/helpers" "$r/web/templates" "$r/web/static/js" "$r/internal/api" "$r/docs"
  cp "$REAL/scripts/a11y-select.sh" "$r/scripts/"
  cp "$REAL/scripts/lib/git-plain.sh" "$REAL/scripts/lib/gate-stamp.sh" "$REAL/scripts/lib/run-flags.sh" "$r/scripts/lib/"
  echo "// a" >"$r/tests/a11y/a.spec.js"; : >"$r/tests/a11y/helpers/seed-x.js"; : >"$r/tests/a11y/notes.md"
  : >"$r/web/templates/page.templ"; : >"$r/web/static/js/g.js"; : >"$r/internal/api/util.go"
  : >"$r/internal/api/x_test.go"; : >"$r/docs/x.md"; : >"$r/scripts/foo.sh"; : >"$r/playwright.config.js"
  cat >"$r/tests/a11y/spec-map.toml" <<'EOF'
[shared]
paths = ["scripts/a11y-select.sh", "scripts/lib/git-plain.sh", "tests/a11y/**"]
[allow]
paths = ["docs/**", "**/*.md", "scripts/**", "**/*_test.go"]
EOF
  git -C "$r" init -q -b main . && git -C "$r" add -A && git -C "$r" "${GC[@]}" commit -q -m base
}
R="$T/r"; mk "$R"
# sel <status-path lines...>: run the selector on a synthetic file list.
sel() { local f="$T/files"; : >"$f"; for l in "$@"; do printf '%s\n' "$l" >>"$f"; done; ST=0; OUT=$(cd "$R" && bash scripts/a11y-select.sh --files-from "$f" 2>&1) || ST=$?; }
sel "M${TAB}docs/x.md" "M${TAB}internal/api/x_test.go" "M${TAB}scripts/foo.sh"
if [ "$ST" -eq 0 ] && [ "$OUT" = "MODE=EMPTY" ]; then ok "only allow-listed files (docs, Go tests, scripts) are EMPTY, nothing else printed"; else bad "allow-listed only (st=$ST)" "$OUT"; fi
for p in web/templates/page.templ web/static/js/g.js internal/api/util.go playwright.config.js; do
  sel "M${TAB}$p"; expect "a file on no list ($p) forces FULL and names it" 0 "^\*${TAB}$p: not on the non-UI allow-list"
done
for p in tests/a11y/a.spec.js tests/a11y/helpers/seed-x.js tests/a11y/notes.md; do
  sel "M${TAB}$p"; expect "anything under tests/a11y ($p) forces FULL, even a .md" 0 "^\*${TAB}$p: shared surface"
done
sel "D${TAB}tests/a11y/gone.spec.js"; expect "a deleted spec forces FULL" 0 '^MODE=FULL$'
for p in scripts/a11y-select.sh scripts/lib/git-plain.sh; do
  sel "M${TAB}$p"; expect "[shared] beats [allow]: $p forces FULL" 0 '^MODE=FULL$'
done
sel "M${TAB}docs/x.md" "D${TAB}web/templates/page.templ"; expect "one non-allow-listed file (a deletion) among docs forces FULL" 0 '^MODE=FULL$'
sel; expect "an empty diff is EMPTY" 0 '^MODE=EMPTY$'
for bad_line in "M${TAB}/etc/passwd" "M${TAB}web/../etc/x" "garbage" "X${TAB}web/templates/page.templ" "M${TAB}a\\b"; do
  sel "$bad_line"; expect "malformed or unsafe line refused: $bad_line" 2 'a11y-select'
done
# real git diff, including a deletion
git -C "$R" rm -q web/templates/page.templ; echo x >>"$R/docs/x.md"
ST=0; OUT=$(cd "$R" && bash scripts/a11y-select.sh HEAD 2>&1) || ST=$?
expect "git diff mode sees a DELETED template" 0 "web/templates/page.templ: not on the non-UI allow-list"
git -C "$R" reset -q --hard HEAD; echo x >>"$R/docs/x.md"
ST=0; OUT=$(cd "$R" && bash scripts/a11y-select.sh HEAD 2>&1) || ST=$?; expect "git diff mode: a docs-only change is EMPTY" 0 '^MODE=EMPTY$'
git -C "$R" reset -q --hard HEAD
# A committed UI change reverted in the working tree is still on the branch (m1).
echo ui >>"$R/web/templates/page.templ"; git -C "$R" add -A; git -C "$R" "${GC[@]}" commit -q -m ui
git -C "$R" show HEAD~1:web/templates/page.templ >"$R/web/templates/page.templ"
ST=0; OUT=$(cd "$R" && bash scripts/a11y-select.sh HEAD~1 2>&1) || ST=$?
expect "a committed template change reverted in the working tree still forces FULL" 0 "web/templates/page.templ: not on the non-UI allow-list"
git -C "$R" reset -q --hard HEAD~1
# A staged template change reverted only in the working tree is still a change.
cp "$R/web/templates/page.templ" "$T/page.orig"; echo staged >>"$R/web/templates/page.templ"; git -C "$R" add web/templates/page.templ
cp "$T/page.orig" "$R/web/templates/page.templ"
ST=0; OUT=$(cd "$R" && bash scripts/a11y-select.sh HEAD 2>&1) || ST=$?
expect "a staged template change reverted in the working tree still forces FULL" 0 "web/templates/page.templ: not on the non-UI allow-list"
git -C "$R" reset -q --hard HEAD
# Untracked, non-ignored files count; ignored files do not.
printf 'ignored.templ\n' >"$R/.git/info/exclude"
for c in "web/templates/new.templ:FULL" "tests/a11y/new.spec.js:FULL" "docs/new.md:EMPTY" "ignored.templ:EMPTY"; do
  f="${c%%:*}"; : >"$R/$f"; ST=0; OUT=$(cd "$R" && bash scripts/a11y-select.sh HEAD 2>&1) || ST=$?; rm -f "$R/$f"
  expect "untracked $f is ${c##*:}" 0 "^MODE=${c##*:}$"
done
# `*` does not cross `/`: a docs/*.md allow glob does not cover docs/sub/x.md.
printf '[allow]\npaths = ["docs/*.md"]\n' >"$T/star.toml"
for c in "docs/x.md:EMPTY" "docs/sub/x.md:FULL"; do
  printf 'M\t%s\n' "${c%%:*}" >"$T/files"; ST=0; OUT=$(cd "$R" && bash scripts/a11y-select.sh --map "$T/star.toml" --files-from "$T/files" 2>&1) || ST=$?
  expect "glob docs/*.md: ${c%%:*} is ${c##*:}" 0 "^MODE=${c##*:}$"
done
ST=0; OUT=$(cd "$R" && bash scripts/a11y-select.sh nonexistent-rev 2>&1) || ST=$?; expect "an unresolvable BASE exits 2" 2 'cannot resolve BASE'
# map errors are exit 2, never a skip
for badmap in 'not toml [' '[[rule]]
paths = ["web/templates/page.templ"]
specs = ["a.spec.js"]' '[allow]
paths = ["a b"]' '[allow]
paths = "docs/**"' '[shared]
paths = []
specs = ["a.spec.js"]'; do
  printf '%s\n' "$badmap" >"$R/tests/a11y/spec-map.toml"; sel "M${TAB}docs/x.md"; expect "bad map refused (exit 2): ${badmap//$'\n'/ }" 2 'a11y-select'
done
git -C "$R" checkout -q -- tests/a11y/spec-map.toml

# --- C. map checker -----------------------------------------------------------
chk() { ST=0; OUT=$(cd "$R" && bash scripts/a11y-select.sh --check 2>&1) || ST=$?; }
chk; expect "checker passes a consistent tree" 0 '^OK'
git -C "$R" rm -qf scripts/lib/git-plain.sh; chk; expect "a shared glob whose file was removed (stale) FAILS" 1 "stale shared glob 'scripts/lib/git-plain.sh'"
git -C "$R" reset -q --hard HEAD
git -C "$R" rm -qf internal/api/x_test.go; chk; expect "an allow glob matching nothing (stale) FAILS" 1 "stale allow glob '\*\*/\*_test.go'"
git -C "$R" reset -q --hard HEAD
printf 'broken [' >"$R/tests/a11y/spec-map.toml"; chk; expect "a malformed map is exit 2, not a pass" 2 'cannot read map'
git -C "$R" checkout -q -- tests/a11y/spec-map.toml
ST=0; OUT=$(cd "$REAL" && bash scripts/a11y-select.sh --check 2>&1) || ST=$?; expect "the real tree's map is consistent" 0 '^OK'

# --- D. the REAL map -----------------------------------------------------------
rsel() { local f="$T/rfiles"; printf 'M\t%s\n' "$@" >"$f"; ST=0; OUT=$(cd "$REAL" && bash scripts/a11y-select.sh --files-from "$f" 2>&1) || ST=$?; }
# Globs are anchored at both ends: a suffix after an allow-listed name is not allow-listed.
for p in scripts/a11y-select.sh scripts/a11y-scope.sh scripts/lib/git-plain.sh scripts/lib/run-flags.sh scripts/pre-push-gate.sh \
  web/templates/notes.md.templ internal/x_test.go.orig LICENSE.go tests/a11y/contrast.spec.js tests/a11y/README.md \
  web/templates/layout.templ web/static/css/styles.css internal/api/router.go internal/i18n/locales/en.json go.mod Makefile playwright.config.js; do
  rsel "$p"; expect "real map: $p forces FULL" 0 '^MODE=FULL$'
done
rsel docs/x.md scripts/foo.sh internal/api/x_test.go .github/workflows/x.yml README.md; expect "real map: docs, scripts, CI and Go tests only is EMPTY" 0 '^MODE=EMPTY$'

# --- E. wiring: the gate and CI both run the checker and this test ------------
GATE="$REAL/scripts/pre-push-gate.sh"; YML="$REAL/.github/workflows/gate.yml"
for want in "a11y-select.sh\" --check" "test-a11y-select.sh\""; do
  if grep -qE "^[[:space:]]*bash \"\\\$SCRIPT_DIR/${want}" "$GATE"; then ok "pre-push-gate.sh runs ${want%\"*}"; else bad "pre-push-gate.sh runs ${want%\"*} (an uncommented line)"; fi
done
for want in "a11y-select.sh --check" "test-a11y-select.sh"; do
  if grep -qE "^[[:space:]]*run: bash scripts/${want}\$" "$YML"; then ok "gate.yml runs $want"; else bad "gate.yml runs $want (an uncommented run: line)"; fi
done

# --- F. the gate's RUN_A11Y=changed arm, EXECUTED with a stub make -------------
# The block is cut out of the REAL pre-push-gate.sh by markers (not line numbers)
# and run in a fixture repo, so a regression in any arm is observable.
H="$T/harness.sh"; STUB="$T/bin"; mkdir -p "$STUB" "$T/stamp"
# shellcheck disable=SC2016  # literal text for a generated script; it must not expand here
printf '#!/usr/bin/env bash\nprintf "%%s\\n" "$*" >>"$STUB_LOG"\nexit "${STUB_MAKE_RC:-0}"\n' >"$STUB/make"; chmod +x "$STUB/make"
# `|| true`: under set -e a failed grep would end this script with no message.
RESOLVE_LINE=$(grep '^resolve_run_flag RUN_A11Y' "$GATE" || true)
# shellcheck disable=SC2016  # literal text for a generated script; it must not expand here
BASE_LINE=$(grep '^BASE=\$(git merge-base' "$GATE" || true)
A11Y_BLOCK=$(sed -n '/^run_a11y=0$/,/^gate_step "UI-preference coverage/p' "$GATE" | sed '$d')
if [ -z "$RESOLVE_LINE" ] || [ -z "$BASE_LINE" ] || ! grep -q 'a11y-select.sh' <<<"$A11Y_BLOCK"; then
  bad "gate markers still locate the a11y block (update this test's markers)"; exit 1
fi
{
  echo 'set -euo pipefail'
  # shellcheck disable=SC2016  # literal text for a generated script; it must not expand here
  echo 'cd "$REPO"; SCRIPT_DIR="$PWD/scripts"; export GATE_STAMP_DIR="$STAMPDIR"'
  # shellcheck disable=SC2016  # literal text for a generated script; it must not expand here
  echo '. "$SCRIPT_DIR/lib/gate-stamp.sh"; . "$SCRIPT_DIR/lib/run-flags.sh"; . "$SCRIPT_DIR/lib/git-plain.sh"'
  echo 'RACE_MODE=default VULN_MODE=default PROVIDER_SMOKE_MODE=default'
  printf '%s\n%s\n' "$RESOLVE_LINE" "$BASE_LINE"
  echo 'gate_stamp_begin'
  printf '%s\n' "$A11Y_BLOCK"
  # shellcheck disable=SC2016  # literal text for a generated script; it must not expand here
  echo 'gate_stamp_write "$BASE" "$RACE_MODE" "$VULN_MODE" "$PROVIDER_SMOKE_MODE" "$A11Y_MODE"'
} >"$H"
mk "$T/g"; G="$T/g"; n=0
# run_h [make-rc]: run the harness on G's current branch with RUN_A11Y=changed.
# The child must not inherit a variable that withholds the stamp (GATE_ENV_DENY
# in gate-stamp.sh, globs like SKIP_* included): the list is derived from it.
# shellcheck source=scripts/lib/gate-stamp.sh
. "$REAL/scripts/lib/gate-stamp.sh"
run_h() {
  local deny=(-u SPEC -u PROJECT) name
  while IFS='|' read -r name _; do deny+=(-u "$name"); done < <(gate_env_hits)
  : >"$T/make.log"; rm -f "$T/stamp/pre-push-gate-stamp.json"
  RC=0; OUT=$(env "${deny[@]}" PATH="$STUB:$PATH" REPO="$G" STAMPDIR="$T/stamp" STUB_LOG="$T/make.log" STUB_MAKE_RC="${1:-0}" RUN_A11Y=changed bash "$H" 2>&1) || RC=$?
  MAKE=$(cat "$T/make.log"); STAMPMODE=$(jq -r '.modes.a11y // "none"' "$T/stamp/pre-push-gate-stamp.json" 2>/dev/null || echo none)
}
# gate <file> <content> [make-rc]: a committed change on a fresh branch off main, then the block.
gate() {
  n=$((n + 1))
  git -C "$G" checkout -q -b "c$n" main
  mkdir -p "$(dirname "$G/$1")"; printf '%s\n' "$2" >>"$G/$1"
  git -C "$G" add -A; git -C "$G" "${GC[@]}" commit -q -m "c$n"
  # A second, allow-listed commit on top: a HEAD~1 diff would see only this one.
  echo more >>"$G/docs/x.md"; git -C "$G" add -A; git -C "$G" "${GC[@]}" commit -q -m "c$n-docs"
  run_h "${3:-}"
  git -C "$G" checkout -q main
}
gate web/templates/page.templ x
if [ "$RC" -eq 0 ] && [ "$MAKE" = "test-a11y SPEC= PROJECT=" ] && [ "$STAMPMODE" = changed ] && grep -q 'mode=FULL' <<<"$OUT"; then
  ok "gate FULL: diffs from the merge base (not HEAD~1), runs make with explicit SPEC= PROJECT=, stamp a11y=changed"
else bad "gate FULL (rc=$RC make=[$MAKE] stamp=$STAMPMODE)" "$OUT"; fi
gate docs/y.md x
if [ "$RC" -eq 0 ] && [ -z "$MAKE" ] && [ "$STAMPMODE" = changed ] && grep -q 'a11y: skipped -- RUN_A11Y=changed' <<<"$OUT" && ! grep -qx 'OK' <<<"$OUT"; then
  ok "gate EMPTY: make never runs, prints a SKIP with the reason and no OK line"
else bad "gate EMPTY (rc=$RC make=[$MAKE] stamp=$STAMPMODE)" "$OUT"; fi
gate web/templates/page.templ y 3
if [ "$RC" -eq 1 ] && grep -q 'reported failures' <<<"$OUT"; then ok "gate: a failing make exits 1"; else bad "gate failing make (rc=$RC)" "$OUT"; fi
gate tests/a11y/spec-map.toml 'not toml ['
if [ "$RC" -eq 2 ] && [ -z "$MAKE" ]; then ok "gate: a selector or map error exits 2 and runs nothing"; else bad "gate bad map (rc=$RC make=[$MAKE])" "$OUT"; fi
n=$((n + 1)); git -C "$G" checkout -q -b "c$n" main
printf '#!/usr/bin/env bash\necho MODE=PARTIAL\n' >"$G/scripts/a11y-select.sh"; git -C "$G" add -A; git -C "$G" "${GC[@]}" commit -q -m "c$n"
run_h; git -C "$G" checkout -q main
if [ "$RC" -eq 1 ] && [ -z "$MAKE" ] && [ "$STAMPMODE" = none ] && grep -q "unknown mode 'PARTIAL'" <<<"$OUT"; then ok "gate: an unknown selector mode exits 1, runs nothing, writes no stamp"; else bad "gate unknown mode (rc=$RC make=[$MAKE] stamp=$STAMPMODE)" "$OUT"; fi
git -C "$G" branch -q -m main gone
git -C "$G" checkout -q -b nomain gone; echo z >>"$G/web/templates/page.templ"; git -C "$G" add -A; git -C "$G" "${GC[@]}" commit -q -m nm
run_h
if [ "$RC" -eq 2 ] && grep -q "needs a local 'main'" <<<"$OUT" && [ -z "$MAKE" ]; then ok "gate: no local main blocks with exit 2 instead of diffing HEAD~1"; else bad "gate without main (rc=$RC make=[$MAKE])" "$OUT"; fi
git -C "$G" checkout -q --orphan unrel; git -C "$G" rm -rfq . ; git -C "$G" "${GC[@]}" commit -q --allow-empty -m root; git -C "$G" branch -q -m main
git -C "$G" checkout -q nomain
run_h
if [ "$RC" -eq 2 ] && grep -q 'merge base' <<<"$OUT" && [ -z "$MAKE" ]; then ok "gate: a main with no merge base blocks with exit 2 instead of diffing HEAD~1"; else bad "gate with unrelated main (rc=$RC make=[$MAKE])" "$OUT"; fi
exit "$rc"
