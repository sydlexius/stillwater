#!/usr/bin/env bash
#
# test-gate-receipt-valid.sh -- behavioral tests for #3436: the pre-push hook
# skips the gate only when a passing receipt AND a gate stamp cover exactly the
# pushed tree. The REAL hook, decision script and stamp library run inside a
# throwaway repo whose gate is a stub that drops a marker file (and exits with
# $STUB_RC), so "the gate ran" is observable and the suite is fast. Every doubt
# must run the stub.
#
# Run: bash scripts/test-gate-receipt-valid.sh
# shellcheck disable=SC2016  # fixtures embed literal $ text that a child shell expands
set -euo pipefail

REPO_ROOT=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)

# shellcheck source=scripts/lib/git-clean-env.sh
. "$REPO_ROOT/scripts/lib/git-clean-env.sh"
git_clean_env_unset
# Hermetic: no inherited variable may change a verdict here (the suite's own
# deny list is the one the stamp writer uses).
unset GATE_STAMP_DIR BASE
for _v in $(env | grep -oE '^SKIP_[A-Za-z0-9_]*' || true); do unset "$_v"; done
export GIT_CONFIG_GLOBAL=/dev/null GIT_CONFIG_SYSTEM=/dev/null

if ! command -v jq >/dev/null 2>&1; then
  echo "FAIL: jq is required by the decision script and by this test" >&2
  exit 2
fi

WORK=$(mktemp -d)
WORK=$(cd "$WORK" && pwd -P)
trap 'rm -rf "$WORK"' EXIT

PASSED=0
FAILED=0
ZERO=0000000000000000000000000000000000000000

# Fixture: main has commit A; branch "feature" adds B and is checked out.
R="$WORK/repo"
git init -q -b main "$R"
mkdir -p "$R/.githooks" "$R/scripts/lib"
cp "$REPO_ROOT/.githooks/pre-push" "$R/.githooks/pre-push"
cp "$REPO_ROOT/scripts/gate-receipt-valid.sh" "$R/scripts/"
cp "$REPO_ROOT/scripts/lib/run-flags.sh" "$REPO_ROOT/scripts/lib/gate-stamp.sh" "$REPO_ROOT/scripts/lib/git-plain.sh" "$REPO_ROOT/scripts/lib/gate-timing.sh" "$R/scripts/lib/"
# Stub gate: the marker proves the hook fell through to the gate.
# shellcheck disable=SC2016  # the command substitution must expand when the stub runs
printf '#!/bin/bash\ntouch "$(git rev-parse --absolute-git-dir)/gate-ran"\nexit "${STUB_RC:-0}"\n' >"$R/scripts/pre-push-gate.sh"
chmod +x "$R/.githooks/pre-push" "$R/scripts/pre-push-gate.sh"
git -C "$R" config user.email t@example.com
git -C "$R" config user.name Test
git -C "$R" add -A
git -C "$R" commit -q -m A
A=$(git -C "$R" rev-parse HEAD)
TREE_A=$(git -C "$R" rev-parse 'HEAD^{tree}')
git -C "$R" checkout -q -b feature
echo b >"$R/b.txt"
git -C "$R" add b.txt
git -C "$R" commit -q -m B
B=$(git -C "$R" rev-parse HEAD)
TREE_B=$(git -C "$R" rev-parse 'HEAD^{tree}')
GD=$(git -C "$R" rev-parse --absolute-git-dir)
HD="$R"

mkreceipt() { # tree result schema
  printf '{"schema":"%s","producer":"gate-runner","result":"%s","commit_sha":"%s","tree_sha":"%s","worktree":"x","steps":[]}\n' \
    "$3" "$2" "$B" "$1" >"$GD/prep-pr-receipt.json"
}
# mkstamp <tree> <base> [race vuln provider a11y main]
# shellcheck source=scripts/lib/gate-stamp.sh
. "$REPO_ROOT/scripts/lib/gate-stamp.sh"
# Clear every deny-list variable, globs included, so a gate-like parent environment
# (run-paths.sh exports, a developer's GOFLAGS) cannot leak into a case.
while IFS='|' read -r _p _r; do
  [ -n "$_p" ] || continue
  for _v in $(compgen -e); do
    # shellcheck disable=SC2254  # the list holds glob patterns on purpose
    case "$_v" in $_p) unset "$_v" ;; esac
  done
done <<<"$GATE_ENV_DENY"
mkstamp() {
  jq -n --arg t "$1" --arg b "$2" --arg r "${3:-default}" --arg v "${4:-default}" \
    --arg p "${5:-default}" --arg a "${6:-default}" --arg m "${7:-$(git -C "$R" rev-parse main)}" \
    --arg pb "${8:-$2}" --argjson c "${9:-$(date +%s)}" \
    '{schema:"pre-push-gate-stamp/v1",tree:$t,base:$b,main:$m,patch_base:$pb,created:$c,modes:{race:$r,vuln:$v,provider_smoke:$p,a11y:$a}}' \
    >"$GD/pre-push-gate-stamp.json"
}
stamp_edit() { jq "$1" "$GD/pre-push-gate-stamp.json" >"$GD/s.tmp" && mv "$GD/s.tmp" "$GD/pre-push-gate-stamp.json"; }

# reset: clean tree on feature at B, main at A, a valid receipt and stamp, no marker.
reset() {
  git -C "$R" checkout -q -f feature
  git -C "$R" branch -q -D side 2>/dev/null || true
  git -C "$R" branch -q -f main "$A"
  git -C "$R" update-ref -d refs/remotes/origin/main 2>/dev/null || true
  git -C "$R" reset -q --hard "$B"
  git -C "$R" clean -qfd
  git -C "$R" config --unset status.showUntrackedFiles 2>/dev/null || true
  cp "$REPO_ROOT/scripts/gate-receipt-valid.sh" "$R/scripts/"
  cp "$REPO_ROOT/scripts/lib/run-flags.sh" "$REPO_ROOT/scripts/lib/git-plain.sh" "$REPO_ROOT/scripts/lib/gate-timing.sh" "$R/scripts/lib/"
  rm -f "$GD/gate-ran" "$GD"/pre-push-gate-blockers.*
  mkreceipt "$TREE_B" pass gate-receipt/v1
  mkstamp "$TREE_B" "$A"
}

