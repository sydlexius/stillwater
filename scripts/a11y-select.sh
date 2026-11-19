#!/usr/bin/env bash
# a11y-select.sh -- classify a diff for the pre-push gate's RUN_A11Y=changed
# mode (#3608): does it need the a11y suite at all?
#
#   a11y-select.sh <BASE>     classify `git diff BASE` (BASE must resolve)
#   a11y-select.sh --check    verify tests/a11y/spec-map.toml against the tree
# Test hooks: --files-from <file> (lines "<status><TAB><path>") replaces the
# git diff; --map <file> replaces the map.
#
# stdout (classify): line 1 `MODE=FULL|EMPTY`, then one `*<TAB><changed file:
# why>` line per file that forces FULL. Exit 0 = a verdict; 2 = bad input or
# map (the gate treats that as a BLOCK, never a skip).
#
# There is no partial selection. A changed file contributes nothing only when it
# matches the map's [allow] list (explicitly non-UI: docs, scripts, CI config,
# Go tests) and no [shared] glob. Every other file, including anything under
# tests/a11y/, forces the FULL suite; [shared] wins over [allow]. EMPTY therefore means "every changed file
# is allow-listed", never "unknown". This script does not check that BASE is
# the merge base with main; the gate's `changed)` arm does that.
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
cd "$ROOT"
MAP="tests/a11y/spec-map.toml"
MODE=select; FILES_FROM=""; BASE=""
while [ $# -gt 0 ]; do
  case "$1" in
    --check) MODE=check ;;
    --files-from) FILES_FROM="${2:?--files-from needs a file}"; shift ;;
    --map) MAP="${2:?--map needs a file}"; shift ;;
    -*) echo "a11y-select: unknown option '$1'" >&2; exit 2 ;;
    *) [ -z "$BASE" ] || { echo "a11y-select: more than one BASE" >&2; exit 2; }; BASE="$1" ;;
  esac
  shift
done

TMP="$(mktemp)"; trap 'rm -f "$TMP"' EXIT
if [ "$MODE" = select ]; then
  if [ -n "$FILES_FROM" ]; then
    cp "$FILES_FROM" "$TMP"
  else
    [ -n "$BASE" ] || { echo "a11y-select: BASE required" >&2; exit 2; }
    git rev-parse --verify -q "$BASE^{commit}" >/dev/null || { echo "a11y-select: cannot resolve BASE '$BASE'" >&2; exit 2; }
    . "$ROOT/scripts/lib/git-plain.sh"
    # Added, modified AND deleted files: a deleted file is a change too. Classify
    # the union of the committed range (BASE..HEAD, what gets pushed) and the
    # working tree: a committed UI change reverted but not yet committed back
    # would otherwise vanish from a working-tree-only diff.
    { git_plain_diff --name-status --no-renames "$BASE" HEAD
      git_plain_diff --name-status --no-renames "$BASE"; } | sort -u >"$TMP"
  fi
else
  git ls-files >"$TMP"
fi

python3 -I - "$MODE" "$MAP" "$TMP" <<'PY'
import re, sys
mode, mapfile, listfile = sys.argv[1:4]

def die(msg):
    print("a11y-select: " + msg, file=sys.stderr); sys.exit(2)

try:
    import tomllib
except ImportError:
    die("python3 >= 3.11 (tomllib) required")

def rx(glob):  # ** crosses '/', * and ? do not
    out, i = "", 0
    while i < len(glob):
        if glob.startswith("**/", i): out += "(?:.*/)?"; i += 3
        elif glob.startswith("**", i): out += ".*"; i += 2
        elif glob[i] == "*": out += "[^/]*"; i += 1
        elif glob[i] == "?": out += "[^/]"; i += 1
        else: out += re.escape(glob[i]); i += 1
    return re.compile("^" + out + "$")

PATH_OK = re.compile(r"^[A-Za-z0-9._*?/-]+$")
try:
    m = tomllib.load(open(mapfile, "rb"))
except (OSError, tomllib.TOMLDecodeError) as e:
    die("cannot read map %s: %s" % (mapfile, e))
# Only [shared] and [allow] exist. Any other table (a per-page [[rule]], say)
# would be silently ignored, so refuse it rather than let it look honored.
extra = sorted(set(m) - {"shared", "allow"})
if extra: die("unknown map section(s) %s: only [shared] and [allow] are supported" % ", ".join(extra))
lists = {}
for sec in ("shared", "allow"):
    t = m.get(sec, {})
    if not isinstance(t, dict) or set(t) - {"paths"}: die("[%s] takes only a `paths` list" % sec)
    paths = t.get("paths", [])
    if not isinstance(paths, list): die("[%s] paths must be a list" % sec)
    for g in paths:
        if not isinstance(g, str) or not PATH_OK.match(g): die("bad %s glob %r" % (sec, g))
    lists[sec] = [(g, rx(g)) for g in paths]
shared_rx, allow_rx = lists["shared"], lists["allow"]

lines = [l.rstrip("\n") for l in open(listfile)]

if mode == "check":
    tracked = [l for l in lines if l]
    bad = ["stale %s glob %r matches no file" % (sec, g)
           for sec in ("shared", "allow") for g, r in lists[sec] if not any(r.match(t) for t in tracked)]
    if bad:
        print("FAIL: tests/a11y/spec-map.toml is out of step with the tree:")
        for b in sorted(set(bad)): print("  " + b)
        sys.exit(1)
    print("OK: spec map globs all match tracked files (%d shared, %d allow)" % (len(shared_rx), len(allow_rx)))
    sys.exit(0)

full = []
for l in lines:
    if not l: continue
    parts = l.split("\t")
    if len(parts) != 2 or parts[0] not in ("A", "M", "D", "T"): die("bad diff line %r" % l)
    p = parts[1]
    if p.startswith("/") or ".." in p.split("/") or "\\" in p or not re.match(r"^[^\x00-\x1f]+$", p): die("refusing path %r" % p)
    sh = [g for g, r in shared_rx if r.match(p)]
    if sh:
        full.append("%s: shared surface (%s)" % (p, sh[0])); continue
    if any(r.match(p) for _, r in allow_rx): continue  # explicitly non-UI: contributes nothing
    full.append("%s: not on the non-UI allow-list" % p)
if full:
    print("MODE=FULL")
    for w in sorted(set(full)): print("*\t" + w)
else:
    print("MODE=EMPTY")
PY
