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
  echo "ERROR: .templ files changed but corresponding *_templ.go not regenerated. Run: go tool templ generate"
  echo "Missing regeneration for:"
  printf "%b" "$missing"
  exit 1
fi

# Content freshness check: regenerate all *_templ.go in a clean state and fail
# if anything differs. This catches stale content from a wrong-version templ
# binary -- not just a missing file -- and ensures the committed output matches
# the pinned version in go.mod.
if ! go tool templ generate 2>/tmp/check-generated-templ.log; then
  echo "ERROR: 'go tool templ generate' failed:"
  cat /tmp/check-generated-templ.log
  exit 1
fi
dirty_templ=$(git diff --name-only -- '*_templ.go' || true)
if [ -n "$dirty_templ" ]; then
  echo "ERROR: *_templ.go files are stale or were generated with a different templ version."
  echo "Run: go tool templ generate && git add <generated files>"
  echo "Stale files:"
  echo "$dirty_templ" | sed 's/^/  /'
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

# Verify the settings reference is in sync with the i18n locale + templ panel
# scan. The companion anchors file is the contract consumed by the in-app
# HelpHint component (#1132); checking it here catches drift that would later
# surface as broken HelpHint deep links. Skip silently if the docs file is
# absent (e.g., a docs-stripped checkout).
if [ -f docs/site/src/reference/settings-by-tab.md ]; then
  if ! go run ./cmd/gen-settings-reference -check; then
    echo "ERROR: docs/site/src/reference/settings-by-tab.md or _settings-anchors.txt is stale. Run: make generate-docs"
    exit 1
  fi
fi

# Verify the doc-anchors file is in sync with the docs tree. The generator
# runs in -check mode and exits non-zero if regeneration is needed. Skip
# silently if neither anchors file is present (e.g., a docs-stripped checkout).
if [ -f docs/site/src/reference/_doc-anchors.txt ] || [ -f web/components/_doc-anchors.txt ]; then
  if ! go run ./cmd/gen-doc-anchors -check; then
    echo "ERROR: _doc-anchors.txt is stale. Run: make generate-docs"
    exit 1
  fi
fi

# Verify the envelope-versions file is in sync with the CurrentEnvelopeVersion
# doc-comment in internal/settingsio/export.go. Skip silently if the generated
# file is absent (e.g., a docs-stripped checkout).
if [ -f docs/_generated/envelope-versions.md ]; then
  if ! go run ./cmd/gen-envelope-changelog -check; then
    echo "ERROR: docs/_generated/envelope-versions.md is stale. Run: make generate-docs"
    exit 1
  fi
fi

echo "Generated files: OK"