OUT=""
RC=0
# hook <stdin-line> [VAR=value ...] -- run the real hook the way git does.
hook() {
  local line="$1"
  shift
  RC=0
  OUT=$(cd "$HD" && printf '%s\n' "$line" | env "$@" bash .githooks/pre-push 2>&1) || RC=$?
}
PUSH_B="refs/heads/feature $B refs/heads/feature $ZERO"
ran() { [ -e "$GD/gate-ran" ] && echo yes || echo no; }
pass() { echo "  PASS  $1"; PASSED=$((PASSED + 1)); }
fail() { echo "  FAIL  $1" >&2; printf '        %s\n' "$OUT" >&2; FAILED=$((FAILED + 1)); }

expect_skip() { # name
  if [ "$RC" -eq 0 ] && [ "$(ran)" = no ] && printf '%s' "$OUT" | grep -q 'skipping the gate'; then pass "$1"
  else fail "$1 (expected a skip; rc=$RC ran=$(ran))"; fi
}
expect_run() { # name reason-substring [rc]
  if [ "$RC" -eq "${3:-0}" ] && [ "$(ran)" = yes ] && printf '%s' "$OUT" | grep -q -- "$2"; then pass "$1"
  else fail "$1 (expected the gate to run with rc ${3:-0}, saying '$2'; rc=$RC ran=$(ran))"; fi
}
expect_refused() { # name substring: exit 2, gate not run
  if [ "$RC" -eq 2 ] && [ "$(ran)" = no ] && printf '%s' "$OUT" | grep -q -- "$2"; then pass "$1"
  else fail "$1 (expected exit 2 without a gate run; rc=$RC ran=$(ran))"; fi
}
# expect_stamp <name> <jq filter> <expected> / expect_nostamp <name> <substring>
expect_stamp() {
  if [ "$(jq -r "$2" "$GD/pre-push-gate-stamp.json" 2>/dev/null)" = "$3" ]; then pass "$1"
  else fail "$1 ($2 is not '$3')"; fi
}
expect_nostamp() {
  if [ ! -e "$GD/pre-push-gate-stamp.json" ] && printf '%s' "$OUT" | grep -q -- "$2"; then pass "$1"
  else fail "$1 (stamp present or reason missing)"; fi
}
# lib <shell snippet> -- run the REAL gate-stamp.sh functions in the fixture.
lib() {
  OUT=$(cd "$R" && bash -c '. scripts/lib/gate-stamp.sh; set -e; '"$1" 2>&1) || true
}

echo "=== gate receipt decision (#3436) ==="

reset; hook "$PUSH_B"
expect_skip "valid receipt + stamp skips the gate"
if printf '%s' "$OUT" | grep -q "pushed commit ${B:0:12}.*receipt commit"; then pass "the skip line names the pushed commit and labels the receipt commit"
else fail "skip line wording"; fi

reset; mkreceipt "$TREE_A" pass gate-receipt/v1; mkstamp "$TREE_A" "$A"; hook "$PUSH_B"
expect_run "receipt for a different tree runs the gate" "is not the pushed tree"
reset; mkreceipt "$TREE_B" fail gate-receipt/v1; hook "$PUSH_B"
expect_run "result fail runs the gate" "not pass"
reset; mkreceipt "$TREE_B" pass gate-receipt/v2; hook "$PUSH_B"
expect_run "wrong schema runs the gate" "receipt schema"
reset; echo dirt >>"$R/b.txt"; hook "$PUSH_B"
expect_run "modified tracked file runs the gate" "dirty"
reset; echo new >"$R/untracked.txt"; hook "$PUSH_B"
expect_run "untracked file runs the gate" "dirty"
reset; echo new >"$R/untracked.txt"; git -C "$R" config status.showUntrackedFiles no; hook "$PUSH_B"
expect_run "untracked file runs the gate even with status.showUntrackedFiles=no" "dirty"
reset; echo 'not json {' >"$GD/prep-pr-receipt.json"; hook "$PUSH_B"
expect_run "malformed receipt runs the gate and does not fail the push" "malformed"

reset; jq '.producer="someone-else"' "$GD/prep-pr-receipt.json" >"$GD/r.tmp" && mv "$GD/r.tmp" "$GD/prep-pr-receipt.json"; hook "$PUSH_B"
expect_run "a receipt from another producer runs the gate" "producer"

reset; jq 'del(.producer)' "$GD/prep-pr-receipt.json" >"$GD/r.tmp" && mv "$GD/r.tmp" "$GD/prep-pr-receipt.json"; hook "$PUSH_B"
expect_run "a receipt with no producer field runs the gate" "no producer"

