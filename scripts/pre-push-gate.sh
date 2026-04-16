#!/bin/bash
# pre-push-gate.sh -- deterministic pre-push checks; run before code review
# Exit status: 0 = all hard checks passed; 1 = a hard check failed
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
BASE=$(git merge-base main HEAD 2>/dev/null || echo "HEAD~1")

COVER_OUT=$(mktemp /tmp/stillwater-cover.XXXXXX.out)
tmp_openapi=""
cleanup() {
  rm -f "${COVER_OUT:-}" "${tmp_openapi:-}"
}
trap cleanup EXIT

echo "=== Tests ==="
go test -race -count=1 -covermode=atomic -coverprofile="$COVER_OUT" ./...

echo ""
echo "=== OpenAPI consistency ==="
go test -count=1 -run TestOpenAPIConsistency -v ./internal/api/

echo ""
echo "=== Generated files ==="
bash "$SCRIPT_DIR/check-generated.sh"

echo ""
echo "=== Raw error leak check ==="
error_leaks=$(git diff "$BASE"..HEAD -- 'internal/api/handlers.go' 'internal/api/handlers_*.go' \
  | grep '^+' \
  | grep -E 'err\.(Error|String)\(\)' \
  | grep -vE '\bslog\.|\blogger\.|\blog\.' || true)
if [ -n "$error_leaks" ]; then
  echo "CRITICAL: Raw error text may be leaking to clients:"
  echo "$error_leaks"
  echo ""
  echo "Client-visible messages must be generic. Log full errors server-side with slog."
  exit 1
fi
echo "OK"

echo ""
echo "=== OpenAPI breaking changes ==="
if command -v oasdiff &>/dev/null; then
  tmp_openapi=$(mktemp /tmp/openapi-base.XXXXXX.yaml)
  if git show main:internal/api/openapi.yaml > "$tmp_openapi" 2>/dev/null; then
    breaking=$(oasdiff breaking "$tmp_openapi" internal/api/openapi.yaml 2>&1 || true)
    if [ -n "$breaking" ]; then
      echo "WARNING: Breaking OpenAPI changes detected (may be intentional):"
      echo "$breaking"
    else
      echo "No breaking changes."
    fi
  else
    echo "Skipped (openapi.yaml not yet on main)."
  fi
else
  echo "Skipped (oasdiff not installed -- install: go install github.com/oasdiff/oasdiff@latest)"
fi

echo ""
echo "=== Patch coverage ==="
# Matches codecov.yml's 70% patch threshold. The script approximates the
# same semantics locally so we catch gaps before push instead of learning
# about them from a failing codecov check.
#
# patch-coverage.sh uses exit codes 0|1|2 (2 = config error). This wrapper
# is documented as 0|1, so collapse any non-zero child status to 1. Using
# an `if` here (rather than calling the script bare under `set -e`) lets
# us capture the exit code without the shell bailing out first.
if COVER_OUT="$COVER_OUT" BASE="$BASE" PATCH_COVERAGE_THRESHOLD=70 \
    bash "$SCRIPT_DIR/patch-coverage.sh"; then
  :
else
  exit 1
fi

echo ""
echo "All hard checks passed. Proceed with /pr-review-toolkit:review-pr."
