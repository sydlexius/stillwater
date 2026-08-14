#!/usr/bin/env bash
# check-goreleaser-extra-files.sh -- assert every repo-file `COPY` source in
# build/docker/Dockerfile.goreleaser is also listed in .goreleaser.yml's
# `extra_files:` (#3034 regression).
#
# THE BUG THIS EXISTS FOR. dockers_v2's buildx context for
# Dockerfile.goreleaser is assembled ONLY from the platform-staged binaries
# (under $TARGETPLATFORM/) plus whatever .goreleaser.yml's `extra_files:`
# lists -- it is NOT the repo checkout. A commit added
# `COPY build/docker/healthcheck.sh /healthcheck.sh` to
# Dockerfile.goreleaser without adding the file to `extra_files:`. Nothing in
# the existing gate caught it: hadolint and shellcheck don't know goreleaser's
# context-assembly rule, and `docker build` against the primary Dockerfile
# builds a different file (Dockerfile, not Dockerfile.goreleaser) with a real
# builder-stage COPY, not an extra_files-staged one. The gap was invisible
# until the next real `goreleaser release`, where it fails Docker image
# assembly for every platform. This script closes that gap mechanically
# rather than relying on the sync-contract comment alone, which did not save
# us here because the thing needing mirroring was in a THIRD file the
# comment's contract never mentioned.
#
# WHAT IT CHECKS. Every `COPY <src> ...` line in Dockerfile.goreleaser whose
# source is a plain repo-relative path (i.e. not a $TARGETPLATFORM/-staged
# platform binary, and not a --from= builder-stage copy) must appear, verbatim,
# as a `- <src>` entry under `extra_files:` in .goreleaser.yml.
#
# Written in awk/grep, Bash 3.2 compatible (stock macOS /bin/bash), matching
# the no-jq/no-`declare -A`/no-`mapfile` convention used by the gate's other
# checks (coverage-floor.sh, check-codecov-floor-mirror.sh).
#
# Exit codes:
#   0  every repo-file COPY source in Dockerfile.goreleaser is listed in
#      .goreleaser.yml's extra_files:.
#   1  one or more COPY sources are missing from extra_files: (reported
#      individually, not just the first).
#   2  setup error: a required file is missing or could not be parsed at all.
set -euo pipefail

ROOT="$(git rev-parse --show-toplevel 2>/dev/null || pwd)"
cd "$ROOT"

DOCKERFILE="build/docker/Dockerfile.goreleaser"
GORELEASER_FILE=".goreleaser.yml"

setup_error() { echo "check-goreleaser-extra-files: setup error: $1" >&2; exit 2; }

[ -f "$DOCKERFILE" ] || setup_error "missing $DOCKERFILE"
[ -f "$GORELEASER_FILE" ] || setup_error "missing $GORELEASER_FILE"
[ -s "$DOCKERFILE" ] || setup_error "$DOCKERFILE is empty"
[ -s "$GORELEASER_FILE" ] || setup_error "$GORELEASER_FILE is empty"

# ---------------------------------------------------------------------------
# Extract repo-file COPY sources from Dockerfile.goreleaser.
#
# `COPY $TARGETPLATFORM/stillwater ...` is the goreleaser-staged platform
# binary, not a repo file -- skipped.
# `COPY --from=... ...` is a builder-stage copy, not present in this
# builder-less Dockerfile, but skipped defensively anyway.
# Everything else is a repo-relative source that must flow through
# extra_files:.
# ---------------------------------------------------------------------------
copy_sources=$(awk '
  /^[[:space:]]*COPY[[:space:]]/ {
    line = $0
    sub(/^[[:space:]]*COPY[[:space:]]+/, "", line)
    if (line ~ /^--from=/) next
    n = split(line, parts, /[[:space:]]+/)
    src = parts[1]
    if (src ~ /^\$TARGETPLATFORM\//) next
    print src
  }
' "$DOCKERFILE")

copy_count=$(printf '%s\n' "$copy_sources" | grep -c . || true)
[ "$copy_count" -ge 1 ] 2>/dev/null \
  || setup_error "parsed zero repo-file COPY sources from $DOCKERFILE -- format changed? extractor needs updating"

# ---------------------------------------------------------------------------
# Extract extra_files: entries from .goreleaser.yml. Scoped to the single
# `extra_files:` block under `dockers_v2:` (only one exists today); a bare
# `- path` line following the `extra_files:` key, ending at dedent.
# ---------------------------------------------------------------------------
extra_files=$(awk '
  /^[[:space:]]*extra_files:[[:space:]]*$/ { in_block = 1; next }
  in_block {
    if ($0 !~ /^[[:space:]]*-[[:space:]]*/) { in_block = 0; next }
    line = $0
    sub(/^[[:space:]]*-[[:space:]]*/, "", line)
    gsub(/[[:space:]]+$/, "", line)
    print line
  }
' "$GORELEASER_FILE")

extra_count=$(printf '%s\n' "$extra_files" | grep -c . || true)
[ "$extra_count" -ge 1 ] 2>/dev/null \
  || setup_error "parsed zero extra_files: entries from $GORELEASER_FILE -- format changed? extractor needs updating"

# ---------------------------------------------------------------------------
# Compare: every COPY source must be listed in extra_files.
# ---------------------------------------------------------------------------
fail=0

while read -r src; do
  [ -z "$src" ] && continue
  if ! printf '%s\n' "$extra_files" | grep -qxF "$src"; then
    echo "FAIL: $DOCKERFILE has 'COPY $src ...' but '$src' is not listed under extra_files: in $GORELEASER_FILE" >&2
    fail=1
  fi
done <<EOF
$copy_sources
EOF

if [ "$fail" -ne 0 ]; then
  echo "" >&2
  echo "A repo file COPYed in $DOCKERFILE will not exist in goreleaser's" >&2
  echo "buildx context unless it is also listed under extra_files: in" >&2
  echo "$GORELEASER_FILE. Add it (see #3034)." >&2
  exit 1
fi

echo "OK: every repo-file COPY source in $DOCKERFILE is listed in $GORELEASER_FILE extra_files: ($copy_count checked)."