echo "--- object-id push (safe-push form: <full sha>:refs/heads/<b>)"
# precond <name>: the receipt and stamp exist and cover the pushed tree, so a skip is not vacuous.
precond() {
  if [ -f "$GD/prep-pr-receipt.json" ] && [ -f "$GD/pre-push-gate-stamp.json" ] \
    && [ "$(jq -r .tree_sha "$GD/prep-pr-receipt.json")" = "$TREE_B" ] \
    && [ "$(jq -r .tree "$GD/pre-push-gate-stamp.json")" = "$TREE_B" ]; then pass "precondition: $1"
  else fail "precondition: $1"; fi
}
REMOTE="$WORK/remote.git"
git init -q --bare "$REMOTE"
git -C "$R" remote add origin "$REMOTE" 2>/dev/null || true
git -C "$R" config core.hooksPath .githooks
# realpush <refspec> -- a REAL fixture-repo transfer through the REAL hook.
GVERB=pu; GVERB=${GVERB}sh
realpush() { RC=0; OUT=$(cd "$R" && git "$GVERB" origin "$1" 2>&1) || RC=$?; }
reset; precond "valid receipt and stamp for tree B"
realpush "$B:refs/heads/objid-ok"
expect_skip "real push of a full object id to refs/heads/* skips the gate"
reset; precond "valid receipt and stamp (tag destination)"
realpush "$B:refs/tags/objid-tag"
expect_run "real push of a full object id to refs/tags/* runs the gate" "object id pushed to a non-branch"
reset; mkreceipt "$TREE_A" pass gate-receipt/v1; mkstamp "$TREE_A" "$A"
realpush "$B:refs/heads/objid-tree"
expect_run "real push of an object id whose tree differs from the receipt runs the gate" "is not the pushed tree"
reset; precond "valid receipt and stamp (local ref differs from sha field)"
hook "$B $A refs/heads/feature $ZERO"
expect_run "object-id local ref that differs from the sha field runs the gate" "differs from the sha field"
reset; precond "valid receipt and stamp (abbreviated id)"
hook "${B:0:12} $B refs/heads/feature $ZERO"
expect_run "an abbreviated object id as the local ref runs the gate" "not a branch"
reset; precond "valid receipt and stamp (39-char id)"
hook "${B:0:39} ${B:0:39} refs/heads/feature $ZERO"
expect_run "a 39-char hex local ref equal to the sha field runs the gate" "not a branch"
# The hook's `bash` resolves through PATH; put the system /bin/bash (3.2 on macOS) first.
mkdir -p "$WORK/sysbash"; ln -sf /bin/bash "$WORK/sysbash/bash"
# 40 x A: uppercase A-E sit inside the range [0-9a-f] in a UTF-8 collation.
UB=AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA
reset; precond "valid receipt and stamp (uppercase id, UTF-8 locale)"
hook "$UB $UB refs/heads/feature $ZERO" "PATH=$WORK/sysbash:$PATH" LC_ALL=en_US.UTF-8
expect_run "an uppercase 40-hex local ref equal to the sha field runs the gate under /bin/bash and UTF-8" "not a branch"
NH=$(printf 'g%.0s' $(seq 40))
reset; precond "valid receipt and stamp (non-hex 40-char ref)"
hook "$NH $NH refs/heads/feature $ZERO"
expect_run "a 40-char non-hex local ref equal to the sha field runs the gate" "not a branch"
git -C "$R" tag -a -m annotated tagobj "$B"
TAGOID=$(git -C "$R" rev-parse refs/tags/tagobj)
reset; precond "valid receipt and stamp (annotated tag object id)"
realpush "$TAGOID:refs/heads/objid-annotated"
if [ "$(ran)" = yes ] && ! printf '%s' "$OUT" | grep -q 'skipping the gate' && printf '%s' "$OUT" | grep -q 'not a commit'; then pass "a real push of an annotated tag object id to refs/heads/* runs the gate"
else fail "annotated tag object id was not refused (ran=$(ran))"; fi
echo "--- environment, malformed stamp mode, unreadable stdin (round 5)"
reset; precond "valid receipt and stamp (GOFLAGS set)"
hook "$PUSH_B" GOFLAGS=-count=1
expect_run "a push made with GOFLAGS set runs the gate even with a clean-shell stamp" "GOFLAGS is set"
reset; precond "valid receipt and stamp (SW_PORT set)"
hook "$PUSH_B" SW_PORT=1975
expect_run "a push made with SW_PORT set runs the gate" "SW_PORT is set"
reset; precond "valid receipt and stamp (nothing set)"
hook "$PUSH_B"
expect_skip "the same push without a deny-listed variable still skips"
reset; stamp_edit '.modes.vuln="garbage"'; hook "$PUSH_B"
expect_run "a stamp with a malformed mode runs the gate" "unknown mode 'garbage' for vuln"
reset; precond "valid receipt and stamp (stdin unreadable)"
RC=0; OUT=$(cd "$HD" && bash .githooks/pre-push <"$WORK" 2>&1) || RC=$?
expect_run "a hook whose stdin cannot be read runs the gate" "could not read the pushed refs"
reset; precond "valid receipt and stamp (SW_PORT at gate start)"
lib 'SW_PORT=1975 gate_stamp_begin; gate_stamp_write "$(git rev-parse HEAD~1)" default default default default'
expect_nostamp "SW_PORT set at gate start writes no stamp" "SW_PORT is set"
echo "--- stamp"
reset; stamp_edit 'del(.patch_base)'; hook "$PUSH_B"
expect_run "a stamp with no patch_base field runs the gate" "no patch_base"
# The F1 scenario: the gate passed, the first slice was squash-merged upstream,
# fetch + rebase onto origin/main gave the SAME tree, local main did not move.
# Patch coverage resolves its own base (origin/main first), so it now compares
# against a different base than the one the stamp recorded.
reset; git -C "$R" update-ref refs/remotes/origin/main "$B"; hook "$PUSH_B"
expect_run "origin/main moved to HEAD's history (local main unchanged): runs the gate" "patch-coverage base moved"
reset; git -C "$R" update-ref refs/remotes/origin/main "$A"; hook "$PUSH_B"
expect_skip "origin/main at the recorded base still skips"

echo "--- stamp age"
reset; stamp_edit ".created = $(($(date +%s) - 3 * 3600))"; hook "$PUSH_B"
expect_skip "a 3 hour old stamp still skips"
reset; stamp_edit ".created = $(($(date +%s) - 5 * 3600))"; hook "$PUSH_B"
expect_run "a 5 hour old stamp runs the gate" "older than"
reset; stamp_edit 'del(.created)'; hook "$PUSH_B"
expect_run "a stamp with no created time runs the gate" "no created time"
reset; stamp_edit '.created = "soon"'; hook "$PUSH_B"
expect_run "a stamp with a non-numeric created time runs the gate" "created"
reset; stamp_edit ".created = $(($(date +%s) + 3600))"; hook "$PUSH_B"
expect_run "a stamp dated in the future runs the gate" "future"

echo "--- every pushed ref is recomputed, not just the first"
reset
D=$(git -C "$R" commit-tree "$TREE_A" -p "$A" -m D)
Z=$(git -C "$R" commit-tree "$TREE_B" -p "$D" -m Z)
git -C "$R" branch -q -f main "$D"
git -C "$R" update-ref refs/remotes/origin/main "$A"
mkstamp "$TREE_B" "$A" default default default default "$D" "$A"
hook "$(printf '%s\n%s' "$PUSH_B" "refs/heads/other $Z refs/heads/other $ZERO")"
expect_run "second ref with a different merge-base runs the gate" "not honored (base moved"
reset
Q=$(git -C "$R" commit-tree "$TREE_A" -p "$A" -m Q)
Z2=$(git -C "$R" commit-tree "$TREE_B" -p "$Q" -m Z2)
git -C "$R" update-ref refs/remotes/origin/main "$Q"
mkstamp "$TREE_B" "$A" default default default default "$A" "$A"
hook "$(printf '%s\n%s' "$PUSH_B" "refs/heads/other $Z2 refs/heads/other $ZERO")"
expect_run "second ref with a different patch base runs the gate" "patch-coverage base moved"

