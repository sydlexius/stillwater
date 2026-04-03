#!/bin/bash
# check-generated.sh -- verify *_templ.go files were regenerated after .templ changes
set -euo pipefail

base=$(git merge-base main HEAD 2>/dev/null || echo "HEAD~1")
templ_changed=$(git diff --name-only "$base"..HEAD -- '*.templ')
gen_changed=$(git diff --name-only "$base"..HEAD -- '*_templ.go')

if [ -n "$templ_changed" ] && [ -z "$gen_changed" ]; then
  echo "ERROR: .templ files changed but *_templ.go not regenerated. Run: templ generate"
  echo "Changed .templ files:"
  echo "$templ_changed"
  exit 1
fi

echo "Generated files: OK"
