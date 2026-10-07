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
# ALLOWED: `git show <rev>:<path>` (a blob; must be the first non-option
# argument, but the test is line-wide), a comment line, and a trailing comment
# `# plain-git-exempt: <reason>` (reason required, no quote after it, so a string
# cannot carry it; the same rule in Python). `--name-only` is NOT special: hook
# name listings go through git_plain_diff too.
# Python is parsed with ast (python3 required: the guard FAILS without it, it is
# a required check and never skips; a file the interpreter cannot parse FAILS
# and names the interpreter version). Any ["git", ... "diff"|"log"|"show"|...]
# list not assigned to GIT_PLAIN_DIFF is flagged; GIT_PLAIN_DIFF must keep the
# hardening flags. It fails when it scanned nothing.
# KNOWN LIMITS (not chased): a git held in a variable or wrapper function,
# `git $VERB`, absolute-path git, quoted "git"/"diff", backslash continuations,
# a blob-show elsewhere on the same line, composed Python argv, shell-string
# commands in Python, extension-less Python, `git stash show -p`, `git
# blame/grep/status` output parsed, Makefile and workflow `run:` blocks, and
# false positives on prose in a string that mentions `git diff` or on `git log
# --format=%H` (use the exempt marker). The hook's marker scan skips `-diff` /
# `binary` attributed files: the intended cost of reading binaries as binary. It checks that git_plain_diff is used,
# not that git's exit status is checked; the tests drive that per site.
# Usage: bash scripts/check-plain-git-diff.sh [root]
set -euo pipefail
ROOT="${1:-$(cd "$(dirname "$0")/.." && pwd)}"
VERBS='diff|diff-tree|diff-index|diff-files|log|show|format-patch|whatchanged|range-diff'
OPTS='(-[^[:space:]]+([[:space:]]+("[^"]*"|[^-"[:space:]][^[:space:]]*))?[[:space:]]+)*'
GITRE="(^|[^[:alnum:]_./-])git[[:space:]]+${OPTS}(${VERBS})([^[:alnum:]_-]|\$)"
BLOBRE="show([[:space:]]+-[^[:space:]]+)*[[:space:]]+[^-[:space:]][^[:space:]]*:"
EXEMPTRE="[[:space:]]#[[:space:]]*plain-git-exempt:[[:space:]]*[^\"'[:space:]][^\"']*\$"
command -v python3 >/dev/null 2>&1 \
  || { echo "FAIL: check-plain-git-diff needs python3 (not found in PATH); this is a required check, so it fails rather than skips" >&2; exit 2; }
PYV=$(python3 -c 'import sys; print("%d.%d" % sys.version_info[:2])')
# The Python scanner lives in a temp file written here, NOT in a heredoc inside
# $( ): bash 3.2 (stock macOS) cannot parse quote characters in such a heredoc.
PYSCAN=$(mktemp)
trap 'rm -f "$PYSCAN"' EXIT
cat > "$PYSCAN" <<'PYEOF'
import ast, re, sys
VERBS = {"diff", "diff-tree", "diff-index", "diff-files", "log", "show",
         "format-patch", "whatchanged", "range-diff"}
NEED = {"--no-ext-diff", "--no-textconv", "--no-color", "--text",
        "--inter-hunk-context=0", "-M"}
src = open(sys.argv[1]).read(); lines = src.splitlines()
try:
    tree = ast.parse(src)
except SyntaxError as e:
    print(f"{e.lineno}: python {sys.version_info[0]}.{sys.version_info[1]} cannot parse this file: {e.msg}")
    sys.exit(1)
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
    if re.search(r"""\s#\s*plain-git-exempt:\s*[^"'\s][^"']*$""", lines[n.lineno - 1]):
        continue
    print(f"{n.lineno}: {lines[n.lineno - 1].strip()}")
PYEOF
scanned=0
bad=""
while IFS= read -r f; do
  rel=${f#"$ROOT"/}
  case "$rel" in
    scripts/check-plain-git-diff.sh|scripts/lib/git-plain.sh) continue ;;
    *.py)
      scanned=$((scanned + 1)); pyrc=0
      out=$(python3 "$PYSCAN" "$f") || pyrc=1
      [ "$pyrc" -eq 0 ] || [ -n "$out" ] || out="python $PYV failed on this file"
      [ -z "$out" ] || bad="$bad$(printf '%s\n' "$out" | sed "s|^|$rel:|")"$'\n'
      ;;
    *)
      scanned=$((scanned + 1))
      out=$(grep -InE "$GITRE" "$f" 2>/dev/null \
        | grep -vE '^[0-9]+:[[:space:]]*#' \
        | grep -vE "$BLOBRE" \
        | grep -vE "$EXEMPTRE" || true)
      [ -n "$out" ] && bad="$bad$(printf '%s\n' "$out" | sed "/^$/d; s|^|$rel:|")"$'\n'
      ;;
  esac
done < <(find "$ROOT/scripts" "$ROOT/.githooks" -type f ! -name 'test-*' 2>/dev/null | sort)
[ "$scanned" -gt 0 ] || { echo "FAIL: check-plain-git-diff scanned 0 files under $ROOT (bad root or find failed)" >&2; exit 2; }
bad=$(printf '%s' "$bad" | sed '/^$/d')
if [ -n "$bad" ]; then
  echo "FAIL: git diff output parsed without the plain-output helper (#3446):"
  printf '%s\n' "$bad" | sed 's/^/  /'
  echo "Use git_plain_diff (scripts/lib/git-plain.sh) or GIT_PLAIN_DIFF in Python."
  exit 1
fi
echo "OK: every patch-producing git call under scripts/ and .githooks/ goes through the plain-output helper"