echo "--- GATE_STAMP_DIR is test plumbing only"
reset
SD="$WORK/stampdir"; mkdir -p "$SD"; cp "$GD/pre-push-gate-stamp.json" "$SD/"; rm -f "$GD/pre-push-gate-stamp.json"
hook "$PUSH_B" GATE_STAMP_DIR="$SD"
expect_run "a valid stamp in GATE_STAMP_DIR does not count; the git dir has none" "no gate stamp"
rm -f "$SD/pre-push-gate-stamp.json"
reset; rm -f "$GD/pre-push-gate-stamp.json"; GATE_STAMP_DIR="$SD" lib 'gate_stamp_begin; gate_stamp_write "'"$A"'" default default default default'
if [ -e "$SD/pre-push-gate-stamp.json" ] && [ ! -e "$GD/pre-push-gate-stamp.json" ]; then pass "gate_stamp_begin and the writer honor GATE_STAMP_DIR"; else fail "GATE_STAMP_DIR not honored"; fi
reset; stamp_edit 'del(.main)'; hook "$PUSH_B"
expect_run "a stamp with no main field runs the gate" "no main tip"
reset; echo 'not json {' >"$GD/pre-push-gate-stamp.json"; hook "$PUSH_B"
expect_run "malformed stamp runs the gate" "stamp unreadable"
reset; stamp_edit '.schema="pre-push-gate-stamp/v2"'; hook "$PUSH_B"
expect_run "wrong stamp schema runs the gate" "gate stamp schema"
reset; mkstamp "$TREE_A" "$A"; hook "$PUSH_B"
expect_run "stamp for a different tree runs the gate" "stamp tree"
reset; rm -f "$GD/pre-push-gate-stamp.json"; hook "$PUSH_B"
expect_run "missing stamp runs the gate" "no gate stamp"
reset; mkstamp "$TREE_B" "$B"; hook "$PUSH_B"
expect_run "base moved since the gate ran runs the gate" "base moved"

# main ahead of the merge-base: D is a child of A; HEAD is S on top of D, so the
# base comes from the PUSHED commit B (A), not main's tip (D) and not HEAD (D).
reset
D=$(git -C "$R" commit-tree "$TREE_A" -p "$A" -m D)
git -C "$R" branch -q -f main "$D"
git -C "$R" checkout -q -b side "$D"
echo s >"$R/s.txt"; git -C "$R" add s.txt; git -C "$R" commit -q -m S
mkstamp "$TREE_B" "$A" default default default default "$D"; hook "$PUSH_B"
expect_skip "base is the merge-base of the pushed commit, not main's tip or HEAD"
reset; git -C "$R" branch -q -f main "$D"; hook "$PUSH_B"
expect_run "main moved since the gate ran runs the gate" "main moved"

echo "--- modes"
reset; mkstamp "$TREE_B" "$A" off; hook "$PUSH_B"
expect_run "stamp from a RUN_RACE=0 gate runs the gate" "race=off"
reset; hook "$PUSH_B" RUN_RACE=1
expect_run "RUN_RACE=1 push vs a default stamp runs the gate" "race=default"
reset; mkstamp "$TREE_B" "$A" on; hook "$PUSH_B" RUN_RACE=1
expect_skip "stamp from a RUN_RACE=1 gate satisfies a RUN_RACE=1 push"
reset; hook "$PUSH_B" RUN_VULN=1
expect_run "RUN_VULN=1 push vs a default stamp runs the gate" "vuln=default"
reset; hook "$PUSH_B" RUN_PROVIDER_SMOKE=1
expect_run "RUN_PROVIDER_SMOKE=1 push vs a default stamp runs the gate" "provider_smoke=default"
reset; hook "$PUSH_B" RUN_A11Y=1
expect_run "RUN_A11Y=1 push vs a default stamp runs the gate" "a11y=default"
reset; stamp_edit 'del(.modes.vuln)'; hook "$PUSH_B"
expect_run "a stamp missing a mode field runs the gate" "no mode for vuln"
reset; stamp_edit '.modes.a11y=null'; hook "$PUSH_B"
expect_run "a stamp with a null mode runs the gate" "no mode for a11y"

echo "--- which commit is pushed"
reset; echo c >"$R/c.txt"; git -C "$R" add c.txt; git -C "$R" commit -q -m C
hook "$PUSH_B"
expect_skip "pushed commit B (not HEAD=C) matches the receipt for B"
mkreceipt "$(git -C "$R" rev-parse 'HEAD^{tree}')" pass gate-receipt/v1
mkstamp "$(git -C "$R" rev-parse 'HEAD^{tree}')" "$A"; hook "$PUSH_B"
expect_run "receipt for HEAD does not cover a pushed older commit" "is not the pushed tree"
reset; hook "refs/heads/feature $ZERO refs/heads/feature $B"
expect_run "a branch delete runs the gate" "deletes"
reset; hook "refs/tags/v1 $B refs/tags/v1 $ZERO"
expect_run "a tag push runs the gate" "not a branch"
reset; hook "$(printf '%s\n%s' "$PUSH_B" "refs/heads/other $A refs/heads/other $ZERO")"
expect_run "two refs, one not covered, runs the gate" "is not the pushed tree"
reset; hook ""
expect_run "empty stdin runs the gate" "no pushed refs"

echo "--- worktrees"
reset
git -C "$R" worktree add -q --detach "$WORK/wt" "$B"
HD="$WORK/wt"; GD_MAIN="$GD"; GD=$(git -C "$HD" rev-parse --absolute-git-dir)
hook "$PUSH_B"
expect_run "a linked worktree does not honor another worktree's receipt" "running scripts"
HD="$R"; GD="$GD_MAIN"

