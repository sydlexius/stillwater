#!/bin/bash
# pre-push-gate.sh -- deterministic pre-push checks; run before code review
# Exit status: 0 = all hard checks passed; 1 = a hard check failed
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
BASE=$(git merge-base main HEAD 2>/dev/null || echo "HEAD~1")

echo "=== Tests ==="
go test -count=1 ./...

echo ""
echo "=== OpenAPI consistency ==="
go test -count=1 -run TestOpenAPIConsistency -v ./internal/api/

echo ""
echo "=== Generated files ==="
bash "$SCRIPT_DIR/check-generated.sh"

echo ""
echo "=== Raw error leak check ==="
error_leaks=$(git diff "$BASE"..HEAD -- 'internal/api/handlers_*.go' \
  | grep '^+' \
  | grep -E 'err\.(Error|String)\(\)|fmt\.(Sprintf|Errorf).*err[^o]' \
  | grep -v 'slog\.\|logger\.\|log\.' || true)
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
  if git show main:internal/api/openapi.yaml > /tmp/openapi-base.yaml 2>/dev/null; then
    breaking=$(oasdiff breaking /tmp/openapi-base.yaml internal/api/openapi.yaml 2>&1 || true)
    rm -f /tmp/openapi-base.yaml
    if [ -n "$breaking" ]; then
      echo "WARNING: Breaking OpenAPI changes detected (may be intentional):"
      echo "$breaking"
    else
      echo "No breaking changes."
    fi
  else
    rm -f /tmp/openapi-base.yaml
    echo "Skipped (openapi.yaml not yet on main)."
  fi
else
  echo "Skipped (oasdiff not installed -- install: go install github.com/oasdiff/oasdiff@latest)"
fi

echo ""
echo "All hard checks passed. Proceed with /pr-review-toolkit:review-pr."
