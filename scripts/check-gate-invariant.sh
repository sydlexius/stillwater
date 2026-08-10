#!/bin/bash
# check-gate-invariant.sh -- assert scripts/pre-push-gate.sh has no advisory
# step in its default path (#2983).
#
# THE INVARIANT
# -------------
#   A check in the gate's default path either BLOCKS the push, or it is not in
#   the default path at all.
#
# The failure this guards against is specific and has happened: the a11y tier
# ran for ~2.4 minutes, FAILED, printed "not blocking this push", and the gate
# then printed "All hard checks passed" and exited 0. The repo's own
# instructions had to carry a warning label telling readers not to trust that
# banner. A gate whose success message needs a footnote is not a gate.
#
# Why a static guard rather than a note in a comment: every advisory step in
# the gate's history was added for a defensible local reason (a flaky harness,
# a slow network scan, a large package). The pressure that produced them has
# not gone away, so the rule needs something that fails rather than something
# that reminds.
#
# WHAT IT CHECKS
# --------------
#   1. No advisory-verdict phrasing. The exact sentences an advisory step
#      prints ("not blocking this push", "advisory failure") are banned in
#      executable lines. This is deliberately phrase-level: it names the
#      behavior, so the message a future advisory step would print is the
#      thing that trips it.
#   2. No `WARN:`-prefixed output. The gate has exactly two verdicts a reader
#      needs: a step FAILED (and the gate exited), or a step was SKIPped (and
#      did not run). `WARN:` is the vocabulary of a third, "it ran and failed
#      and we are continuing anyway", which the invariant forbids. Use `SKIP:`
#      for a step that did not run and `HINT:` for advice attached to a real
#      failure. (`WARNING:` is left alone: the two uses in the gate are
#      bookkeeping about the lint-cache roster, not the verdict of a check.)
#   3. Every `FAIL`-announcing line is followed by an exit. A `FAIL:` message
#      that does not exit within the next few lines is an advisory step
#      wearing a blocking step's words -- the most likely way this regresses,
#      since it reads as correct at a glance.
#
# Scope: the gate script only. It is cheap (three greps) and hermetic, so the
# gate runs it on itself near the top of every run, and it is also runnable
# standalone: `bash scripts/check-gate-invariant.sh [path-to-gate]`.
set -euo pipefail

GATE="${1:-$(cd "$(dirname "$0")" && pwd)/pre-push-gate.sh}"

if [ ! -f "$GATE" ]; then
  echo "FAIL: gate script not found at '$GATE'" >&2
  exit 2
fi

# Executable lines only: strip full-line comments so the gate's own prose
# ABOUT the banned patterns (this file's rationale, the gate's header block)
# does not trip the guard. A trailing comment on a real line is left in place
# -- an advisory phrase hidden there would still be inside a live statement.
exec_lines() {
  grep -nv '^[[:space:]]*#' "$GATE" || true
}

status=0

# --- 1. advisory-verdict phrasing --------------------------------------------
advisory=$(exec_lines | grep -nEi 'not blocking this push|advisory failure' || true)
if [ -n "$advisory" ]; then
  echo "FAIL: pre-push-gate.sh contains advisory-verdict phrasing (#2983 invariant):"
  printf '%s\n' "$advisory" | sed 's/^/  /'
  echo "  A step that runs and fails must exit non-zero. If it should not block,"
  echo "  it does not belong in the default path -- give it a RUN_* opt-in that"
  echo "  defaults to SKIP, and name the REQUIRED CI check that covers it."
  status=1
fi

# --- 2. WARN: verdicts --------------------------------------------------------
warns=$(exec_lines | grep -nE '(^|[^A-Za-z])WARN:' || true)
if [ -n "$warns" ]; then
  echo "FAIL: pre-push-gate.sh emits a 'WARN:' verdict (#2983 invariant):"
  printf '%s\n' "$warns" | sed 's/^/  /'
  echo "  The gate has two verdicts: a check FAILED (and it exits), or a check"
  echo "  was SKIPped (and did not run). Use 'SKIP:' for the latter, or 'HINT:'"
  echo "  for advice printed alongside a real failure."
  status=1
fi

# --- 3. every FAIL announcement exits ----------------------------------------
# Read the file once and look ahead a small window from each FAIL-announcing
# line. The window (5 lines) covers the shapes the gate actually uses: a FAIL
# echo followed by one or two detail echoes and then `exit`. A wider window
# would start swallowing genuinely separate branches.
missing_exit=$(awk '
  /^[[:space:]]*#/ { next }
  { line[NR] = $0 }
  /echo[^#]*"FAIL/ { fails[NR] = $0 }
  END {
    for (n in fails) {
      # Array subscripts are STRINGS in awk, so `n` and a bare `n + 5` compare
      # LEXICALLY ("5" > "10"), which silently collapses the look-ahead window
      # to zero iterations on single-digit line numbers -- reporting every
      # FAIL as exit-less. Coerce to a number first.
      start = n + 0
      found = 0
      for (i = start; i <= start + 5; i++) {
        if (i in line && line[i] ~ /(^|[[:space:];&|])exit[[:space:]]+[0-9]/) { found = 1; break }
      }
      if (!found) printf "%d:%s\n", start, fails[n]
    }
  }
' "$GATE" | sort -n || true)
if [ -n "$missing_exit" ]; then
  echo "FAIL: pre-push-gate.sh announces a FAIL without exiting (#2983 invariant):"
  printf '%s\n' "$missing_exit" | sed 's/^/  /'
  echo "  A 'FAIL:' message that lets the run continue is an advisory step"
  echo "  wearing a blocking step's words; the gate would still print"
  echo "  \"All hard checks passed\" underneath it."
  status=1
fi

if [ "$status" -eq 0 ]; then
  echo "OK: no advisory step in the gate's default path."
fi
exit "$status"