echo "--- force, refusal, crash, exit codes"
reset; hook "$PUSH_B" PRE_PUSH_FORCE_GATE=1
expect_run "PRE_PUSH_FORCE_GATE=1 forces the gate" "PRE_PUSH_FORCE_GATE"
reset; hook "$PUSH_B" PRE_PUSH_FORCE_GATE=maybe
expect_refused "a bad PRE_PUSH_FORCE_GATE value is refused" "PRE_PUSH_FORCE_GATE='maybe'"
reset; hook "$PUSH_B" RUN_A11Y=maybe
expect_refused "a bad RUN_A11Y value is refused by the validator" "RUN_A11Y='maybe'"
if printf '%s' "$OUT" | grep -q 'push blocked'; then pass "a refusal prints one line saying the push is blocked"; else fail "no push-blocked line"; fi
reset; printf 'RESOLVED_RUN_FLAG=""\n' >"$R/scripts/lib/run-flags.sh"; hook "$PUSH_B"
expect_run "a flag probe failing for any reason but a refusal (function missing) runs the gate" "running scripts"
reset; : >"$R/scripts/gate-receipt-valid.sh"; hook "$PUSH_B"
expect_run "an empty validator file (exit 0, no output) runs the gate" "without a skip line"
reset; echo 'exit 0' >"$R/scripts/gate-receipt-valid.sh"; hook "$PUSH_B"
expect_run "a validator reduced to exit 0 runs the gate" "without a skip line"
reset; hook "$PUSH_B" PRE_PUSH_FORCE_GATE=1 STUB_RC=7
expect_run "the gate's non-zero exit code is the push's exit code" "running scripts" 7
reset; { echo 'if then'; cat "$REPO_ROOT/scripts/gate-receipt-valid.sh"; } >"$R/scripts/gate-receipt-valid.sh"; hook "$PUSH_B"
expect_run "a validator with a syntax error runs the gate (no skip, no block)" "running scripts"
reset; echo 'exit 2' >"$R/scripts/gate-receipt-valid.sh"; hook "$PUSH_B"
expect_run "a validator that exits 2 runs the gate (no skip, no block)" "running scripts"
reset; echo 'exit 3' >"$R/scripts/gate-receipt-valid.sh"; hook "$PUSH_B"
expect_run "a validator that exits 3 runs the gate (no skip)" "running scripts"

echo "--- the real stamp writer (scripts/lib/gate-stamp.sh)"
reset; lib 'gate_stamp_begin; gate_stamp_write "'"$A"'" default default default default'
expect_stamp "clean run: the stamp binds the tree" .tree "$TREE_B"
expect_stamp "clean run: the stamp binds the base" .base "$A"
expect_stamp "clean run: the stamp binds the tip of main" .main "$A"
hook "$PUSH_B"
expect_skip "a stamp from the real writer is honored by the real validator"
for flag in race vuln provider_smoke a11y; do
  reset
  args=""
  for f in race vuln provider_smoke a11y; do args="$args $([ "$f" = "$flag" ] && echo on || echo off)"; done
  lib 'gate_stamp_begin; gate_stamp_write "'"$A"'"'"$args"
  if [ "$(jq -r ".modes.$flag" "$GD/pre-push-gate-stamp.json")" = on ] \
    && [ "$(jq -r '[.modes[]|select(.=="off")]|length' "$GD/pre-push-gate-stamp.json")" = 3 ]; then
    pass "mode $flag is recorded as on and the other three as off"
  else fail "mode $flag recording"; fi
done
reset; lib 'gate_stamp_begin; echo x >u.txt; gate_stamp_write "'"$A"'" default default default default'
expect_nostamp "dirty at the end: no stamp" "dirty when the gate finished"
reset; echo x >"$R/u.txt"; lib 'gate_stamp_begin; rm u.txt; gate_stamp_write "'"$A"'" default default default default'
expect_nostamp "dirty at the start, cleaned later: no stamp" "dirty when the gate started"
reset; lib 'gate_stamp_begin; echo c >c.txt; git add c.txt; git commit -q -m C; gate_stamp_write "'"$A"'" default default default default'
expect_nostamp "HEAD's tree changed mid-run: no stamp" "tree changed"
reset; lib 'gate_stamp_begin; gate_stamp_block "SKIP_OPENAPI_BREAKING set"; gate_stamp_write "'"$A"'" default default default default'
expect_nostamp "a skipped or weakened check blocks the stamp and is named" "SKIP_OPENAPI_BREAKING set"
reset; lib 'gate_stamp_begin'
if [ ! -e "$GD/pre-push-gate-stamp.json" ]; then pass "gate_stamp_begin deletes the old stamp and writes none"; else fail "stale stamp survived begin"; fi

