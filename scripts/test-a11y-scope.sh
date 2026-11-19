#!/bin/bash
# test-a11y-scope.sh -- tests for scripts/a11y-scope.sh and its Makefile wiring (#3608).
# Run: bash scripts/test-a11y-scope.sh
set -euo pipefail
D="$(cd "$(dirname "$0")" && pwd)"
SCOPE="$D/a11y-scope.sh"
T="$(mktemp -d)"
trap 'rm -r "$T"' EXIT
rc=0
ok() { echo "PASS: $1"; }
bad() { echo "FAIL: $1"; [ -n "${2:-}" ] && printf '%s\n' "$2" | sed 's/^/      /'; rc=1; }

# run <SPEC> <PROJECT> -> sets OUT, ST
run() { ST=0; OUT=$(SPEC="$1" PROJECT="$2" bash "$SCOPE" 2>&1) || ST=$?; }

run "" ""
{ [ "$ST" -eq 0 ] && [ -z "$OUT" ]; } && ok "unset SPEC/PROJECT prints nothing (default run unchanged)" || bad "unset scope" "$OUT"

run "tests/a11y/contrast.spec.js" ""
if [ "$ST" -eq 0 ] && [ "$OUT" = '/tests/a11y/contrast\.spec\.js$' ]; then ok "literal spec becomes an anchored escaped regex"
else bad "anchored regex" "$OUT"; fi

run "tests/a11y/*contrast*.spec.js" ""
if [ "$ST" -eq 0 ] && [ "$(wc -l <<<"$OUT")" -ge 3 ] && ! grep -qv '^/tests/a11y/.*\\\.spec\\\.js\$$' <<<"$OUT"; then ok "glob expands to one anchored regex per file"
else bad "glob expansion" "$OUT"; fi

run "tests/a11y/cheat-sheet.spec.js,tests/a11y/cheat-*.spec.js tests/a11y/cheat-sheet.spec.js" ""
if [ "$ST" -eq 0 ] && [ "$(wc -l <<<"$OUT")" -eq 1 ]; then ok "list (comma + space) is de-duplicated"; else bad "dedupe" "$OUT"; fi

for p in "firefox:--project=firefox-a11y" "firefox-a11y:--project=firefox-a11y" "chromium:--project=chromium-a11y" "chromium-a11y:--project=chromium-a11y"; do
  run "" "${p%%:*}"
  if [ "$ST" -eq 0 ] && [ "$OUT" = "${p#*:}" ]; then ok "PROJECT=${p%%:*} maps to ${p#*:}"; else bad "PROJECT ${p%%:*}" "$OUT"; fi
done
run "" "webkit"
if [ "$ST" -eq 2 ]; then ok "unknown PROJECT exits 2"; else bad "bogus PROJECT (st=$ST)" "$OUT"; fi

# Injection / path tokens: refused with 2 AND the canary command never ran.
n=0
for tok in 'tests/a11y/x.spec.js;touch CANARY' 'tests/a11y/$(touch CANARY).spec.js' 'tests/a11y/`touch CANARY`.spec.js' \
  'tests/a11y/../../etc/x.spec.js' '/etc/passwd.spec.js' 'tests/a11y/contrast.js' 'package.json' 'tests/a11y/a|touch CANARY'; do
  n=$((n + 1))
  ST=0; OUT=$(cd "$T" && SPEC="$tok" bash "$SCOPE" 2>&1) || ST=$?
  if [ "$ST" -eq 2 ] && [ ! -e "$T/CANARY" ]; then ok "refuses token #$n"; else bad "token #$n '$tok' (st=$ST)" "$OUT"; fi
done

run "tests/a11y/no-such-spec-*.spec.js" ""
if [ "$ST" -eq 2 ]; then ok "token matching nothing exits 2 (never an unfiltered run)"; else bad "zero-match (st=$ST)" "$OUT"; fi
run "tests/a11y/cheat-sheet.spec.js tests/a11y/nope.spec.js" ""
if [ "$ST" -eq 2 ]; then ok "one good + one dead token still exits 2"; else bad "mixed (st=$ST)" "$OUT"; fi

# Makefile wiring (dry run: prints the recipe, executes nothing).
M=$(cd "$D/.." && make -n test-a11y SPEC='tests/a11y/cheat-sheet.spec.js' 2>&1 || true)
if grep -q 'bash scripts/a11y-scope.sh' <<<"$M" && grep -q 'SCOPE_ARGS\[@\]' <<<"$M"; then ok "Makefile test-a11y consumes a11y-scope.sh output"; else bad "Makefile wiring" "$M"; fi
exit "$rc"
