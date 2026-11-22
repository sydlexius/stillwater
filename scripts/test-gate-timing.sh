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
#   4. check-git-init-guarded.sh runs before every fixture-building script
#      (#3051): its whole value is PREVENTING an unguarded fixture repository
#      from rewriting the main repo's config earlier in the same gate run.
# NOTE: also run by CI's "Gate Invariant" job (gate.yml), so --no-verify pushes are covered.
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
check "gate has no header that bypasses gate_step" test "$(grep -c '^[[:space:]]*echo .=== ' "$GATE")" -eq 0
check "gate sources the timing lib" grep -q 'lib/gate-timing.sh' "$GATE"
check "gate prints the summary before its success banner" \
    bash -c 'awk "/^gate_timing_summary/{s=NR} /^echo \"All hard checks passed/{b=NR} END{exit !(s && b && s<b)}" "$1"' _ "$GATE"
check "gate prints the summary from its EXIT cleanup" \
    bash -c 'sed -n "/^cleanup() {/,/^}/p" "$1" | grep -q gate_timing_summary' _ "$GATE"

# 3. single execution of the plain-git-diff suite.
check "gate does not run test-check-plain-git-diff.sh as its own step" \
    test "$(grep -v '^[[:space:]]*#' "$GATE" | grep -c 'test-check-plain-git-diff\.sh')" -eq 0
check "test-git-clean-env.sh still covers it" grep -q '^    test-check-plain-git-diff\.sh$' "$CLEAN_ENV"
check "clean-env full-verdict branch runs the suite without SW_PLAIN_SELFCHECK" \
    grep -qF 'OUT=$(cd "$REPO_ROOT" && env -u SW_PLAIN_SELFCHECK GIT_DIR="$GDIR" bash "scripts/$helper" 2>&1)' "$CLEAN_ENV"
check "clean-env requires the full-verdict exit to be exactly 0" \
    grep -qF 'test "$RC" -eq 0 || true' "$CLEAN_ENV"
check "test-git-clean-env.sh enforces its exit status" grep -q '^FULL_VERDICT_HELPERS="test-check-plain-git-diff\.sh"' "$CLEAN_ENV"

# 4. git-init guard ordering (#3051). "Fixture-building" is defined by the
#    checker's own --list (ONE definition of a git-init site, shared with
#    test-git-clean-env.sh), then closed TRANSITIVELY: a script whose executable
#    lines name a fixture-building script is one too, because harnesses run
#    their helpers by name (test-git-clean-env.sh's HELPERS list, the a11y
#    self-tests). Naming is coarser than running -- a script that only greps
#    another one's text joins the set -- and that over-match fails CLOSED: the
#    worst case is demanding the guard run earlier than strictly necessary.
#    Every executable gate line ABOVE the first `check-git-init-guarded.sh`
#    invocation must name no script in that set.
GUARD_RE='(^|[^A-Za-z0-9_.-])check-git-init-guarded\.sh'
LIST_OUT=$(bash "$ROOT/scripts/check-git-init-guarded.sh" --list 2>&1); LIST_RC=$?
check "the checker's --list runs" test "$LIST_RC" -eq 0
check "the --list is non-empty (an empty set would pass the ordering check vacuously)" \
    test "$(grep -c '\.sh$' <<<"$LIST_OUT")" -gt 0
check "the gate itself contains no git-init site (only the scripts it invokes are mapped)" \
    test "$(grep -c '^scripts/pre-push-gate\.sh$' <<<"$LIST_OUT")" -eq 0

# Space-delimited basename set; bash 3.2 has no associative arrays.
FIXTURE_SET=" "
for f in $LIST_OUT; do FIXTURE_SET="$FIXTURE_SET${f##*/} "; done
ALL_SCRIPTS=$(git -C "$ROOT" ls-files scripts .githooks | grep -v '^scripts/pre-push-gate\.sh$')
names_in() { grep -v '^[[:space:]]*#' "$1" 2>/dev/null | grep -oE '[A-Za-z0-9_.-]+\.sh' | sort -u; }
changed=1
while [ "$changed" -eq 1 ]; do
    changed=0
    for f in $ALL_SCRIPTS; do
        b=${f##*/}
        case "$FIXTURE_SET" in *" $b "*) continue ;; esac
        [ -f "$ROOT/$f" ] || continue
        for n in $(names_in "$ROOT/$f"); do
            case "$FIXTURE_SET" in
                *" $n "*) FIXTURE_SET="$FIXTURE_SET$b "; changed=1; break ;;
            esac
        done
    done
done

# guard_order_violations <gate> -- print each executable line above the first
# checker invocation that names a fixture-building script; "NO-GUARD" if the
# checker is never invoked, "WRONG-STEP" if its step is not the guard step.
guard_order_violations() {
    local gate=$1 anchor step n
    anchor=$(grep -nE "$GUARD_RE" "$gate" | grep -vE '^[0-9]+:[[:space:]]*#' | head -1 | cut -d: -f1)
    if [ -z "$anchor" ]; then echo "NO-GUARD"; return; fi
    step=$(head -n "$anchor" "$gate" | grep -E '^gate_step ' | tail -1)
    case "$step" in *'git-init guard presence'*) ;; *) echo "WRONG-STEP: $step" ;; esac
    head -n $((anchor - 1)) "$gate" | grep -nv '^[[:space:]]*#' | while IFS= read -r line; do
        while IFS= read -r n; do
            case "$FIXTURE_SET" in *" $n "*) echo "$line"; break ;; esac
        done < <(grep -oE '[A-Za-z0-9_.-]+\.sh' <<<"$line" | sort -u)
    done
}
VIOLATIONS=$(guard_order_violations "$GATE")
check "check-git-init-guarded.sh runs before every fixture-building script" test -z "$VIOLATIONS"
[ -z "$VIOLATIONS" ] || printf '%s\n' "$VIOLATIONS" | sed 's/^/        /'

# Mutation: the same gate with a fixture-building self-test invoked above the
# checker must be refused, or the ordering check is a check of nothing.
MUT=$(mktemp); trap 'rm -f "$MUT"' EXIT
awk '{print} /^gate_step "Conflict markers/{print "bash \"$SCRIPT_DIR/test-a11y-select.sh\""}' "$GATE" >"$MUT"
check "mutation: a fixture-building script above the checker is refused" \
    test -n "$(guard_order_violations "$MUT")"
# Transitivity: this file runs no git init of its own, but its executable lines
# name the checker (itself on the --list), so the closure must have added it.
in_fixture_set() { case "$FIXTURE_SET" in *" $1 "*) return 0 ;; esac; return 1; }
check "the fixture set is closed transitively (this file joins it by naming the checker)" \
    in_fixture_set test-gate-timing.sh

echo "=== RESULTS: $pass passed, $fail failed ==="
[ "$fail" -eq 0 ]