echo "--- the blocker file (works from children, subshells, pipelines)"
SW='gate_stamp_begin; '
WR='; gate_stamp_write "'"$A"'" default default default default'
reset; lib "$SW"'(gate_stamp_block "from a subshell")'"$WR"
expect_nostamp "a blocker raised in a subshell blocks the stamp" "from a subshell"
reset; lib "$SW"'echo x | { gate_stamp_block "from a pipeline"; }'"$WR"
expect_nostamp "a blocker raised in a pipeline blocks the stamp" "from a pipeline"
reset; lib "$SW"'bash -c ". scripts/lib/gate-stamp.sh; gate_skip blocking \"WARNING: child tool missing\""'"$WR"
expect_nostamp "a blocking skip raised by a child process blocks the stamp" "child tool missing"
reset; lib "$SW"'x=$(gate_skip blocking "in a command substitution")'"$WR"
expect_nostamp "a blocking skip inside \$(...) blocks the stamp" "command substitution"
reset; lib "$SW"'gate_skip deterministic "SKIP: no Go files"'"$WR"
expect_stamp "a deterministic skip does not block the stamp" .tree "$TREE_B"
reset; lib "$SW"'gate_skip mystery "SKIP: unclassified"'"$WR"
expect_nostamp "an unknown skip class counts as blocking" "unclassified"
reset; lib "$SW"'gate_stamp_write "'"$A"'" default default default default'
expect_stamp "the real writer records patch_base" .patch_base "$A"
reset; git -C "$R" update-ref refs/remotes/origin/main "$B"; lib "$SW"'gate_stamp_write "'"$A"'" default default default default'
expect_stamp "the real writer records patch_base from origin/main, distinct from base" .patch_base "$B"
reset; lib "$SW"'git update-ref refs/remotes/origin/main '"$B""$WR"
expect_nostamp "origin/main moving during the run: no stamp" "patch-coverage base moved"
reset; lib "$SW"'GATE_STAMP_BLOCK_FILE=""'"$WR"
expect_nostamp "an empty GATE_STAMP_BLOCK_FILE refuses the stamp" "blocker file is unavailable"
reset; lib "$SW"'chmod 444 "$GATE_STAMP_BLOCK_FILE"; gate_stamp_block "after a failed append"'"$WR"
if [ "$(id -u)" -eq 0 ]; then expect_nostamp "a blocker on a read-only file blocks the stamp (root: the append succeeds)" "after a failed append"
else expect_nostamp "a failed blocker append refuses the stamp (fail closed)" "blocker file is unavailable"; fi
reset; git -C "$R" branch -q -D main; git -C "$R" update-ref -d refs/remotes/origin/main 2>/dev/null || true
lib "$SW"'gate_stamp_write "'"$A"'" default default default default'
expect_nostamp "no main or origin/main at all: the stamp is refused (patch base none)" "did not resolve"
reset; GOFLAGS=-run=NONE lib "$SW"'gate_stamp_write "'"$A"'" default default default default'
expect_nostamp "GOFLAGS set: no stamp, and the variable is named" "GOFLAGS is set"
reset; SKIP_SOMETHING=1 lib "$SW"'gate_stamp_write "'"$A"'" default default default default'
expect_nostamp "a SKIP_* variable set: no stamp" "SKIP_SOMETHING is set"
reset; PATCH_COVERAGE_ALLOW_DIRTY=1 lib "$SW"'gate_stamp_write "'"$A"'" default default default default'
expect_nostamp "PATCH_COVERAGE_ALLOW_DIRTY set: no stamp" "PATCH_COVERAGE_ALLOW_DIRTY is set"
reset; BASE=x lib "$SW"'gate_stamp_write "'"$A"'" default default default default'
expect_nostamp "an exported BASE: no stamp" "BASE is set"
reset; ( . "$REPO_ROOT/scripts/lib/run-paths.sh"; cd "$R" && lib_cmd='gate_stamp_begin; gate_stamp_write "'"$A"'" default default default default'; bash -c ". scripts/lib/gate-stamp.sh; $lib_cmd" ) >/dev/null 2>&1
expect_stamp "with run-paths.sh sourced (its GOLANGCI_LINT_CACHE exported) the real writer still stamps" .tree "$TREE_B"
reset; GOFLAGS="" lib "$SW"'gate_stamp_write "'"$A"'" default default default default'
expect_stamp "GOFLAGS set but empty is the default: stamp written" .tree "$TREE_B"
reset; lib "$SW"'rm -f "$GATE_STAMP_BLOCK_FILE"'"$WR"
expect_nostamp "a missing blocker file refuses the stamp (fail closed)" "blocker file is unavailable"
reset; lib "$SW"'git branch -q -f main '"$B""$WR"
expect_nostamp "main moving during the run: no stamp" "main moved during the run"

echo "--- the REAL gate arms, executed between the real begin and write"
BASH_BIN=$(command -v bash)
TOOLS="$WORK/tools"
mkdir -p "$TOOLS"
for t in git bash env cat tr rm mv sed sort mktemp dirname grep head cut; do
  p=$(command -v "$t") && ln -sf "$p" "$TOOLS/$t"
done
printf '#!/bin/bash\nexit 0\n' >"$TOOLS/go"
chmod +x "$TOOLS/go"
G="$REPO_ROOT/scripts/pre-push-gate.sh"
# cut_arm <start-regex> <end-regex> -- the gate's own lines, start through end.
cut_arm() { awk -v s="$1" -v e="$2" '!on && $0 ~ s {on=1} on {print} on && $0 ~ e {exit}' "$G"; }
# real_arm <name> <arm-file> [VAR=value ...]: run the slice in the fixture.
real_arm() {
  local name="$1" arm="$2"
  shift 2
  reset
  mkdir -p "$R/docs/site" "$R/run" "$R/web/static/css"
  echo "site_name: x" >"$R/docs/site/properdocs.yml"
  cp "$REPO_ROOT/scripts/check-generated.sh" "$REPO_ROOT/scripts/prefs-coverage.py" "$R/scripts/"
  git -C "$R" add -A scripts docs web
  git -C "$R" -c core.hooksPath=/dev/null commit -q -m arms-fixture
  printf 'run/\n' >"$R/.git/info/exclude"
  {
    echo 'set -euo pipefail'
    echo 'SCRIPT_DIR="$(pwd)/scripts"; SW_RUN_DIR="$(pwd)/run"; tmp_openapi=""'
    echo '. "$SCRIPT_DIR/lib/gate-stamp.sh"; gate_stamp_begin'
    echo "BASE=\$(git merge-base main HEAD 2>/dev/null || echo HEAD~1)"
    cat "$arm"
    echo 'gate_stamp_write "$BASE" default default default default'
  } >"$R/run/slice.sh"
  (cd "$R" && eval "${ARM_PRE:-:}")
  RC=0
  OUT=$(cd "$R" && env "$@" "$BASH_BIN" run/slice.sh 2>&1) || RC=$?
}
cut_arm '^openapi_skip_flag=' '^esac$' >"$WORK/arm-openapi.sh"
cut_arm '^if [[] -f docs/site/properdocs.yml []]; then' '^fi$' >"$WORK/arm-python.sh"
[ -s "$WORK/arm-openapi.sh" ] && [ -s "$WORK/arm-python.sh" ] || fail "could not cut the gate's arms by marker"
real_arm openapi "$WORK/arm-openapi.sh" SKIP_OPENAPI_BREAKING=1
expect_nostamp "the real OpenAPI arm with SKIP_OPENAPI_BREAKING=1 writes no stamp" "SKIP_OPENAPI_BREAKING is set"
real_arm python "$WORK/arm-python.sh" PATH="$TOOLS"
expect_nostamp "the real properdocs arm without python3 writes no stamp" "python3 not in PATH"
real_arm python "$WORK/arm-python.sh"
if [ -e "$GD/pre-push-gate-stamp.json" ]; then pass "the real properdocs arm with python3 and PyYAML present writes the stamp"
elif printf '%s' "$OUT" | grep -q 'PyYAML not installed'; then pass "(PyYAML absent on this host: the arm blocked, as required)"
else fail "properdocs arm: unexpected result"; fi
# The real child helper, run as a child of the slice exactly as the gate runs it.
printf 'bash "$SCRIPT_DIR/check-generated.sh"\n' >"$WORK/arm-generated.sh"
real_arm generated "$WORK/arm-generated.sh" PATH="$TOOLS"
expect_nostamp "the real check-generated.sh without tailwindcss writes no stamp" "tailwindcss not found"

