#!/bin/bash
# test-check-pr-trigger-scope.sh -- mutation tests for check-pr-trigger-scope.sh.
# Run: bash scripts/test-check-pr-trigger-scope.sh
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
GUARD="$SCRIPT_DIR/check-pr-trigger-scope.sh"
TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT

pass=0
fail=0

# expect <want> <name> <dir>
expect() {
  local want="$1" name="$2" dir="$3" out status
  out=$(bash "$GUARD" "$dir" 2>&1) && status=0 || status=$?
  if [ "$status" -eq "$want" ]; then
    pass=$((pass + 1)); echo "PASS: $name"
  else
    fail=$((fail + 1)); echo "FAIL: $name (expected exit $want, got $status)"
    printf '%s\n' "$out" | sed 's/^/      /'
  fi
}

GUARDED="ci bruno-ci security codeql gate signed-commits"
PUSH='  push:\n    branches: [main]\n'

# fixture <dir> : compliant workflows (push restricted, PR unrestricted)
fixture() {
  local w
  mkdir -p "$1"
  for w in $GUARDED; do
    printf 'name: x\non:\n%b  pull_request:\n    types: [opened]\npermissions: {}\n' "$PUSH" > "$1/$w.yml"
  done
}
# put <dir> <file> <on-body> : overwrite one workflow
put() { printf 'name: x\non:\n%b' "$3" > "$1/$2.yml"; }

fixture "$TMP/clean"
expect 0 "compliant fixture passes" "$TMP/clean"

# Mutation: re-restrict EACH guarded workflow (kills any dropped list entry).
for w in $GUARDED; do
  fixture "$TMP/r-$w"; put "$TMP/r-$w" "$w" "  pull_request:\n    branches: [main]\n"
  expect 1 "rejects branches: [main] in $w" "$TMP/r-$w"
done

fixture "$TMP/m2"; put "$TMP/m2" gate "$PUSH  pull_request:\n    branches:\n      - main\n"
expect 1 "rejects block-list branches" "$TMP/m2"

fixture "$TMP/m3"; put "$TMP/m3" codeql "  pull_request:\n    branches-ignore: [wip]\n"
expect 1 "rejects branches-ignore" "$TMP/m3"

fixture "$TMP/flow"; put "$TMP/flow" ci "  pull_request: {branches: [main]}\n"
expect 1 "rejects one-line flow-map branches" "$TMP/flow"

fixture "$TMP/nopr"; put "$TMP/nopr" signed-commits "$PUSH"
expect 1 "rejects a guarded workflow that lost its pull_request trigger" "$TMP/nopr"

# Default-deny: an UNLISTED new workflow is caught; an EXEMPT one is not.
fixture "$TMP/new"; put "$TMP/new" brand-new "  pull_request:\n    branches: [main]\n"
expect 1 "rejects a new unlisted workflow that restricts the base" "$TMP/new"
fixture "$TMP/ex"; put "$TMP/ex" dependabot-auto-approve "  pull_request:\n    branches: [main]\n"
expect 0 "EXEMPT workflow may restrict the base" "$TMP/ex"
fixture "$TMP/tgt"; put "$TMP/tgt" pr-labels "  pull_request_target:\n    branches: [main]\n"
expect 0 "pull_request_target is out of scope" "$TMP/tgt"

# Push filter and comments must not trip it.
fixture "$TMP/ok"; put "$TMP/ok" security "$PUSH  pull_request:\n    # branches: [main] was removed (#3002)\n    types: [opened]\n"
expect 0 "push filter and comments are ignored" "$TMP/ok"

fixture "$TMP/missing"; rm "$TMP/missing/bruno-ci.yml"
expect 2 "missing guarded workflow is a setup error" "$TMP/missing"

# Live tree: the real workflows must comply.
expect 0 "live workflows comply" "$SCRIPT_DIR/../.github/workflows"

echo "=== RESULTS: $pass passed, $fail failed ==="
[ "$fail" -eq 0 ]
