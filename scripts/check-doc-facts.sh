#!/bin/bash
# check-doc-facts.sh -- assert that hand-written docs cite code-derived facts
# correctly. Part of the pre-push gate (see scripts/pre-push-gate.sh).
#
# Rationale: most reference pages are mechanically generated (cmd/gen-*), but a
# handful of hand-written pages quote a value that lives in code -- a rule
# count, a version constant, a body-size limit. Those drift silently (the
# audit in #1711 found three live-wrong citations). Each check below extracts
# the canonical value from source and fails loudly if a doc citation no longer
# matches. This is a targeted grep guard, not a generator: the surrounding
# prose stays hand-written; only the embedded fact is policed.
#
# Adding a fact: write a check that derives the canonical value from source,
# then compares it to the doc citation, calling `problem` on a mismatch and
# `exit 2` if the canonical value itself cannot be read (a missing source is a
# setup error, not a drift, and must not silently pass).
#
# Exit status: 0 = all facts match; 1 = a doc drifted; 2 = a source-of-truth
# file or pattern could not be read (extractor needs updating).
set -euo pipefail

ROOT=$(git rev-parse --show-toplevel 2>/dev/null || pwd)
cd "$ROOT"

fail=0
problem() { echo "FAIL: $1" >&2; fail=1; }
setup_error() { echo "FAIL: check-doc-facts setup: $1" >&2; exit 2; }
require_file() { [ -f "$1" ] || setup_error "expected file missing: $1"; }

# --- Fact 1: built-in rule count -------------------------------------------
# Canonical: number of Rule entries in defaultRules (internal/rule/service.go).
# Each entry's ID field is a constant (RuleNFOExists, ...), so match the field
# name, not a quoted value.
require_file internal/rule/service.go
rule_count=$(sed -n '/defaultRules = \[\]Rule{/,/^}/p' internal/rule/service.go \
  | grep -cE '^[[:space:]]*ID:' || true)
[ "$rule_count" -ge 1 ] 2>/dev/null \
  || setup_error "could not count defaultRules entries (got '$rule_count'); extractor needs updating"

check_rule_count_doc() {
  local file="$1"
  require_file "$file"
  local claimed
  claimed=$(grep -oiE '[0-9]+ built-in rules' "$file" | grep -oE '[0-9]+' | head -1 || true)
  if [ -z "$claimed" ]; then
    problem "rule count: no 'N built-in rules' claim found in $file (phrasing changed?)"
  elif [ "$claimed" != "$rule_count" ]; then
    problem "rule count: $file says $claimed built-in rules; code has $rule_count (internal/rule/service.go defaultRules)"
  fi
}
check_rule_count_doc docs/site/src/how-to/enable-and-configure-rules.md
check_rule_count_doc docs/site/src/reference/rules-catalogue.md
check_rule_count_doc docs/site/src/reference/index.md

# --- Fact 2: settings envelope version -------------------------------------
require_file internal/settingsio/export.go
env_ver=$(grep -E 'const CurrentEnvelopeVersion' internal/settingsio/export.go \
  | grep -oE '"[0-9]+\.[0-9]+"' | tr -d '"' | head -1 || true)
[ -n "$env_ver" ] || setup_error "could not read CurrentEnvelopeVersion from internal/settingsio/export.go"
exp_doc=docs/site/src/how-to/export-import-settings.md
require_file "$exp_doc"
anchor_ver=$(grep -oE 'CurrentEnvelopeVersion [0-9]+\.[0-9]+' "$exp_doc" \
  | grep -oE '[0-9]+\.[0-9]+' | head -1 || true)
if [ -z "$anchor_ver" ]; then
  problem "envelope version: no 'CurrentEnvelopeVersion X.Y' anchor in $exp_doc (phrasing changed?)"
elif [ "$anchor_ver" != "$env_ver" ]; then
  problem "envelope version: $exp_doc anchor says $anchor_ver; code is $env_ver (internal/settingsio/export.go)"
fi

# --- Fact 3: Go minimum version --------------------------------------------
require_file go.mod
go_ver=$(grep -oE '^go [0-9]+\.[0-9]+(\.[0-9]+)?' go.mod | awk '{print $2}' | head -1 || true)
[ -n "$go_ver" ] || setup_error "could not read the go directive from go.mod"
dev_doc=docs/dev-setup.md
require_file "$dev_doc"
if ! grep -qF "$go_ver" "$dev_doc"; then
  doc_go=$(grep -oiE '\| *Go *\| *[0-9]+\.[0-9.]+' "$dev_doc" | grep -oE '[0-9]+\.[0-9.]+' | head -1 || true)
  problem "Go version: $dev_doc cites ${doc_go:-none}; go.mod requires $go_ver"
fi

# --- Fact 4: reverse-proxy body-size limit ---------------------------------
# Canonical: maxUploadSize is "N << 20" bytes, i.e. N MB. SWAG sample configs
# and the reverse-proxy how-to must agree.
require_file internal/api/handlers_image.go
up_mb=$(grep -E 'maxUploadSize[[:space:]]*=[[:space:]]*[0-9]+[[:space:]]*<<[[:space:]]*20' \
  internal/api/handlers_image.go | grep -oE '[0-9]+' | head -1 || true)
[ -n "$up_mb" ] || setup_error "could not read maxUploadSize from internal/api/handlers_image.go"
for swag in build/swag/stillwater.subdomain.conf.sample build/swag/stillwater.subfolder.conf.sample; do
  require_file "$swag"
  grep -qE "client_max_body_size ${up_mb}m" "$swag" \
    || problem "reverse-proxy: $swag client_max_body_size is not ${up_mb}m (maxUploadSize=${up_mb}MB)"
done
rp_doc=docs/site/src/how-to/reverse-proxy.md
require_file "$rp_doc"
grep -qE "${up_mb} MB" "$rp_doc" \
  || problem "reverse-proxy: $rp_doc does not cite ${up_mb} MB (maxUploadSize)"

# ---------------------------------------------------------------------------
if [ "$fail" -ne 0 ]; then
  echo "" >&2
  echo "Doc facts drifted from code. Update the doc(s) above, or update" >&2
  echo "scripts/check-doc-facts.sh if a source of truth moved." >&2
  exit 1
fi
echo "OK: doc facts match code (rules=$rule_count, envelope=$env_ver, go=$go_ver, upload=${up_mb}MB)."
