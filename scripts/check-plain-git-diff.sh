#!/bin/bash
# check-plain-git-diff.sh -- fail when the pre-push gate, a helper, or a git hook
# runs a patch-producing git command without the plain-output helper (#3446). A
# developer's GIT_EXTERNAL_DIFF, textconv, color, prefix, binary-attribute or
# pathspec config changes the text a parser reads, so the check passes on
# nothing; the measured effect of each is in scripts/lib/git-plain.sh. Shell
# goes through git_plain_diff, Python through GIT_PLAIN_DIFF (+ GIT_PLAIN_ENV).
#
# SCANNED: every non-test file under scripts/ (including scripts/lib/) and
# .githooks/, except this file and lib/git-plain.sh. Shell verbs matched: diff,
# diff-tree, diff-index, diff-files, log, show, format-patch, whatchanged,
# range-diff, after any `-`-prefixed global options. `--no-pager`, `-C x`,
# `-c k=v` and the verb ending at |, >, ; or a backtick all count.
# ALLOWED: `git show <rev>:<path>` (a blob; <rev>:<path> must be the first
# non-option argument), a comment line, and a line ending in
# `# plain-git-exempt: <reason>` (reason required). In .githooks/ a line with
# --name-only is also allowed: those only choose staged files to lint, they
# decide no verdict from patch text.
# Python is parsed with ast: any ["git", ... "diff"|"log"|"show"|...] list not
# assigned to GIT_PLAIN_DIFF is flagged, and GIT_PLAIN_DIFF must carry the
# hardening flags.
# KNOWN LIMITS (not chased): a git held in a variable or wrapper function,
# `git $VERB`, `git stash show -p`, `git blame/grep/status` output parsed,
# Makefile and workflow `run:` blocks, and false positives on prose inside a
# string that mentions `git diff` or on `git log --format=%H` (use the exempt
# marker). It also checks only that git_plain_diff is used, not that git's exit
# status is checked; the tests drive that per site.
# Usage: bash scripts/check-plain-git-diff.sh [root]
set -euo pipefail
ROOT="${1:-$(cd "$(dirname "$0")/.." && pwd)}"
VERBS='diff|diff-tree|diff-index|diff-files|log|show|format-patch|whatchanged|range-diff'
OPTS='(-[^[:space:]]+([[:space:]]+("[^"]*"|[^-"[:space:]][^[:space:]]*))?[[:space:]]+)*'
GITRE="(^|[^[:alnum:]_./-])git[[:space:]]+${OPTS}(${VERBS})([^[:alnum:]_-]|\$)"
BLOBRE="show([[:space:]]+-[^[:space:]]+)*[[:space:]]+[^-[:space:]][^[:space:]]*:"
EXEMPTRE='#[[:space:]]*plain-git-exempt:[[:space:]]*[^[:space:]]'
bad=""
while IFS= read -r f; do
  rel=${f#"$ROOT"/}
  case "$rel" in
    scripts/check-plain-git-diff.sh|scripts/lib/git-plain.sh) continue ;;
    *.py)
      out=$(python3 - "$f" <<'PYEOF'
import ast, sys
VERBS = {"diff", "diff-tree", "diff-index", "diff-files", "log", "show",
         "format-patch", "whatchanged", "range-diff"}
NEED = {"--no-ext-diff", "--no-textconv", "--no-color", "--text",
        "--inter-hunk-context=0", "-M"}
src = open(sys.argv[1]).read(); lines = src.splitlines()
tree = ast.parse(src)
plain = {id(n.value) for n in ast.walk(tree) if isinstance(n, ast.Assign)
         and any(getattr(t, "id", "") == "GIT_PLAIN_DIFF" for t in n.targets)}
for n in ast.walk(tree):
    if isinstance(n, ast.Assign) and id(n.value) in plain:
        have = {e.value for e in n.value.elts if isinstance(e, ast.Constant)}
        if NEED - have:
            print(f"{n.lineno}: GIT_PLAIN_DIFF lacks {sorted(NEED - have)}")
    if not isinstance(n, (ast.List, ast.Tuple)) or id(n) in plain or not n.elts:
        continue
    e = n.elts
    if not (isinstance(e[0], ast.Constant) and e[0].value == "git"):
        continue
    vs = [i for i, x in enumerate(e) if isinstance(x, ast.Constant) and x.value in VERBS]
    if not vs:
        continue
    nxt = e[vs[0] + 1] if vs[0] + 1 < len(e) else None
    if e[vs[0]].value == "show" and nxt is not None and (isinstance(nxt, ast.JoinedStr)
            or (isinstance(nxt, ast.Constant) and ":" in str(nxt.value))):
        continue
    if "plain-git-exempt:" in lines[n.lineno - 1].split("#", 1)[-1]:
        continue
    print(f"{n.lineno}: {lines[n.lineno - 1].strip()}")
PYEOF
) || { bad="$bad$rel: python parse failed"$'\n'; continue; }
      [ -n "$out" ] && bad="$bad$(printf '%s\n' "$out" | sed "s|^|$rel:|")"$'\n'
      ;;
    *)
      out=$(grep -InE "$GITRE" "$f" 2>/dev/null \
        | grep -vE '^[0-9]+:[[:space:]]*#' \
        | grep -vE "$BLOBRE" \
        | grep -vE "$EXEMPTRE" || true)
      case "$rel" in .githooks/*) out=$(printf '%s\n' "$out" | grep -v -e '--name-only' || true) ;; esac
      [ -n "$out" ] && bad="$bad$(printf '%s\n' "$out" | sed "/^$/d; s|^|$rel:|")"$'\n'
      ;;
  esac
done < <(find "$ROOT/scripts" "$ROOT/.githooks" -type f ! -name 'test-*' 2>/dev/null | sort)
bad=$(printf '%s' "$bad" | sed '/^$/d')
if [ -n "$bad" ]; then
  echo "FAIL: git diff output parsed without the plain-output helper (#3446):"
  printf '%s\n' "$bad" | sed 's/^/  /'
  echo "Use git_plain_diff (scripts/lib/git-plain.sh) or GIT_PLAIN_DIFF in Python."
  exit 1
fi
echo "OK: every patch-producing git call under scripts/ and .githooks/ goes through the plain-output helper"
