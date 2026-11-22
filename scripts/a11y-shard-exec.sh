#!/bin/bash
# a11y-shard-exec.sh -- run a command with ONE a11y-test matrix entry's
# Playwright selection arguments appended (#3442).
#
# This is the ONLY place the matrix's `specs` value is turned into arguments.
# CI's "Run Playwright a11y tests" step (ci.yml) runs Playwright through it, and
# scripts/check-a11y-shards.sh runs its `playwright test --list` proof (and its
# structural token read) through it too, so the guard proves the exact argv CI
# passes. Two tokenizers drifted once: Python's str.split() saw every line of a
# `specs: |` block while the run step's `read` passed only the first.
#
# Input (env, as the run step sets it from the matrix):
#   SPECS          the entry's `specs` value; empty for a --shard entry.
#   SW_A11Y_SHARD  "<shard>/<shards>".
# A non-empty SPECS must be ONE line of space-separated repo-relative paths: only
# [A-Za-z0-9._/-] and ASCII space, no token starting with "-" (it would be read
# as a flag). A newline, tab, non-ASCII whitespace (U+00A0) or any other byte
# fails, as does a value with no tokens -- never a partial selection. YAML's
# folded `>-` scalar produces exactly this shape. A token must also be a
# normalized relative path: no leading "/", no ".", ".." or empty segment.
# SW_A11Y_SHARD must be "<k>/<n>" with 1 <= k <= n in BOTH modes. An empty SPECS
# selects `--shard=$SW_A11Y_SHARD`.
#
# The selection is printed to stderr (the CI log shows the exact filters), then
# the command is exec'd with it appended:
#   bash scripts/a11y-shard-exec.sh npx playwright test --project=X
# Exit 1 on an invalid value, 2 on no command.
set -euo pipefail
# The checks below run in the C locale (byte-wise character classes); the
# command is exec'd with the caller's locale restored, so Playwright and the
# browsers it starts see the environment CI gave the step, not LC_ALL=C.
if [ "${LC_ALL+set}" = set ]; then orig_lc_all=(LC_ALL="$LC_ALL"); else orig_lc_all=(-u LC_ALL); fi
LC_ALL=C
[ "$#" -gt 0 ] || { echo "a11y-shard-exec: usage: a11y-shard-exec.sh <command> [args...]" >&2; exit 2; }
SPECS="${SPECS-}"
SHARD="${SW_A11Y_SHARD-}"
die() { echo "a11y-shard-exec: shard ${SHARD:-?}: $*" >&2; exit 1; }
# Both mechanisms: SW_A11Y_SHARD is "<k>/<n>" with 1 <= k <= n (global-teardown.js
# reads it too). At most 6 digits each, so the arithmetic cannot overflow.
[[ "$SHARD" =~ ^([1-9][0-9]{0,5})/([1-9][0-9]{0,5})$ ]] || die "SW_A11Y_SHARD is not <shard>/<shards> (positive integers)"
[ "${BASH_REMATCH[1]}" -le "${BASH_REMATCH[2]}" ] || die "SW_A11Y_SHARD shard ${BASH_REMATCH[1]} exceeds shards ${BASH_REMATCH[2]}"
if [ -n "$SPECS" ]; then
  if [[ "$SPECS" =~ [^A-Za-z0-9._/\ -] ]]; then
    bad=$(printf '%s' "$SPECS" | LC_ALL=C tr -d 'A-Za-z0-9._/ -' | LC_ALL=C od -An -c | tr -s ' ' | head -c 80)
    die "specs may hold only [A-Za-z0-9._/-] paths separated by ASCII spaces, on ONE line (use a folded \`>-\` scalar, not \`|\`); found:${bad}"
  fi
  # SPECS holds no newline (checked above), so read sees the whole value.
  IFS=' ' read -ra sel <<<"$SPECS"
  [ "${#sel[@]}" -gt 0 ] || die "specs is non-empty but holds no paths"
  for t in "${sel[@]}"; do
    case "$t" in -*) die "specs token '$t' starts with '-' (would be read as a flag)" ;; esac
    # Repo-relative and normalized: no leading '/', and no empty, '.' or '..'
    # segment ('.', '..', '/', 'a//b', 'a/../b' and a trailing '/' all fail).
    case "/$t/" in //* | *//* | */./* | */../*)
      die "specs token '$t' is not a normalized repo-relative path (no leading '/', no '.', '..' or empty segment)" ;;
    esac
  done
  echo "a11y-shard-exec: shard $SHARD: ${#sel[@]} spec file filter(s): ${sel[*]}" >&2
else
  sel=("--shard=$SHARD")
  echo "a11y-shard-exec: shard $SHARD: count-based ${sel[0]}" >&2
fi
exec env "${orig_lc_all[@]}" "$@" "${sel[@]}"