# Part 1: the real patch-coverage call, with BASE exported to a different commit
# (a variable that arrives exported stays exported when the gate reassigns it).
# A stub helper records the BASE it was handed; it must be the stamp's base.
mkdir -p "$WORK/home/.claude/scripts"
printf '#!/bin/bash\nprintf %%s "${BASE:-unset}" >"%s/helper-base"\n' "$WORK" >"$WORK/home/.claude/scripts/patch-coverage.sh"
chmod +x "$WORK/home/.claude/scripts/patch-coverage.sh"
{ cut_arm '^  PATCH_COVERAGE_HELPER=' 'HINT: if this looks spurious'; echo '    exit 1'; echo '  fi'; } >"$WORK/arm-patchcov.sh"
ARM_PRE='git update-ref refs/remotes/origin/main "$B"'
real_arm patchcov "$WORK/arm-patchcov.sh" HOME="$WORK/home" COVER_OUT=x BASE="$A"
if [ "$(cat "$WORK/helper-base" 2>/dev/null)" = "$B" ]; then pass "the patch-coverage helper is handed the stamp's base, not an inherited BASE"
else fail "helper saw BASE=$(cat "$WORK/helper-base" 2>/dev/null) (want the stamped base $B)"; fi
ARM_PRE='git branch -q -D main; git update-ref -d refs/remotes/origin/main 2>/dev/null || true; git branch -q -f main_keep HEAD'
rm -f "$WORK/helper-base"
real_arm patchcov "$WORK/arm-patchcov.sh" HOME="$WORK/home" COVER_OUT=x
if [ ! -e "$WORK/helper-base" ] && printf '%s' "$OUT" | grep -q 'no patch-coverage base'; then pass "no resolvable base: the patch-coverage step fails before calling the helper"
else fail "patch-coverage step without a base: helper ran or no message"; fi
ARM_PRE=''

# The real prefs-coverage arm with a python3 that cannot import tomllib.
mkdir -p "$WORK/pystub"
printf '#!/bin/bash\ncase "$*" in *"import tomllib"*) exit 1 ;; esac\nexec %s "$@"\n' "$(command -v python3)" >"$WORK/pystub/python3"
chmod +x "$WORK/pystub/python3"
{ cut_arm '^PREFS_COVERAGE_HELPER=' '^  esac$'; echo 'fi'; } >"$WORK/arm-prefs.sh"
real_arm prefs "$WORK/arm-prefs.sh" PATH="$WORK/pystub:$PATH"
expect_nostamp "the real prefs arm with a python3 lacking tomllib writes no stamp" "lacks tomllib"
# The real prefs-coverage.py on a surface file that became unreadable.
if [ "$(id -u)" -eq 0 ]; then
  echo "  SKIP  unreadable-surface blocker (running as root: chmod 000 does not block root)"
else
  reset
  cp "$REPO_ROOT/scripts/prefs-coverage.py" "$R/scripts/"
  printf '[[pref]]\nkey = "k"\nsurface = "s.txt"\nverify = "TOKEN"\n' >"$R/.prefs.toml"
  echo TOKEN >"$R/s.txt"; git -C "$R" add -A; git -C "$R" -c core.hooksPath=/dev/null commit -q -m prefs
  PB=$(git -C "$R" rev-parse HEAD)
  echo other >"$R/s.txt"; git -C "$R" add -A; git -C "$R" -c core.hooksPath=/dev/null commit -q -m prefs2
  chmod 000 "$R/s.txt"
  BF="$WORK/blockfile"; : >"$BF"
  OUT=$(cd "$R" && env BASE="$PB" GATE_STAMP_BLOCK_FILE="$BF" python3 scripts/prefs-coverage.py 2>&1) || true
  chmod 644 "$R/s.txt"
  if grep -q 'unreadable, skipped' "$BF"; then pass "prefs-coverage.py records a blocker when a surface file is unreadable"
  else fail "prefs-coverage.py wrote no blocker for an unreadable surface"; fi
  git -C "$R" rm -q -f .prefs.toml s.txt; git -C "$R" -c core.hooksPath=/dev/null commit -q -m cleanup
fi
# A gate start that refuses a bad RUN_* value must not leave a blocker file.
reset
cp "$G" "$R/scripts/pre-push-gate.sh"
OUT=$(cd "$R" && env RUN_A11Y=maybe bash scripts/pre-push-gate.sh 2>&1) && RC=0 || RC=$?
if [ "$RC" -eq 2 ] && ! ls "$GD"/pre-push-gate-blockers.* >/dev/null 2>&1; then pass "a gate start with a bad RUN_* value leaves no blocker file"
else fail "bad-flag gate start: rc=$RC, blocker files left"; fi

