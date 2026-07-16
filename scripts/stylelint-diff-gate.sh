#!/bin/bash
# stylelint-diff-gate.sh -- diff-scoped stylelint ratchet for the hand-written
# CSS layer (design-tokens.css, input.css, scalar-theme.css).
#
# The design-token migration (#2402) is not complete: input.css alone still
# carries ~241 pre-existing literal-color declarations that stylelint's
# declaration-strict-value rule would otherwise flag file-wide. Failing the
# build on every pre-existing violation would block all CSS work behind a
# large, unrelated migration. Instead this is a ONE-WAY RATCHET (same shape
# as coverage-floor.sh / patch-coverage.sh): a violation on a line the diff
# did NOT touch is ignored; a violation on a line the diff ADDED fails the
# build. Existing debt can only be paid down, never added to.
#
# Usage:
#   stylelint-diff-gate.sh [BASE]
#
# BASE defaults to the merge-base against origin/main (falling back to the
# local main branch, then HEAD~1) -- see resolve_base() below. Pass BASE
# explicitly to check against a specific ref (e.g. a PR's actual base SHA).
#
# Exit status:
#   0 = no NEW violations on added lines (pre-existing violations may remain)
#   1 = at least one NEW violation on an added line
#   2 = invalid input / setup state (BASE unresolvable, stylelint/jq missing)
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
REPO_ROOT="$(cd "$SCRIPT_DIR/.." && pwd)"
cd "$REPO_ROOT"

CSS_GLOB='web/static/css/*.css'

resolve_base() {
  if [ -n "${1:-}" ]; then
    echo "$1"
    return 0
  fi
  if git rev-parse --verify -q origin/main >/dev/null 2>&1; then
    git merge-base origin/main HEAD
    return 0
  fi
  if git rev-parse --verify -q main >/dev/null 2>&1; then
    git merge-base main HEAD
    return 0
  fi
  git rev-parse HEAD~1
}

BASE="$(resolve_base "${1:-}")"

if ! git rev-parse --verify -q "$BASE^{commit}" >/dev/null; then
  echo "FAIL: cannot resolve BASE ('$BASE') to a commit; aborting gate" >&2
  exit 2
fi

if ! command -v jq >/dev/null 2>&1; then
  echo "FAIL: jq not in PATH" >&2
  exit 2
fi

if ! npx --no-install stylelint --version >/dev/null 2>&1; then
  echo "FAIL: stylelint not installed (run: npm ci)" >&2
  exit 2
fi

WORK_DIR="$(mktemp -d)"
trap 'rm -rf "$WORK_DIR"' EXIT

ADDED_LINES="$WORK_DIR/added-lines.txt"
: > "$ADDED_LINES"

# Parse `git diff --unified=0` for this BASE..HEAD over the CSS glob into a
# flat "file:line" list of every line the diff ADDED. --unified=0 emits only
# hunk headers and +/- lines (no context), so the running line counter only
# needs to advance on '+' lines; deletions ('-') don't consume a new-file
# line number and are skipped.
current_file=""
current_line=0
while IFS= read -r diff_line; do
  case "$diff_line" in
    "diff --git "*)
      # `diff --git a/<path> b/<path>` -- take the b/ side (post-change path).
      current_file="${diff_line#*" b/"}"
      ;;
    "@@ "*)
      # `@@ -<old>[,<count>] +<new>[,<count>] @@...` -- start counting from
      # the new-file line number.
      current_line="${diff_line#*+}"
      current_line="${current_line%%[, ]*}"
      ;;
    "+++"*) ;; # file-header noise, not a real added line
    "+"*)
      echo "${current_file}:${current_line}" >> "$ADDED_LINES"
      current_line=$((current_line + 1))
      ;;
  esac
done < <(git diff --unified=0 "$BASE" -- "$CSS_GLOB")

if [ ! -s "$ADDED_LINES" ]; then
  echo "No added lines in $CSS_GLOB since $BASE -- nothing to check. OK"
  exit 0
fi

STYLELINT_JSON="$WORK_DIR/stylelint.json"
# stylelint exits non-zero when it finds ANY violation (including
# pre-existing ones on untouched lines) -- that's expected here, so don't let
# `set -e` abort before we get to filter the output against $ADDED_LINES.
# It also writes the --formatter json report to STDERR (not stdout) whenever
# it exits non-zero, so both streams must be captured or the report is lost.
npx --no-install stylelint --formatter json "$CSS_GLOB" > "$STYLELINT_JSON" 2>&1 || true

# Reduce stylelint's JSON to one "relative/path:line" per warning, then keep
# only the ones present in $ADDED_LINES.
ALL_VIOLATIONS="$WORK_DIR/all-violations.txt"
jq -r --arg root "$REPO_ROOT/" '
  .[] | (.source | ltrimstr($root)) as $rel | .warnings[] | "\($rel):\(.line)\t\(.text)"
' "$STYLELINT_JSON" > "$ALL_VIOLATIONS"

NEW_VIOLATIONS="$WORK_DIR/new-violations.txt"
: > "$NEW_VIOLATIONS"
while IFS=$'\t' read -r loc text; do
  if grep -qxF "$loc" "$ADDED_LINES"; then
    printf '%s\t%s\n' "$loc" "$text" >> "$NEW_VIOLATIONS"
  fi
done < "$ALL_VIOLATIONS"

TOTAL=$(wc -l < "$ALL_VIOLATIONS" | tr -d ' ')
NEW=$(wc -l < "$NEW_VIOLATIONS" | tr -d ' ')

if [ "$NEW" -gt 0 ]; then
  echo "FAIL: $NEW new stylelint violation(s) on lines added since $BASE (of $TOTAL total, including pre-existing debt):" >&2
  while IFS=$'\t' read -r loc text; do
    echo "  $loc  $text" >&2
  done < "$NEW_VIOLATIONS"
  exit 1
fi

echo "OK: 0 new stylelint violations on added lines ($TOTAL pre-existing violation(s) untouched by this diff)"
exit 0
