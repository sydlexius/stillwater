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

echo "Generated files: OK"
