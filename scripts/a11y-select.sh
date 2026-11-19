#!/usr/bin/env bash
# a11y-select.sh -- classify a diff for the pre-push gate's RUN_A11Y=changed
# mode (#3608): does it need the a11y suite at all?
#
#   a11y-select.sh <BASE>     classify `git diff BASE` (BASE must resolve)
#   a11y-select.sh --check    verify tests/a11y/spec-map.toml against the tree
# Test hooks: --files-from <file> (lines "<status><TAB><path>") replaces the
# git diff; --map <file> replaces the map.
#
# stdout (classify): line 1 `MODE=FULL|SELECT|EMPTY`; then, for FULL, one
# `*<TAB><changed file: why>` line per file that forces it, or, for SELECT, one
# `<spec path><TAB><why>` line per selected spec. Exit 0 = a verdict; 2 = bad
# input or map (the gate treats that as a BLOCK, never a skip).
#
# Each changed file is classified in this order (a generated x_templ.go counts
# as its x.templ):
#   1. tests/a11y/<name>.spec.js or tests/a11y/helpers/seed-*.js: selects every
#      spec that is, or imports it (directly or through other specs and seeds);
#      FULL when any other .js under tests/a11y (global-setup.js, bootstrap.js,
#      api.js: the shared harness) imports it, when no spec does, or when it is
#      a deleted spec.
#   2. a [shared] glob: FULL. (The rest of tests/a11y/ is [shared].)
#   3. one or more [[rule]] path globs: selects the union of those rules' specs.
#   4. an [allow] glob (explicitly non-UI: docs, scripts, CI config, Go tests):
#      contributes nothing.
#   5. anything else: FULL.
# Any FULL file makes the verdict FULL; otherwise SELECT if any spec was
# selected, else EMPTY. EMPTY therefore means "every changed file is
# allow-listed", never "unknown". This script does not check that BASE is the
# merge base with main; the gate's `changed)` arm does that.
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
    # the union of every place a change can sit: the committed range (BASE..HEAD,
    # what gets pushed), the index, the working tree, and untracked files that
    # are not ignored (git diff never lists those). Any one of them alone can
    # hide a UI change the others show, e.g. a committed change reverted in the
    # working tree, or a staged change reverted only in the working tree.
    # Untracked paths go through the same plain environment as git_plain_diff;
    # a path git must quote (control characters) starts with `"` and contains
    # `\`, which the classifier refuses (exit 2).
    { git_plain_diff --name-status --no-renames "$BASE" HEAD
      git_plain_diff --name-status --no-renames --cached "$BASE"
      git_plain_diff --name-status --no-renames "$BASE"
      GIT_LITERAL_PATHSPECS=0 GIT_GLOB_PATHSPECS=0 GIT_NOGLOB_PATHSPECS=0 GIT_ICASE_PATHSPECS=0 \
        command git -c core.quotePath=false ls-files --others --exclude-standard | awk '{ print "A\t" $0 }'
    } | sort -u >"$TMP"
  fi
else
  git ls-files >"$TMP"
fi

python3 -I - "$MODE" "$MAP" "$TMP" <<'PY'
import os, re, sys
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
# Only [shared], [allow] and [[rule]] exist. Any other table would be silently
# ignored, so refuse it rather than let it look honored.
extra = sorted(set(m) - {"shared", "allow", "rule"})
if extra: die("unknown map section(s) %s: only [shared], [allow] and [[rule]] are supported" % ", ".join(extra))
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
SPEC_GLOB = re.compile(r"^[A-Za-z0-9._*?-]+\.spec\.js$")
# The charset scripts/a11y-scope.sh accepts for a SPEC token: a selected path
# outside it would be refused later by `make test-a11y SPEC=`, so refuse it here.
SPEC_PATH = re.compile(r"^tests/a11y/[A-Za-z0-9._-]+\.spec\.js$")
rules = m.get("rule", [])
if not isinstance(rules, list): die("[[rule]] must be an array of tables")
for r in rules:
    if not isinstance(r, dict) or set(r) != {"paths", "specs"}: die("each [[rule]] takes exactly `paths` and `specs`: %r" % (r,))
    for k, ok in (("paths", PATH_OK), ("specs", SPEC_GLOB)):
        if not isinstance(r[k], list) or not r[k]: die("[[rule]] %s must be a non-empty list: %r" % (k, r))
        for g in r[k]:
            if not isinstance(g, str) or not ok.match(g): die("bad rule %s glob %r" % (k, g))
rule_rx = [([(g, rx(g)) for g in r["paths"]], r["specs"]) for r in rules]
specs_on_disk = sorted(f for f in os.listdir("tests/a11y") if f.endswith(".spec.js")) if os.path.isdir("tests/a11y") else []
def expand(globs):
    hit = sorted({"tests/a11y/" + s for g in globs for s in specs_on_disk if rx(g).match(s)})
    for s in hit:
        if not SPEC_PATH.match(s): die("spec %r is outside the make test-a11y SPEC charset" % s)
    return hit
HARNESS_JS = sorted(os.path.join(d, f)
                    for d, _, fs in os.walk("tests/a11y") if "node_modules" not in d for f in fs if f.endswith(".js"))