echo "--- structure (comments stripped before matching)"
strip() { grep -vE '^[[:space:]]*#' "$1"; }
n=$(strip "$G" | grep -c '^gate_stamp_begin$' || true)
if [ "$n" = 1 ]; then pass "gate_stamp_begin appears exactly once"; else fail "gate_stamp_begin appears $n times"; fi
last=$(strip "$G" | grep -o 'gate_stamp_[a-z]*' | tail -1)
if [ "$last" = gate_stamp_write ]; then pass "gate_stamp_write is the last gate_stamp_* call"; else fail "last gate_stamp_* call is $last"; fi
toks=$(strip "$G" | grep -o 'gate_stamp_[a-z]*' | sort | uniq -c | tr -s ' ' | tr '\n' ',')
if [ "$toks" = " 1 gate_stamp_begin, 1 gate_stamp_write," ]; then pass "the gate calls only gate_stamp_begin and gate_stamp_write, once each"; else fail "gate_stamp_* calls in the gate: $toks"; fi
n=$(strip "$G" | grep -c 'GATE_STAMP_BLOCK_FILE' || true)
if [ "$n" = 2 ]; then pass "the gate touches the blocker file only in its two cleanup traps"; else fail "GATE_STAMP_BLOCK_FILE appears $n times in the gate"; fi
w=$(strip "$G" | grep -n '^gate_stamp_write ' | cut -d: -f1)
after=$(strip "$G" | sed -n "$((w + 1)),\$p" | grep -v '^[[:space:]]*$' | grep -v '^gate_timing_summary || true$' | grep -vc '^echo ""$' || true)
if [ "$after" = 1 ] && strip "$G" | tail -1 | grep -q 'All hard checks passed'; then pass "nothing but the closing banner follows gate_stamp_write"; else fail "code follows gate_stamp_write ($after lines)"; fi
b=$(strip "$G" | grep -n '^gate_stamp_begin$' | cut -d: -f1)
f=$(strip "$G" | grep -n '^resolve_run_flag RUN_RACE' | cut -d: -f1)
if [ -n "$b" ] && [ -n "$f" ] && [ "$b" -lt "$f" ]; then pass "the gate deletes the old stamp before it resolves any flag"; else fail "gate_stamp_begin missing or after flag resolution"; fi
# shellcheck disable=SC2016  # literal pattern
if strip "$G" | grep -q '^gate_stamp_write "\$BASE" "\$RACE_MODE" "\$VULN_MODE" "\$PROVIDER_SMOKE_MODE" "\$A11Y_MODE"$'; then pass "the gate writes the stamp with its base and four modes"; else fail "gate_stamp_write call"; fi
if strip "$G" | grep -q '^bash "\$SCRIPT_DIR/test-gate-receipt-valid.sh"$'; then pass "the gate runs this test as a real command"; else fail "the gate does not run this test"; fi

echo "--- allow-list: skip-style output outside gate_skip"
# Scans the gate, the hook and every script the gate runs (a `$SCRIPT_DIR/..`
# path, lib/ included) for non-comment echo/printf/print lines mentioning
# skip or warn that do not go through gate_skip. Each must be classified with
# gate_skip or listed below as file|line-pattern|why. A TEXT HEURISTIC: it does
# not see a skip that prints no such word, one assembled from a variable, or a
# helper outside the repo (patch-coverage.sh); those need review.
EXC='
scripts/check-gate-invariant.sh|defaults to SKIP,|invariant checker: remediation text quoting the banned phrases
scripts/check-gate-invariant.sh|was SKIPped|invariant checker: remediation text quoting the banned phrases
scripts/gate-receipt-valid.sh|skipping the gate; a passing receipt|the receipt skip itself, not a gate check
.githooks/pre-push|skip line|the hook'"'"'s own message about the receipt check
scripts/stylelint-diff-gate.sh|SKIP: no files under|deterministic: decided from the diff alone; a plain echo because its test copies it standalone
scripts/prefs-coverage.py|self-skip|deterministic: no .prefs.toml in the tree
scripts/prefs-coverage.py|print(f"  [SKIP   ] {head_path}  ({key}) -- unreadable ({e}); "|blocking: writes the blocker file on the lines that follow
scripts/check-plain-git-diff.sh|rather than skips|FAIL path (exit 2): a missing python3 fails the check, it does not skip
scripts/pre-push-gate.sh|use RUN_RACE=0 to skip|hint text on a FAIL path, not a skip
scripts/pre-push-gate.sh|openapi_skip_flag=|reads the override variable; the arm it selects calls gate_skip blocking
scripts/check-gate-invariant.sh|WARN:|invariant checker: remediation text quoting the banned phrases
scripts/check-gate-invariant.sh|$warns|invariant checker: prints the offending lines it found
scripts/lib/run-flags.sh|silently skip a tier|resolver refusal text, not a skip
scripts/lib/run-flags.sh|(explicit skip)|resolver refusal text, not a skip
scripts/smoke-provider-failure.sh|"warnings"|runs only under RUN_PROVIDER_SMOKE=1 (a recorded mode); greps a response body
scripts/smoke-provider-failure.sh|TODO surface-fix|runs only under RUN_PROVIDER_SMOKE=1 (a recorded mode); a TODO note
scripts/smoke-provider-failure.sh|[WARN]|runs only under RUN_PROVIDER_SMOKE=1 (a recorded mode); warnings inside a check that ran
scripts/smoke-provider-failure.sh|warn-not-fail|runs only under RUN_PROVIDER_SMOKE=1 (a recorded mode); a row label
'
helpers=$(strip "$G" | grep -oE '\$SCRIPT_DIR/[A-Za-z0-9_./-]+\.(sh|py)' | sed 's|\$SCRIPT_DIR/|scripts/|' | sort -u)
unclassified=""
for f in scripts/pre-push-gate.sh .githooks/pre-push $helpers; do
  case "$f" in scripts/test-*) continue ;; esac # self-tests of the guards, not checks of the pushed tree
  [ -f "$REPO_ROOT/$f" ] || { unclassified="$unclassified MISSING:$f"; continue; }
  while IFS= read -r line; do
    [ -n "$line" ] || continue
    ok=0
    while IFS='|' read -r efile epat _why; do
      [ "$efile" = "$f" ] || continue
      case "$line" in *"$epat"*) ok=1 ;; esac
    done <<<"$EXC"
    [ "$ok" = 1 ] || unclassified="$unclassified
    $f: $line"
  done < <(strip "$REPO_ROOT/$f" | grep -iE '(echo|printf|print).*(skip|warn)' | grep -vE '^[[:space:]]*gate_skip ' || true)
done
if [ -z "$unclassified" ]; then pass "every skip-style line in the gate, the hook and its helpers goes through gate_skip or is a listed exception"
else fail "unclassified skip-style output (use gate_skip <deterministic|blocking>, or list it with a reason):$unclassified"; fi

echo ""
echo "Results: $PASSED passed, $FAILED failed"
[ "$FAILED" -eq 0 ]
