#!/bin/bash
# test-check-gate-invariant.sh -- hermetic tests for check-gate-invariant.sh.
#
# The guard's whole value is that it FAILS on a regression, so the tests that
# matter are the mutation cases: take a gate-shaped script that passes, inject
# exactly one advisory shape, and assert the guard now rejects it. A test that
# only checks the happy path would pass against a guard that returns 0
# unconditionally.
#
# Run: bash scripts/test-check-gate-invariant.sh
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
GUARD="$SCRIPT_DIR/check-gate-invariant.sh"

TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT

pass=0
fail=0

# expect <expected-status> <name> <script-body>
expect() {
  local want="$1" name="$2" body="$3"
  local f="$TMP/gate.sh" out status
  printf '%s\n' "$body" > "$f"
  out=$(bash "$GUARD" "$f" 2>&1) && status=0 || status=$?
  if [ "$status" -eq "$want" ]; then
    pass=$((pass + 1))
    echo "PASS: $name"
  else
    fail=$((fail + 1))
    echo "FAIL: $name (expected exit $want, got $status)"
    printf '%s\n' "$out" | sed 's/^/      /'
  fi
}

# --- baseline: a compliant gate ----------------------------------------------
# Every later case is this script plus one mutation, so a guard that rejects
# everything is caught here rather than looking like a row of successes.
CLEAN='#!/bin/bash
set -euo pipefail
echo "=== Something ==="
if ! do_a_thing; then
  echo "FAIL: the thing failed" >&2
  exit 1
fi
echo "SKIP: expensive tier skipped by default; CI is authoritative"
echo "All hard checks passed."'

expect 0 "compliant gate passes" "$CLEAN"

# --- mutation 1: advisory phrasing -------------------------------------------
expect 1 "rejects 'not blocking this push'" "$CLEAN
echo \"WARNING no verdict word here but: not blocking this push\""

expect 1 "rejects 'advisory failure'" "$CLEAN
echo \"tier: advisory failure in the auto path\""

# --- mutation 2: WARN: verdict ------------------------------------------------
expect 1 "rejects a WARN: verdict" "$CLEAN
echo \"WARN: the tier failed but we are continuing\""

# --- mutation 3: FAIL without exit -------------------------------------------
expect 1 "rejects a FAIL: announcement that does not exit" '#!/bin/bash
set -euo pipefail
if ! do_a_thing; then
  echo "FAIL: the thing failed" >&2
fi
echo "All hard checks passed."'

# --- the exemptions, asserted explicitly -------------------------------------
# These are the shapes the guard must NOT reject. Without them the guard would
# be trivially satisfiable by deleting the gate's documentation, and the first
# person to write an honest comment about advisory behavior would be blocked.
expect 0 "allows the banned phrases inside full-line comments" "$CLEAN
# Historical note: this tier used to print 'not blocking this push' and was
# an advisory failure in the auto path. It no longer is."

expect 0 "allows 'WARNING:' bookkeeping that is not a check verdict" "$CLEAN
echo \"    WARNING: could not update the roster; next run may miss a removal\" >&2"

expect 0 "allows HINT: attached to a real failure" '#!/bin/bash
if ! measure; then
  echo "FAIL: coverage below threshold" >&2
  echo "HINT: run RUN_RACE=1 for the CI-equivalent profile." >&2
  exit 1
fi
echo "All hard checks passed."'

# --- input handling ----------------------------------------------------------
missing_out=$(bash "$GUARD" "$TMP/does-not-exist.sh" 2>&1) && missing_status=0 || missing_status=$?
if [ "$missing_status" -eq 2 ]; then
  pass=$((pass + 1))
  echo "PASS: missing gate script exits 2 (setup error, not a check failure)"
else
  fail=$((fail + 1))
  echo "FAIL: missing gate script (expected exit 2, got $missing_status)"
  printf '%s\n' "$missing_out" | sed 's/^/      /'
fi

# --- the real gate ------------------------------------------------------------
# The guard is pointed at the live gate by the gate itself, so a test suite
# that never looks at the real file could pass while the real file violates
# the invariant.
if bash "$GUARD" "$SCRIPT_DIR/pre-push-gate.sh" >/dev/null 2>&1; then
  pass=$((pass + 1))
  echo "PASS: the live pre-push-gate.sh satisfies the invariant"
else
  fail=$((fail + 1))
  echo "FAIL: the live pre-push-gate.sh violates the invariant"
  bash "$GUARD" "$SCRIPT_DIR/pre-push-gate.sh" 2>&1 | sed 's/^/      /'
fi

echo ""
echo "=== RESULTS: $pass passed, $fail failed (of $((pass + fail)) checks) ==="
[ "$fail" -eq 0 ]