SEED = re.compile(r"^tests/a11y/helpers/seed-[A-Za-z0-9._-]+\.js$")
def importers(p):
    """(specs that are or import p, global .js users), through specs and seeds, transitively."""
    seen, todo, glob_users = {p}, [p], []
    while todo:
        b = os.path.basename(todo.pop())
        pat = re.compile(r"[/'\"]%s(?:\.js)?['\"]" % re.escape(b[:-3]))
        for f in HARNESS_JS:
            if f in seen: continue
            try: src = open(f, encoding="utf-8", errors="replace").read()
            except OSError: continue
            if not pat.search(src): continue
            seen.add(f)
            if f.endswith(".spec.js") or SEED.match(f): todo.append(f)
            else: glob_users.append(f)
    return sorted(s for s in seen if re.match(r"^tests/a11y/[^/]+\.spec\.js$", s) and os.path.exists(s)), sorted(glob_users)

lines = [l.rstrip("\n") for l in open(listfile)]

if mode == "check":
    tracked = [l for l in lines if l]
    bad = ["stale %s glob %r matches no file" % (sec, g)
           for sec in ("shared", "allow") for g, r in lists[sec] if not any(r.match(t) for t in tracked)]
    bad += ["stale rule glob %r matches no file" % g for gl, _ in rule_rx for g, r in gl if not any(r.match(t) for t in tracked)]
    ruled = set()
    for _, sg in rule_rx:
        for g in sg:
            hit = expand([g])
            if not hit: bad.append("stale rule spec glob %r matches no spec" % g)
            ruled.update(hit)
    # A spec in no rule must be named on a `# unruled: <spec>... -- <why>` line.
    unruled = {}
    for l in open(mapfile, encoding="utf-8"):
        mm = re.match(r"^# unruled: (.+?) -- \S", l)
        if mm:
            for s in mm.group(1).split(): unruled["tests/a11y/" + s] = 1
    for s in ("tests/a11y/" + x for x in specs_on_disk):
        if s not in ruled and s not in unruled: bad.append("spec %s is in no rule and not on an `# unruled:` line" % s)
    for s in unruled:
        if s in ruled or s[len("tests/a11y/"):] not in specs_on_disk: bad.append("stale `# unruled:` entry %s (in a rule, or no such spec)" % s)
    if bad:
        print("FAIL: tests/a11y/spec-map.toml is out of step with the tree:")
        for b in sorted(set(bad)): print("  " + b)
        sys.exit(1)
    ui = [t for t in tracked if re.match(r"^(web/templates/.*\.templ|internal/api/handlers_[^/]*\.go)$", t) and not t.endswith("_test.go")]
    unmapped = [t for t in ui if not any(r.match(t) for _, r in shared_rx) and not any(r.match(t) for gl, _ in rule_rx for _, r in gl)]
    print("OK: spec map globs all match tracked files (%d shared, %d allow, %d rules; %d specs in a rule, %d unruled)"
          % (len(shared_rx), len(allow_rx), len(rule_rx), len(ruled), len(unruled)))
    print("  %d of %d UI files are in no rule, so a change to one runs the FULL suite: %s" % (len(unmapped), len(ui), " ".join(unmapped)))
    sys.exit(0)

full, picked = [], {}
def pick(specs, why):
    for s in specs: picked.setdefault(s, why)
for l in lines:
    if not l: continue
    parts = l.split("\t")
    if len(parts) != 2 or parts[0] not in ("A", "M", "D", "T"): die("bad diff line %r" % l)
    p = parts[1]
    if p.startswith("/") or ".." in p.split("/") or "\\" in p or not re.match(r"^[^\x00-\x1f]+$", p): die("refusing path %r" % p)
    if p.startswith("web/") and p.endswith("_templ.go"): p = p[:-len("_templ.go")] + ".templ"  # generated from x.templ
    is_spec = bool(re.match(r"^tests/a11y/[^/]+\.spec\.js$", p))
    if is_spec or SEED.match(p):
        if is_spec and not SPEC_PATH.match(p): die("changed spec %r is outside the make test-a11y SPEC charset" % p)
        if is_spec and not os.path.exists(p):
            full.append("%s: deleted spec" % p); continue
        sel, users = importers(p)
        if users: full.append("%s: imported by the shared harness (%s)" % (p, ", ".join(users)))
        elif not sel: full.append("%s: imported by no spec" % p)
        else: pick(sel, "%s: %s" % (p, "changed" if is_spec else "seed imported"))
        continue
    sh = [g for g, r in shared_rx if r.match(p)]
    if sh:
        full.append("%s: shared surface (%s)" % (p, sh[0])); continue
    hit = [sg for gl, sg in rule_rx if any(r.match(p) for _, r in gl)]
    if hit:
        sel = expand([g for sg in hit for g in sg])
        # A rule hit that selects nothing is a map fault, never a reason to skip.
        if not sel: full.append("%s: its rule selects no existing spec" % p)
        else: pick(sel, "%s: rule" % p)
        continue
    if any(r.match(p) for _, r in allow_rx): continue  # explicitly non-UI: contributes nothing
    full.append("%s: not on the non-UI allow-list or in a rule" % p)
if full:
    print("MODE=FULL")
    for w in sorted(set(full)): print("*\t" + w)
elif picked:
    print("MODE=SELECT")
    for s in sorted(picked): print("%s\t%s" % (s, picked[s]))
else:
    print("MODE=EMPTY")
PY
