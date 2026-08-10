#!/bin/bash
# test-run-flag-resolution.sh -- hermetic tests for scripts/lib/run-flags.sh,
# plus an end-to-end assertion that the pre-push gate actually consumes it.
#
# THE BUG THIS PINS
# -----------------
# Each RUN_* tier is three-state (run / skip / default), and each was written
# as a `case` whose `*)` branch carried the DEFAULT. That folds a FOURTH state
# into the third: `RUN_VULN=truee` matched `*)`, printed "skipped by default",
# and ran nothing -- while the developer who typed it believed they had asked
# for a blocking run and read a green gate as the answer. That is the exact
# silent-failure shape #2983 exists to delete, so the gate reintroducing it
# would contradict the change's own premise.
#
# Two layers, because either alone is satisfiable by a broken gate:
#   1. UNIT -- source run-flags.sh in a subshell and assert every state,
#      including the invalid one, without booting anything.
#   2. WIRING -- run the real gate with a bad value and assert it refuses.
#      A perfect resolver that nothing calls is not a fix.
#
# Run: bash scripts/test-run-flag-resolution.sh
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
GATE="$SCRIPT_DIR/pre-push-gate.sh"
LIB="$SCRIPT_DIR/lib/run-flags.sh"

pass=0
fail=0

record() {
  local ok="$1" name="$2" detail="${3:-}"
  if [ "$ok" -eq 1 ]; then
    pass=$((pass + 1))
    echo "PASS: $name"
  else
    fail=$((fail + 1))
    echo "FAIL: $name"
    [ -n "$detail" ] && printf '%s\n' "$detail" | sed 's/^/      /'
  fi
}

# --- 1. UNIT: the resolver itself ---------------------------------------------
# Each call runs in its own subshell so the invalid case's `exit 2` is
# observable as an exit status rather than killing this suite.
#
# `|| true` on the subshell is load-bearing, not defensive noise: a resolver
# that WRONGLY refuses a valid spelling exits 2, and without it `set -e` would
# kill this suite mid-loop -- a nonzero exit with no output naming which
# spelling broke. The failure must be REPORTED, not merely fatal.
resolve() {
  # shellcheck source=scripts/lib/run-flags.sh
  ( . "$LIB"; resolve_run_flag TEST_FLAG "$1"; printf '%s\n' "$RESOLVED_RUN_FLAG" ) 2>&1 || true
}
resolve_status() {
  # shellcheck source=scripts/lib/run-flags.sh
  ( . "$LIB"; resolve_run_flag TEST_FLAG "$1" ) >/dev/null 2>&1 && echo 0 || echo $?
}

# Every accepted spelling, in both cases, plus surrounding whitespace -- the
# documented contract (CONTRIBUTING.md, docs/pr-workflow.md) promises all of
# these, so a resolver that quietly dropped, say, `on` would break a
# documented invocation with no other test noticing.
for v in 1 true yes on TRUE Yes ON " 1 " "  true"; do
  got=$(resolve "$v")
  if [ "$got" = "on" ]; then
    record 1 "'$v' resolves to 'on'"
  else
    record 0 "'$v' resolves to 'on'" "got: $got"
  fi
done

for v in 0 false no off FALSE No OFF " 0 "; do
  got=$(resolve "$v")
  if [ "$got" = "off" ]; then
    record 1 "'$v' resolves to 'off'"
  else
    record 0 "'$v' resolves to 'off'" "got: $got"
  fi
done

# Unset, and whitespace-only (which normalizes to unset), take the DEFAULT.
# Without this the refusal could have been implemented by rejecting everything
# that is not explicitly on/off, which would abort every ordinary push.
for v in "" "   "; do
  got=$(resolve "$v")
  if [ "$got" = "default" ]; then
    record 1 "'$v' (empty/whitespace) resolves to 'default'"
  else
    record 0 "'$v' (empty/whitespace) resolves to 'default'" "got: $got"
  fi
done

# The invalid state: near-misses of every accepted spelling, so a resolver
# that special-cased one typo rather than fixing the fold-through is caught.
for v in truee yess offf onnn 2 -1 maybe "true false" nope; do
  status=$(resolve_status "$v")
  if [ "$status" = "2" ]; then
    record 1 "'$v' is refused with exit 2 (invalid input, not a check failure)"
  else
    record 0 "'$v' is refused with exit 2 (invalid input, not a check failure)" \
      "got exit $status"
  fi
done

# The message has to name the variable AND the value: "invalid value" alone
# leaves the operator hunting across four variables for their own typo.
msg=$(resolve "truee")
if printf '%s' "$msg" | grep -q "TEST_FLAG='truee' is not a recognized value"; then
  record 1 "the refusal names both the variable and the offending value"
else
  record 0 "the refusal names both the variable and the offending value" "$msg"
fi

# The refusal must not read as a skip. If it ever prints the default-path SKIP
# vocabulary, the operator is back to believing a tier ran when it did not.
if printf '%s' "$msg" | grep -qi 'skipped'; then
  record 0 "the refusal does not read as a skip" "$msg"
else
  record 1 "the refusal does not read as a skip"
fi

# --- 2. WIRING: the real gate consumes the resolver ---------------------------
# One case per variable rather than a single representative: the bug was
# copy-pasted across all four tiers, so a fix applied to one and not the
# others would pass a single-variable test.
#
# Cheap despite invoking the real gate: resolution happens at the TOP of the
# script, before the worktree lock and before any check runs, so an invalid
# value aborts in milliseconds. That ordering is load-bearing for this test's
# safety, so it is ASSERTED rather than assumed -- an exit 2 caused by lock
# contention would otherwise be indistinguishable from the refusal, and this
# test would report green over a deleted feature.
for spec in "RUN_RACE:truee" "RUN_VULN:yess" "RUN_PROVIDER_SMOKE:offf" "RUN_A11Y:onnn"; do
  var="${spec%%:*}"
  bad="${spec##*:}"

  out=$(env -u RUN_RACE -u RUN_VULN -u RUN_PROVIDER_SMOKE -u RUN_A11Y \
        "$var=$bad" bash "$GATE" 2>&1) && status=0 || status=$?

  if [ "$status" -eq 2 ]; then
    record 1 "gate: $var=$bad exits 2"
  else
    record 0 "gate: $var=$bad exits 2" "got exit $status; output:
$out"
  fi

  if printf '%s' "$out" | grep -q "$var='$bad' is not a recognized value"; then
    record 1 "gate: $var=$bad names the offending variable and value"
  else
    record 0 "gate: $var=$bad names the offending variable and value" "$out"
  fi

  if printf '%s' "$out" | grep -qi 'another pre-push-gate is running\|lock is initializing'; then
    record 0 "gate: $var=$bad aborts BEFORE the worktree lock" \
      "the gate reached the lock, so flag resolution moved below it:
$out"
  else
    record 1 "gate: $var=$bad aborts BEFORE the worktree lock"
  fi

  if printf '%s' "$out" | grep -q 'skipped by default'; then
    record 0 "gate: $var=$bad prints no 'skipped by default' line" "$out"
  else
    record 1 "gate: $var=$bad prints no 'skipped by default' line"
  fi
done

echo ""
echo "=== RESULTS: $pass passed, $fail failed (of $((pass + fail)) checks) ==="
[ "$fail" -eq 0 ]
