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
echo "=== Coverage on changed files ==="
changed_go=$(git diff "$BASE"..HEAD --name-only -- '*.go' | grep -v '_test.go' || true)
if [ -z "$changed_go" ]; then
  echo "Skipped (no Go source changes)."
else
  if ! cover_funcs=$(go tool cover -func="$COVER_OUT" 2>/dev/null); then
    echo "WARNING: Unable to parse coverage profile; skipping changed-file coverage check."
    echo "Codecov will still validate patch coverage in CI."
    cover_funcs=""
  fi
  uncovered=""
  while IFS= read -r f; do
    hits=$(printf '%s\n' "$cover_funcs" | grep -F "/${f}:" | awk '$NF == "0.0%"' || true)
    if [ -n "$hits" ]; then
      uncovered="${uncovered}${hits}"$'\n'
    fi
  done <<< "$changed_go"

  if [ -z "$cover_funcs" ]; then
    :
  elif [ -n "$uncovered" ]; then
    echo "WARNING: Functions with 0% coverage in changed files:"
    echo "$uncovered"
    echo "These may be intentionally tested via integration or missing unit tests."
    echo "Codecov will flag patch coverage -- consider adding tests before opening a PR."
  else
    echo "OK"
  fi
fi

echo ""
echo "All hard checks passed. Proceed with /pr-review-toolkit:review-pr."
