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

# --- mutation 2: WARN:/WARNING: verdict ----------------------------------------
# WARN: and WARNING: are a single reserved vocabulary (#2994) -- both spellings
# must be rejected, or widening one without the other reopens the hole a real
# CodeRabbit finding caught (a default-path step printing "WARNING: check
# failed; continuing" reached "All hard checks passed" undetected).
expect 1 "rejects a WARN: verdict" "$CLEAN
echo \"WARN: the tier failed but we are continuing\""

expect 1 "rejects a WARNING: verdict" "$CLEAN
echo \"WARNING: check failed; continuing\""

# --- mutation 3: FAIL without exit -------------------------------------------
# Each case below is the SAME advisory shape ("we announced a failure and kept
# going") written a different way. The detector is a static pattern, so every
# idiom an author might reach for has to be named explicitly or the guard
# reports OK on a real regression -- these cases are what proves the pattern
# actually covers them rather than only the one form it was written against.
expect 1 "rejects a FAIL: announcement that does not exit" '#!/bin/bash
set -euo pipefail
if ! do_a_thing; then
  echo "FAIL: the thing failed" >&2
fi
echo "All hard checks passed."'

expect 1 "rejects a SINGLE-quoted echo FAIL: that does not exit" '#!/bin/bash
set -euo pipefail
if ! do_a_thing; then
  echo '"'"'FAIL: the thing failed'"'"' >&2
fi
echo "All hard checks passed."'

expect 1 "rejects a printf FAIL: that does not exit" '#!/bin/bash
set -euo pipefail
if ! do_a_thing; then
  printf '"'"'FAIL: the thing failed\n'"'"' >&2
fi
echo "All hard checks passed."'

# shellcheck disable=SC2016  # the fixture must contain a LITERAL $reason: it is
# gate source text handed to the guard, never expanded by this script.
expect 1 "rejects a double-quoted printf FAIL: that does not exit" '#!/bin/bash
set -euo pipefail
if ! do_a_thing; then
  printf "FAIL: %s\n" "$reason" >&2
fi
echo "All hard checks passed."'

# `exit` with a status the guard cannot STATICALLY show to be non-zero does not
# satisfy "must exit". A bare `exit` returns the status of the last command --
# which, right after a successful `echo`, is 0. `exit "$rc"` is the same class:
# the value is a runtime unknown, and half of its possible values are advisory.
expect 1 "rejects a bare 'exit' after a FAIL: (it exits 0 after a successful echo)" '#!/bin/bash
set -euo pipefail
if ! do_a_thing; then
  echo "FAIL: the thing failed" >&2
  exit
fi
echo "All hard checks passed."'

# shellcheck disable=SC2016  # the fixture must contain a LITERAL $rc -- that is
# precisely the shape under test.
expect 1 "rejects 'exit \$rc' after a FAIL: (status is not statically non-zero)" '#!/bin/bash
set -euo pipefail
if ! do_a_thing; then
  echo "FAIL: the thing failed" >&2
  exit "$rc"
fi
echo "All hard checks passed."'

expect 1 "rejects an explicit 'exit 0' after a FAIL:" '#!/bin/bash
set -euo pipefail
if ! do_a_thing; then
  echo "FAIL: the thing failed" >&2
  exit 0
fi
echo "All hard checks passed."'

# --- the VALID forms of each newly-covered shape ------------------------------
# Widening a detector is only half the work: without these, the pattern could
# have been widened into one that rejects everything, and the mutation cases
# above would still all pass.
expect 0 "allows a single-quoted echo FAIL: that exits" '#!/bin/bash
set -euo pipefail
if ! do_a_thing; then
  echo '"'"'FAIL: the thing failed'"'"' >&2
  exit 1
fi
echo "All hard checks passed."'

expect 0 "allows a printf FAIL: that exits" '#!/bin/bash
set -euo pipefail
if ! do_a_thing; then
  printf '"'"'FAIL: the thing failed\n'"'"' >&2
  exit 1
fi
echo "All hard checks passed."'

expect 0 "allows a multi-digit exit status after a FAIL:" '#!/bin/bash
set -euo pipefail
if ! do_a_thing; then
  echo "FAIL: the thing failed" >&2
  exit 42
fi
echo "All hard checks passed."'

expect 0 "allows 'exit 2' (setup error) after a FAIL:" '#!/bin/bash
set -euo pipefail
if ! resolve_base; then
  echo "FAIL: cannot resolve BASE" >&2
  exit 2
fi
echo "All hard checks passed."'

# The gate's real shape: a FAIL announcement, one or two detail lines, then the
# exit. If the look-ahead window ever collapses (the awk lexical-subscript bug
# this suite was written for), this is the case that catches it.
expect 0 "allows a FAIL: followed by detail lines and then an exit" '#!/bin/bash
set -euo pipefail
if ! measure; then
  echo "FAIL: the thing failed" >&2
  echo "      first detail line" >&2
  echo "      second detail line" >&2
  exit 1
fi
echo "All hard checks passed."'

# --- diagnostics report TRUE gate line numbers --------------------------------
# Checks 1 and 2 filter the file through `grep -nv` (which prefixes the true
# line number) before matching. A second `-n` on the inner grep would prepend a
# position in the FILTERED stream, producing `7:64:echo ...` -- a reader
# jumping to "line 7" lands somewhere unrelated. Assert the reported number is
# the real one.
lineno_gate="$TMP/lineno-gate.sh"
cat > "$lineno_gate" <<'LINENO_GATE'
#!/bin/bash
# a full-line comment, stripped by exec_lines
set -euo pipefail
echo "hello"
# another stripped comment
echo "WARN: it failed and we are continuing"
LINENO_GATE
lineno_out=$(bash "$GUARD" "$lineno_gate" 2>&1 || true)
if printf '%s\n' "$lineno_out" | grep -qE '^[[:space:]]*6:echo "WARN:'; then
  pass=$((pass + 1))
  echo "PASS: diagnostics cite the TRUE gate line number (6), not a filtered-stream position"
else
  fail=$((fail + 1))
  echo "FAIL: diagnostics did not cite line 6 for the WARN: on line 6"
  printf '%s\n' "$lineno_out" | sed 's/^/      /'
fi

# --- the exemptions, asserted explicitly -------------------------------------
# These are the shapes the guard must NOT reject. Without them the guard would
# be trivially satisfiable by deleting the gate's documentation, and the first
# person to write an honest comment about advisory behavior would be blocked.
expect 0 "allows the banned phrases inside full-line comments" "$CLEAN
# Historical note: this tier used to print 'not blocking this push' and was
# an advisory failure in the auto path. It no longer is."

expect 0 "allows 'NOTE:' bookkeeping that is not a check verdict" "$CLEAN
echo \"    NOTE: could not update the roster; next run may miss a removal\" >&2"

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
