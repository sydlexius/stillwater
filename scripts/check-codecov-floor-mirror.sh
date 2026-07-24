#!/usr/bin/env bash
# check-codecov-floor-mirror.sh -- assert codecov.yml's per-package project
# targets mirror testdata/coverage-floor.json exactly (#2756).
#
# codecov.yml's `coverage.status.project` block claims, in its own comment, to
# mirror testdata/coverage-floor.json entry-for-entry. Nothing enforced that
# claim: the floor was lowered for internal/server in #2066 (91 -> 87) but the
# codecov.yml side was never updated, and the drift sat unnoticed because
# codecov/project statuses are not among this repo's required checks. This
# script is the enforcement that was missing.
#
# What it checks:
#   - Every key in testdata/coverage-floor.json (e.g. "internal/server") has a
#     matching project entry in codecov.yml, found via its `paths:` list
#     (not by name -- see the name-mapping note below).
#   - That entry's `target:` percentage equals the floor JSON's value exactly.
#   - Every non-`default` project entry in codecov.yml maps back to some floor
#     entry (catches a stale codecov.yml entry for a package the floor no
#     longer tracks).
#
# Name mapping between the two files: the floor JSON is keyed by the real
# package path ("internal/connection/mediabrowser"); codecov.yml keys its
# project entries by an arbitrary YAML-safe label ("connection_mediabrowser")
# but always states the real path underneath as a `paths:` entry. So this
# script never guesses a naming rule -- it reads codecov.yml's own `paths:`
# line for each entry and matches on that, which is exactly what Codecov
# itself uses to decide which entry governs which package.
#
# Written in awk/grep, Bash 3.2 compatible (stock macOS /bin/bash), matching
# the convention in scripts/coverage-floor.sh: no jq dependency, no `declare
# -A`, no `mapfile`.
#
# Exit codes:
#   0  every floor entry has a matching, value-equal codecov.yml entry, and
#      every codecov.yml entry maps back to a floor entry.
#   1  one or more divergences found (reported individually, not just the
#      first).
#   2  setup error: a required file is missing or could not be parsed at all
#      (this is deliberately distinct from "0 divergences found" -- a check
#      that silently passes because it read nothing would be worse than no
#      check).
set -euo pipefail

ROOT="$(git rev-parse --show-toplevel 2>/dev/null || pwd)"
cd "$ROOT"

FLOOR_FILE="testdata/coverage-floor.json"
CODECOV_FILE="codecov.yml"

setup_error() { echo "check-codecov-floor-mirror: setup error: $1" >&2; exit 2; }

[ -f "$FLOOR_FILE" ] || setup_error "missing $FLOOR_FILE"
[ -f "$CODECOV_FILE" ] || setup_error "missing $CODECOV_FILE"
[ -s "$FLOOR_FILE" ] || setup_error "$FLOOR_FILE is empty"
[ -s "$CODECOV_FILE" ] || setup_error "$CODECOV_FILE is empty"

# ---------------------------------------------------------------------------
# Parse testdata/coverage-floor.json into "internal/pkg value" lines.
# The file is a flat JSON object of "path": number pairs (see coverage-floor.sh
# for the writer). A dedicated line-oriented awk pass is sufficient; no JSON
# library needed, matching the rest of the gate's no-jq convention.
# ---------------------------------------------------------------------------
floor_pairs=$(awk '
  /"internal\/[^"]+"[[:space:]]*:[[:space:]]*[0-9]+/ {
    line = $0
    # Extract the quoted key.
    keystart = index(line, "\"") + 1
    rest = substr(line, keystart)
    keyend = index(rest, "\"")
    key = substr(rest, 1, keyend - 1)
    if (substr(key, 1, 9) != "internal/") next
    # Extract the trailing integer value.
    after = substr(rest, keyend + 1)
    match(after, /[0-9]+/)
    val = substr(after, RSTART, RLENGTH)
    print key, val
  }
' "$FLOOR_FILE")

floor_count=$(printf '%s\n' "$floor_pairs" | grep -c . || true)
[ "$floor_count" -ge 1 ] 2>/dev/null \
  || setup_error "parsed zero entries from $FLOOR_FILE -- format changed? extractor needs updating"

