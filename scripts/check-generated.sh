#!/bin/bash
# check-generated.sh -- verify *_templ.go files were regenerated after .templ changes
set -euo pipefail

base=$(git merge-base main HEAD 2>/dev/null || echo "HEAD~1")
templ_changed=$(git diff --name-only "$base"..HEAD -- '*.templ')
gen_changed=$(git diff --name-only "$base"..HEAD -- '*_templ.go')

missing=""
while IFS= read -r templ_file; do
  [ -z "$templ_file" ] && continue
  expected="${templ_file%.templ}_templ.go"
  if ! echo "$gen_changed" | grep -qxF "$expected"; then
    missing="${missing}  $templ_file\n"
  fi
done <<< "$templ_changed"

if [ -n "$missing" ]; then
  echo "ERROR: .templ files changed but corresponding *_templ.go not regenerated. Run: templ generate"
  echo "Missing regeneration for:"
  printf "%b" "$missing"
  exit 1
fi

# Verify the docs provider matrix is in sync with the live registry. The
# generator runs in -check mode and exits non-zero if regeneration is needed.
# Skip silently if the docs file is absent (e.g., a docs-stripped checkout).
if [ -f docs/site/src/reference/providers.md ]; then
  if ! go run ./cmd/gen-provider-matrix -check; then
    echo "ERROR: docs/site/src/reference/providers.md is stale. Run: make generate-docs"
    exit 1
  fi
fi

# Verify the env-var reference is in sync with the config struct tags. The
# generator runs in -check mode and exits non-zero if regeneration is needed.
# Skip silently if the docs file is absent (e.g., a docs-stripped checkout).
if [ -f docs/site/src/reference/environment-variables.md ]; then
  if ! go run ./cmd/gen-env-reference -check; then
    echo "ERROR: docs/site/src/reference/environment-variables.md is stale. Run: make generate-docs"
    exit 1
  fi
fi

# Verify the rules catalogue is in sync with the built-in rule definitions.
# Skip silently if the docs file is absent (e.g., a docs-stripped checkout).
if [ -f docs/site/src/reference/rules-catalogue.md ]; then
  if ! go run ./cmd/gen-rules-catalogue -check; then
    echo "ERROR: docs/site/src/reference/rules-catalogue.md is stale. Run: make generate-docs"
    exit 1
  fi
fi

echo "Generated files: OK"
