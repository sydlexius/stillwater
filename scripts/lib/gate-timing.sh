#!/usr/bin/env bash
# scripts/lib/gate-timing.sh -- per-step wall-clock timing for pre-push-gate.sh.
#
# gate_step "<name>" prints the usual `=== <name> ===` header and starts a
# timer; the PREVIOUS step's elapsed seconds are printed the moment the next
# header (or gate_timing_summary) closes it. gate_timing_summary prints a
# step/seconds table, once. The gate calls it before its success banner, and
# from its EXIT trap so a failing run still shows where the time went.
#
# Pure observation: nothing here changes any step's exit status. Bash 3.2
# compatible (macOS): indexed arrays and SECONDS only, no mapfile, no
# associative arrays. Source this file; do not execute it.

GATE_STEP_NAMES=()
GATE_STEP_SECS=()
GATE_STEP_CUR=""
GATE_STEP_START=0
GATE_TIMING_PRINTED=0

# gate_timing_close -- finish the open step (if any): record it, print its line.
gate_timing_close() {
  [ -n "$GATE_STEP_CUR" ] || return 0
  local elapsed=$((SECONDS - GATE_STEP_START))
  GATE_STEP_NAMES[${#GATE_STEP_NAMES[@]}]="$GATE_STEP_CUR"
  GATE_STEP_SECS[${#GATE_STEP_SECS[@]}]="$elapsed"
  echo "    [step took ${elapsed}s: ${GATE_STEP_CUR}]"
  GATE_STEP_CUR=""
}

gate_step() {
  gate_timing_close
  GATE_STEP_CUR="$1"
  GATE_STEP_START=$SECONDS
  echo "=== $1 ==="
}

# gate_timing_summary -- idempotent; prints the table at most once per run.
gate_timing_summary() {
  [ "$GATE_TIMING_PRINTED" -eq 0 ] || return 0
  GATE_TIMING_PRINTED=1
  gate_timing_close
  [ "${#GATE_STEP_NAMES[@]}" -gt 0 ] || return 0
  local i total=0
  echo ""
  echo "=== Gate step timing ==="
  for ((i = 0; i < ${#GATE_STEP_NAMES[@]}; i++)); do
    printf '%6ss  %s\n' "${GATE_STEP_SECS[$i]}" "${GATE_STEP_NAMES[$i]}"
    total=$((total + GATE_STEP_SECS[i]))
  done
  printf '%6ss  total (sum of steps)\n' "$total"
}