# ---------------------------------------------------------------------------
# Parse codecov.yml's coverage.status.project block into
# "internal/pkg target label" lines. Only the block between the line
# "    project:" and the following "ignore:" (or EOF) top-level key is
# considered project entries; the sibling "patch:" block above is intentionally
# excluded by never entering scope for it.
#
# Each entry is a YAML mapping of the form:
#   label:
#     target: NN%
#     paths:
#       - internal/some/path
# `default:` (the catch-all, no `paths:`) is deliberately skipped: it does not
# correspond to a floor JSON entry.
# ---------------------------------------------------------------------------
codecov_pairs=$(awk '
  /^[[:space:]]*project:[[:space:]]*$/ { in_project = 1; next }
  in_project && /^[a-z]/ { in_project = 0 }  # dedent to a new top-level key
  !in_project { next }

  # A project entry label is indented exactly 6 spaces, e.g. "      server:"
  /^      [a-zA-Z0-9_]+:[[:space:]]*$/ {
    label = $0
    sub(/^      /, "", label)
    sub(/:[[:space:]]*$/, "", label)
    target = ""
    path = ""
    next
  }
  /^        target:/ {
    line = $0
    sub(/^        target:[[:space:]]*/, "", line)
    gsub(/%/, "", line)
    gsub(/[[:space:]]/, "", line)
    target = line
    next
  }
  /^          - internal\// {
    line = $0
    sub(/^          - /, "", line)
    gsub(/[[:space:]]/, "", line)
    path = line
    if (label != "default" && path != "" && target != "" && target != "auto") {
      print path, target, label
    }
    next
  }
' "$CODECOV_FILE")

codecov_count=$(printf '%s\n' "$codecov_pairs" | grep -c . || true)
[ "$codecov_count" -ge 1 ] 2>/dev/null \
  || setup_error "parsed zero project entries from $CODECOV_FILE -- format changed? extractor needs updating"

# ---------------------------------------------------------------------------
# Compare.
# ---------------------------------------------------------------------------
fail=0

# 1. Every floor entry has a matching codecov.yml entry with an equal target.
while read -r fpath fval; do
  [ -z "$fpath" ] && continue
  match=$(printf '%s\n' "$codecov_pairs" | awk -v p="$fpath" '$1 == p { print $2, $3; exit }')
  if [ -z "$match" ]; then
    echo "FAIL: $fpath is in $FLOOR_FILE (floor=${fval}) but has no matching 'paths:' entry in $CODECOV_FILE" >&2
    fail=1
    continue
  fi
  cval=$(printf '%s\n' "$match" | awk '{print $1}')
  clabel=$(printf '%s\n' "$match" | awk '{print $2}')
  if [ "$cval" != "$fval" ]; then
    echo "FAIL: $fpath diverges -- $FLOOR_FILE floor=${fval}, $CODECOV_FILE '$clabel' target=${cval}" >&2
    fail=1
  fi
done <<EOF
$floor_pairs
EOF

# 2. Every codecov.yml project entry maps back to a floor entry (catches a
#    stale codecov.yml entry left behind after a floor entry is removed).
while read -r cpath cval clabel; do
  [ -z "$cpath" ] && continue
  in_floor=$(printf '%s\n' "$floor_pairs" | awk -v p="$cpath" '$1 == p { print "yes"; exit }')
  if [ -z "$in_floor" ]; then
    echo "FAIL: $CODECOV_FILE entry '$clabel' (paths: $cpath, target=${cval}) has no matching entry in $FLOOR_FILE" >&2
    fail=1
  fi
done <<EOF
$codecov_pairs
EOF

if [ "$fail" -ne 0 ]; then
  echo "" >&2
  echo "codecov.yml's coverage.status.project targets have drifted from $FLOOR_FILE." >&2
  echo "Reconcile them (usually: copy the floor JSON's value into the matching codecov.yml target)." >&2
  exit 1
fi

echo "OK: codecov.yml project targets mirror $FLOOR_FILE ($floor_count entries)."
