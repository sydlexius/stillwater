#!/usr/bin/env bash
# scripts/lib/run-flags.sh -- three-state RUN_* flag resolution for the
# pre-push gate.
#
# Sourced, not executed. Provides one function, `resolve_run_flag`, which maps
# a RUN_* environment variable to exactly one of: "on", "off", or "default".
#
# WHY THIS IS A FUNCTION AND NOT FOUR `case` STATEMENTS
# ----------------------------------------------------
# Each tier used to carry its own inline `case` whose `*)` branch held the
# tier's DEFAULT. That silently folded a FOURTH state into the third: a
# mistyped `RUN_VULN=truee` matched `*)`, printed "skipped by default", and ran
# nothing -- while the developer who typed it had asked for a blocking run and
# then read a green gate as the answer. A gate that observes a request it
# cannot honor and says nothing actionable is the precise defect #2983 exists
# to remove, so the gate reintroducing it would contradict its own premise.
#
# One shared resolver makes the invalid case impossible to omit from a tier:
# there is no per-tier `*)` left to get wrong. It also makes the rule
# independently testable (scripts/test-run-flag-resolution.sh) without booting
# the gate.
#
# Resolution is deliberately done EARLY, before the worktree lock and before
# any check runs. A typo is a setup error; it should cost a second, not a full
# gate run followed by a puzzling result.

# resolve_run_flag <VAR_NAME> <raw value>
#
# Writes its answer to the global RESOLVED_RUN_FLAG rather than stdout. A
# command substitution would run the `exit` below inside a SUBSHELL, so the
# abort would depend on `set -e` firing on the enclosing assignment -- correct
# today and quietly broken the first time a caller writes
# `if x=$(resolve_run_flag ...)`, where `set -e` is suppressed.
#
# Exits 2, not 1, per the gate's documented convention: 1 means "a hard check
# failed", 2 means "invalid input / setup state". Nothing was checked here --
# the operator mistyped a variable name's value.
#
# shellcheck disable=SC2034  # read by the sourcing script, not by this file.
RESOLVED_RUN_FLAG=""
resolve_run_flag() {
  local name="$1" raw="$2" normalized
  normalized="$(printf '%s' "$raw" | tr '[:upper:]' '[:lower:]' | tr -d '[:space:]')"
  case "$normalized" in
    1 | true | yes | on) RESOLVED_RUN_FLAG="on" ;;
    0 | false | no | off) RESOLVED_RUN_FLAG="off" ;;
    "") RESOLVED_RUN_FLAG="default" ;;
    *)
      echo "FAIL: ${name}='${raw}' is not a recognized value -- refusing to continue," >&2
      echo "      because treating it as 'unset' would silently skip a tier you asked to run." >&2
      echo "      Use 1/true/yes/on (blocking run) or 0/false/no/off (explicit skip)," >&2
      echo "      case-insensitive; leave ${name} unset for the default." >&2
      exit 2
      ;;
  esac
}
