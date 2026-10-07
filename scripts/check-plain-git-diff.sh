#!/bin/bash
# check-plain-git-diff.sh -- fail when the pre-push gate or a helper runs a
# `git diff` / `git log` / `git show <patch>` without the plain-output helper
# (#3446). A developer's GIT_EXTERNAL_DIFF, textconv, color or prefix config
# changes the text a parser reads, so the check passes on nothing; the measured
# effect of each is in scripts/lib/git-plain.sh. Shell goes through
# git_plain_diff; scripts/prefs-coverage.py goes through GIT_PLAIN_DIFF.
# Allowed without the helper: `git show <rev>:<path>` (a blob, probed
# unaffected), a line carrying all of --no-ext-diff --no-textconv --no-color,
# and a line carrying `# plain-git-exempt: <reason>`.
# Scope: scripts/*.sh and scripts/*.py except test-* (.githooks/pre-commit's one
# content-parsing call carries the flags inline; its others list names). Usage: bash scripts/check-plain-git-diff.sh [root]
set -euo pipefail
ROOT="${1:-$(cd "$(dirname "$0")/.." && pwd)}"
bad=$(
  for f in "$ROOT"/scripts/*.sh; do
    case "$(basename "$f")" in test-*|check-plain-git-diff.sh) continue ;; esac
    grep -nE '(^|[^[:alnum:]_-])git( +-[cC] +[^ ]+)* +(diff|log|show|format-patch)( |$|\))' "$f" \
      | grep -vE '^[0-9]+:[[:space:]]*#' \
      | grep -vE 'git( +-[cC] +[^ ]+)* +show +[^ ]+:' \
      | grep -vE -e '--no-ext-diff.*--no-textconv.*--no-color|plain-git-exempt:' \
      | sed "s|^|$f:|" || true
  done
  for f in "$ROOT"/scripts/*.py; do
    case "$(basename "$f")" in test-*) continue ;; esac
    grep -nE '"(diff|log|format-patch)"|"show"' "$f" \
      | grep -vE '^[0-9]+:[[:space:]]*#|GIT_PLAIN_DIFF = |"show", f"[^"]*:|plain-git-exempt:' \
      | sed "s|^|$f:|" || true
  done
)
if [ -n "$bad" ]; then
  echo "FAIL: git diff output parsed without the plain-output helper (#3446):"
  printf '%s\n' "$bad" | sed 's/^/  /'
  echo "Use git_plain_diff (scripts/lib/git-plain.sh) or GIT_PLAIN_DIFF in Python."
  exit 1
fi
echo "OK: every parsed git diff in scripts/ goes through the plain-output helper"
