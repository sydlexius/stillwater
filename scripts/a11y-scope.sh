#!/usr/bin/env bash
# a11y-scope.sh -- turn the SPEC / PROJECT scope of `make test-a11y` (#3608)
# into the extra arguments for `npx playwright test`, one per line on stdout.
#
# Inputs arrive by ENVIRONMENT (never as text spliced into a shell line):
#   SPEC     space- or comma-separated repo-relative spec globs,
#            e.g. 'tests/a11y/extrafanart-*.spec.js tests/a11y/cheat-sheet.spec.js'
#   PROJECT  firefox | chromium | firefox-a11y | chromium-a11y
# Both unset/empty prints nothing: the default full run is byte-for-byte today's.
#
# Two traps this exists to avoid:
#   - Playwright treats a positional arg as a REGEX matched as a SUBSTRING of
#     the path, so a bare `contrast.spec.js` also selects muted-text-contrast.
#     Each expanded file is emitted as an anchored, escaped regex instead.
#   - SPEC is operator input. Tokens are checked against a strict charset
#     BEFORE any glob expansion, so `; rm -rf`, `$(..)`, `..` or an absolute
#     path is refused (exit 2) rather than interpreted.
# A token that matches no file exits 2: an empty scope must never fall through
# to "no filter" (a full run) or to a silent green.
set -euo pipefail
set -f # no accidental globbing while tokenizing

SPEC="${SPEC:-}"
PROJECT="${PROJECT:-}"
ROOT="$(cd "$(dirname "$0")/.." && pwd)"

if [ -n "$PROJECT" ]; then
  case "$PROJECT" in
    firefox | firefox-a11y) echo "--project=firefox-a11y" ;;
    chromium | chromium-a11y) echo "--project=chromium-a11y" ;;
    *)
      echo "a11y-scope: PROJECT='$PROJECT' is not one of firefox, chromium, firefox-a11y, chromium-a11y" >&2
      exit 2
      ;;
  esac
fi

[ -n "${SPEC//[[:space:],]/}" ] || exit 0

seen=" "
for tok in $(printf '%s' "$SPEC" | tr ',' ' '); do
  if ! [[ "$tok" =~ ^tests/a11y/[A-Za-z0-9._*?/-]+\.spec\.js$ ]] || [[ "$tok" == *..* ]]; then
    echo "a11y-scope: bad SPEC token '$tok' (want repo-relative tests/a11y/<glob>.spec.js)" >&2
    exit 2
  fi
  matched=0
  set +f
  for f in "$ROOT"/$tok; do
    [ -f "$f" ] || continue
    matched=1
    rel="${f#"$ROOT"/}"
    case "$seen" in *" $rel "*) continue ;; esac
    seen="$seen$rel "
    # Only `.` is a regex metacharacter the charset admits.
    printf '/%s$\n' "${rel//./\\.}"
  done
  set -f
  if [ "$matched" -eq 0 ]; then
    echo "a11y-scope: SPEC token '$tok' matches no spec file" >&2
    exit 2
  fi
done
