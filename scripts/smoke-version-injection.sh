#!/usr/bin/env bash
# smoke-version-injection.sh -- version-ldflags-injection smoke test
#
# Go's linker treats an unresolved `-X` symbol path as a silent no-op: a
# stale/misspelled `-X path=value` does not fail the build, it just leaves
# the target variable at its zero value. That means a release pipeline
# whose ldflags symbol path has drifted from internal/version's actual
# var path would ship a binary reporting a blank/default version with no
# build-time signal.
#
# This script extracts the `-X ...version.Version=` symbol path from a
# caller-supplied source file, rebuilds the real binary with that exact
# symbol path set to a distinctive test token, and asserts the built
# binary's `version` subcommand output actually contains that token.
#
# The symbol path is duplicated across multiple release-path configs that
# do NOT share a single file (stable release.yml builds with an inline
# shell -ldflags string; nightly builds via goreleaser reading
# .goreleaser.nightly.yml; .goreleaser.yml is not invoked by either CI path
# today). Each caller must therefore point this script at the file it
# actually builds from, via --source, so the smoke test actually guards
# the config it is meant to guard rather than an unrelated one.
#
# Usage:
#   bash scripts/smoke-version-injection.sh --source <path>
#   bash scripts/smoke-version-injection.sh   # defaults to .goreleaser.yml
#
# Exit codes:
#   0 -- version injection verified working
#   1 -- injected version did not surface (blank output or drifted symbol)
#   2 -- setup/infrastructure failure (build failed, source file unreadable)

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
. "$SCRIPT_DIR/lib/run-paths.sh"

REPO_ROOT="$(cd "$SCRIPT_DIR/.." && pwd)"
SOURCE_FILE="$REPO_ROOT/.goreleaser.yml"
TEST_TOKEN="9.9.9-test"

while [[ $# -gt 0 ]]; do
  case "$1" in
    --source)
      SOURCE_FILE="$2"
      shift 2
      ;;
    --source=*)
      SOURCE_FILE="${1#--source=}"
      shift
      ;;
    -h | --help)
      sed -n '/^# Usage:/,/^# *$/p' "$0" | sed 's/^# \{0,1\}//'
      exit 0
      ;;
    *)
      echo "smoke-version-injection.sh: unknown argument '$1' (try --source <path> or --help)" >&2
      exit 2
      ;;
  esac
done

# Resolve relative --source values against the repo root so the script works
# the same whether invoked from the repo root (CI) or elsewhere.
if [[ "$SOURCE_FILE" != /* ]]; then
  SOURCE_FILE="$REPO_ROOT/$SOURCE_FILE"
fi

PASS=0
FAIL=0
FAILURES=()

assert_pass() {
  echo "[PASS] $1"
  PASS=$((PASS + 1))
}

assert_fail() {
  echo "[FAIL] $1 -- $2"
  FAIL=$((FAIL + 1))
  FAILURES+=("$1 -- $2")
}

echo "======================================================="
echo "  Version Injection Smoke Test"
echo "  Source: $SOURCE_FILE"
echo "======================================================="
echo ""

# ---------------------------------------------------------------------------
# Extract the -X symbol path for version.Version from the source file this
# invocation is guarding. Matching the extraction style already used by
# scripts/check-tool-versions.sh (grep -oE / sed, no YAML parser dependency).
# Tolerant of "-X path=val", "-X=path=val", and quoted forms so a benign
# reformat of the ldflags line does not false-fail a release.
# ---------------------------------------------------------------------------

if [[ ! -f "$SOURCE_FILE" ]]; then
  echo "FATAL: $SOURCE_FILE not found." >&2
  exit 2
fi

SYMBOL_TOKEN=$(grep -oE -- '-X[[:space:]=]+"?[A-Za-z0-9./_-]+\.Version=' "$SOURCE_FILE" | head -1 || true)
if [[ -z "$SYMBOL_TOKEN" ]]; then
  echo "FATAL: could not find a '-X <symbol>.Version=' ldflags entry in $SOURCE_FILE." >&2
  echo "       This itself indicates the release config has drifted from expectations." >&2
  exit 2
fi

SYMBOL=$(printf '%s' "$SYMBOL_TOKEN" | sed -E 's/^-X[[:space:]=]+"?//; s/=$//')
echo "Extracted symbol path: $SYMBOL"
echo ""

# ---------------------------------------------------------------------------
# Build a probe binary with the real symbol path injecting the test token.
# ---------------------------------------------------------------------------

PROBE_BIN="$SW_RUN_DIR/vis-probe"
BUILD_LOG="$SW_RUN_DIR/vis-build.log"

echo "Building probe binary..."
if ! (cd "$REPO_ROOT" && go build -ldflags "-X ${SYMBOL}=${TEST_TOKEN}" -o "$PROBE_BIN" ./cmd/stillwater) >"$BUILD_LOG" 2>&1; then
  echo "FATAL: go build failed. Log:" >&2
  cat "$BUILD_LOG" >&2
  exit 2
fi
echo "Probe built at $PROBE_BIN"
echo ""

# ---------------------------------------------------------------------------
# Run the probe and assert the injected token surfaced.
# ---------------------------------------------------------------------------

echo "--- Assertion ---"
echo ""

OUTPUT=$("$PROBE_BIN" version 2>&1 || true)
echo "Probe output: ${OUTPUT:-<blank>}"

if [[ -z "$OUTPUT" ]]; then
  assert_fail "probe 'version' output" "output was blank (silent linker no-op)"
elif [[ "$OUTPUT" != *"$TEST_TOKEN"* ]]; then
  assert_fail "probe 'version' output" "expected to contain '$TEST_TOKEN', got: $OUTPUT"
else
  assert_pass "probe 'version' output contains injected token '$TEST_TOKEN'"
fi
echo ""

# ---------------------------------------------------------------------------
# Summary
# ---------------------------------------------------------------------------

TOTAL=$((PASS + FAIL))
echo "======================================================="
echo "=== RESULTS: $PASS passed, $FAIL failed (of $TOTAL checks) ==="
echo "======================================================="

if [[ ${#FAILURES[@]} -gt 0 ]]; then
  echo ""
  echo "FAILED:"
  for f in "${FAILURES[@]}"; do
    echo "  $f"
  done
  echo ""
  exit 1
fi

echo ""
echo "Version injection verified."
exit 0
