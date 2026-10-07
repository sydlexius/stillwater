#!/usr/bin/env bash
# test-gate-timing.sh -- guards for the pre-push gate's per-step timing and for
# the single execution of test-check-plain-git-diff.sh.
#   1. scripts/lib/gate-timing.sh prints a per-step elapsed line and one summary
#      table, on success AND from an EXIT trap on failure, without changing the
#      exit status, and runs under /bin/bash (3.2 on macOS).
#   2. every step header in pre-push-gate.sh goes through gate_step, so no step
#      can be missing from the table.
#   3. test-check-plain-git-diff.sh runs exactly once per gate: not as its own
#      gate step, and enforced (exit 0) inside test-git-clean-env.sh instead.
set -uo pipefail
ROOT=$(cd "$(dirname "$0")/.." && pwd)
GATE="${GATE:-$ROOT/scripts/pre-push-gate.sh}"
LIB="${LIB:-$ROOT/scripts/lib/gate-timing.sh}"
CLEAN_ENV="${CLEAN_ENV:-$ROOT/scripts/test-git-clean-env.sh}"
pass=0; fail=0
ok()  { pass=$((pass + 1)); echo "  PASS  $1"; }
bad() { fail=$((fail + 1)); echo "  FAIL  $1"; }
check() { local d=$1; shift; if "$@"; then ok "$d"; else bad "$d"; fi; }

BASH_BIN=/bin/bash; [ -x "$BASH_BIN" ] || BASH_BIN=bash

# 1a. success path: two steps, summary printed once, names present.
OUT=$("$BASH_BIN" -c '. "$1"; gate_step "alpha"; gate_step "beta"; gate_timing_summary; gate_timing_summary' _ "$LIB" 2>&1)
check "step headers still print" grep -q '^=== alpha ===$' <<<"$OUT"
check "each step reports elapsed seconds" test "$(grep -c '\[step took [0-9]*s: ' <<<"$OUT")" -eq 2
check "summary table lists every step" bash -c 'grep -Eq "^ +[0-9]+s  alpha$" <<<"$1" && grep -Eq "^ +[0-9]+s  beta$" <<<"$1"' _ "$OUT"
check "summary prints exactly once" test "$(grep -c '^=== Gate step timing ===$' <<<"$OUT")" -eq 1

# 1b. failure path: EXIT trap prints the table and the exit status survives.
OUT=$("$BASH_BIN" -c '. "$1"; trap "gate_timing_summary" EXIT; gate_step "gamma"; exit 7' _ "$LIB" 2>&1); RC=$?
check "failing run keeps its exit status (7)" test "$RC" -eq 7
check "failing run still prints the summary" grep -Eq '^ +[0-9]+s  gamma$' <<<"$OUT"

# 2. no raw `echo "=== ..."` header left in the gate, and the gate is wired up.
check "gate has no header that bypasses gate_step" test "$(grep -c '^echo "=== ' "$GATE")" -eq 0
check "gate sources the timing lib" grep -q 'lib/gate-timing.sh' "$GATE"
check "gate prints the summary before its success banner" \
    bash -c 'awk "/^gate_timing_summary\$/{s=NR} /^echo \"All hard checks passed/{b=NR} END{exit !(s && b && s<b)}" "$1"' _ "$GATE"
check "gate prints the summary from its EXIT cleanup" \
    bash -c 'sed -n "/^cleanup() {/,/^}/p" "$1" | grep -q gate_timing_summary' _ "$GATE"

# 3. single execution of the plain-git-diff suite.
check "gate does not run test-check-plain-git-diff.sh as its own step" \
    test "$(grep -v '^[[:space:]]*#' "$GATE" | grep -c 'test-check-plain-git-diff\.sh')" -eq 0
check "test-git-clean-env.sh still covers it" grep -q '^    test-check-plain-git-diff\.sh$' "$CLEAN_ENV"
check "test-git-clean-env.sh enforces its exit status" grep -q '^FULL_VERDICT_HELPERS="test-check-plain-git-diff\.sh"' "$CLEAN_ENV"

echo "=== RESULTS: $pass passed, $fail failed ==="
[ "$fail" -eq 0 ]
