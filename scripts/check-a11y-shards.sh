#!/bin/bash
# check-a11y-shards.sh -- assert the a11y-test matrix in ci.yml runs every
# Playwright test exactly once per project (#3442).
#
# A matrix entry selects its slice of a project's suite in ONE of two ways:
#   - `--shard=<shard>/<shards>` (Playwright's count-based split), when the
#     entry has no `specs` key;
#   - an explicit, time-balanced list of spec files (the entry's `specs` key:
#     ONE line of ASCII-space-separated paths relative to the repo root), passed
#     to `playwright test` as positional file filters.
# scripts/a11y-shard-exec.sh is the ONLY tokenizer: CI's run step runs
# Playwright through it, and this checker reads every entry's tokens (layer 1)
# and runs every per-entry `--list` (layer 2) through it.
# The run step itself is pinned: its `run` must equal RUN (below) exactly, its
# env must equal STEP_ENV exactly, it may carry no key but name/env/run (no
# `if`, `continue-on-error`, `shell`, `working-directory`, `timeout-minutes`,
# `id`, ...), the job may set no `continue-on-error`, `defaults`, `env`,
# `container` or `services`, the workflow no `defaults`, the matrix nothing but
# `include`, and no other step's `run` may name Playwright (any case) beyond
# installing browsers, or GITHUB_ENV, GITHUB_PATH or BASH_ENV. RUN must be
# plain words (no quoting or shell operators but the one `--project="$PROJECT"`
# word), so the argv bash runs is the argv layer 2 runs: RUN's own words per
# entry, with only `npx playwright`, the exec-script path, the config path and
# $PROJECT mapped to this checkout and `--list --reporter=json` appended.
# Blind spots (it claims no more): it cannot see other steps' side effects
# beyond those rejected patterns (a step that deletes or edits a spec or the
# config, or reaches the env files through indirection, passes; the patterns
# are textual), composite and other `uses:` actions, runner images,
# workflow-level env, or spec-level skips (`test.skip`/`fixme` inside a spec).
# A project uses one mechanism for all of its entries, never a mix.
#
# Step order of the background binary build (scripts/a11y-bg-build.sh). The
# build embeds web/static/ and compiles the templ output, both COMMITTED, so a
# build started too early passes on stale CSS with no failure anywhere. By step
# name (setup-go and setup-node by their `uses` action), each exactly once:
# setup-go < "Restore the a11y binary's Go cache" < "Generate templ" < "Build
# Tailwind CSS" < "Start the binary build (background)" < setup-node; the start
# < "Wait for the binary build" < "Start Stillwater (ephemeral)"; the wait <
# "Save the a11y binary's Go cache". A missing or duplicated step fails. No step
# strictly between the start and the wait may mention web/static/, templ,
# tailwind, go generate, or a *.go / *.templ name in its `run` or `uses` (a
# TEXTUAL check: it cannot see a write reached through indirection).
#
# Layer 1, structural (python3 + PyYAML only, always runs). Per project: every
# entry carries `shard` and `shards` as positive integers, `shards` constant,
# the entry count equal to it, and the `shard` values exactly 1..N; the
# expected project set is the `*-a11y` names declared in playwright.config.js
# (layer 2 re-checks it against the projects Playwright itself loads).
# For a `specs` project, additionally: every listed path is an existing spec
# file under the config's testDir, no path is listed twice, and the lists
# together name EVERY spec file under testDir. A new spec that no list names
# fails here, loudly -- a hand-maintained list must never drop a spec silently.
#
# Layer 2, `playwright test --list` (needs node_modules; see below). Per
# project, the multiset union of the tests each entry selects (run with that
# entry's REAL arguments) must equal the project's full `--list` exactly: no
# test in two entries, no test in none. For a `specs` entry, the files its
# filters select must also equal the files it lists. This is the proof that
# matters: Playwright's positional filters are case-insensitive regex SUBSTRING
# matches against the absolute path, so a bare `contrast.spec.js` also selects
# filter-flyout-, muted-text- and reports-contrast.spec.js, and a filter that
# matches nothing is silently ignored when another filter matches. Layer 1
# cannot see either; layer 2 does.
#
# Layer 2 is REQUIRED by default: a missing node_modules/.bin/playwright is a
# FAIL ("run npm ci"), never a silent pass. A11Y_SHARDS_LIST=off runs layer 1
# only; the caller that sets it owns reporting that (scripts/pre-push-gate.sh
# does so only when no partition input changed, via gate_skip deterministic).
# CI's always-running, required "Gate Invariant" job (gate.yml) runs `npm ci`
# first, so it always runs both layers.
# Every layer reports all of its findings before the run exits. Malformed input
# fails with the FAIL line, never a traceback.
#
# Usage: bash scripts/check-a11y-shards.sh [ci.yml [playwright.config.js]]
# The config's directory is the root `specs` paths are relative to, and the
# directory Playwright runs from. PLAYWRIGHT_BIN overrides the binary.
set -euo pipefail
ROOT="$(cd "$(dirname "$0")/.." && pwd)"
CI="${1:-$ROOT/.github/workflows/ci.yml}"
CFG="${2:-$ROOT/playwright.config.js}"
case "${A11Y_SHARDS_LIST:-require}" in require|off) ;; *)
  echo "check-a11y-shards: A11Y_SHARDS_LIST must be 'require' or 'off', got '${A11Y_SHARDS_LIST}'" >&2; exit 2 ;; esac
python3 -c 'import yaml' 2>/dev/null || { echo "check-a11y-shards: python3 with PyYAML required" >&2; exit 2; }
CFG_DIR="$(cd "$(dirname "$CFG")" && pwd)"
PW="${PLAYWRIGHT_BIN:-$CFG_DIR/node_modules/.bin/playwright}"
python3 - "$CI" "$CFG" "$PW" "${A11Y_SHARDS_LIST:-require}" "$ROOT/scripts/a11y-shard-exec.sh" <<'PYEOF'
import collections, concurrent.futures, json, os, re, subprocess, sys, yaml
ci, cfg, pw, list_mode, exec_sh = sys.argv[1:6]
# Symlink-resolved, and handed to Playwright resolved too: under a symlinked
# directory (macOS /var -> /private/var) Playwright reports spec paths against
# a mix of resolved and unresolved roots, and the file sets stop comparing.
cfg_dir = os.path.realpath(os.path.dirname(os.path.abspath(cfg)))
# PyYAML resolves a duplicated mapping key last-wins; GitHub and actionlint
# reject it. Reject it here too, so a hidden first `run:` or `env:` can never
# be what the checker reads while something else is what CI would run.
class UniqueKeyLoader(yaml.SafeLoader):
    pass
def _unique_mapping(loader, node, deep=False):
    seen = set()
    for k, _ in node.value:
        if k.tag == "tag:yaml.org,2002:merge":
            continue
        key = loader.construct_object(k, deep=True)
        if key in seen:
            raise yaml.constructor.ConstructorError(None, None, f"duplicate key {key!r}", k.start_mark)
        seen.add(key)
    return loader.construct_mapping(node, deep)
UniqueKeyLoader.add_constructor(yaml.resolver.BaseResolver.DEFAULT_MAPPING_TAG, _unique_mapping)
def fail(*msgs):
    print("FAIL: a11y-test matrix shards incomplete:\n  " + "\n  ".join(msgs))
    sys.exit(1)
try:
    cfg_src = open(cfg).read()
    expected = set(re.findall(r"name:\s*'([\w-]+-a11y)'", cfg_src))
    wf = yaml.load(open(ci), Loader=UniqueKeyLoader)
    job = wf["jobs"]["a11y-test"]
    inc = job["strategy"]["matrix"]["include"]
    assert isinstance(inc, list) and all(isinstance(e, dict) for e in inc)
    steps = job.get("steps") or []
    assert isinstance(steps, list) and all(isinstance(x, dict) for x in steps)
    assert isinstance(job["strategy"]["matrix"], dict)
except Exception as e:
    fail(f"cannot read the matrix or project list ({type(e).__name__}: {e})")
if not expected:
    fail("no *-a11y projects found in " + cfg)

# ---- the one tokenizer ----------------------------------------------------------
def entry_env(e):
    """The env CI's run step gives the tokenizer for entry e (STEP_ENV's selection keys)."""
    return dict(os.environ, SPECS=e.get("specs") or "", SW_A11Y_SHARD=f"{e['shard']}/{e['shards']}", PROJECT=e["project"])
def via_exec(e, cmd):
    """Run cmd through a11y-shard-exec.sh with this entry's env, as CI's step does."""
    return subprocess.run(["bash", exec_sh, *cmd], env=entry_env(e), cwd=cfg_dir, capture_output=True, text=True)
def tokens(e):
    r = via_exec(e, ["printf", "%s\\0"])
    if r.returncode:
        return None, (r.stderr.strip() or f"exit {r.returncode}").replace("a11y-shard-exec: ", "")
    return r.stdout.split("\0")[:-1], None

# ---- the run step is pinned EXACTLY; layer 2 runs its own argv ----------------
# RUN is the a11y-test run step's `run` value, verbatim (one trailing newline
# stripped). Any other value fails: a suffix (`|| true`, `| tee`, `--shard=1/2`,
# `--grep`), a prefix line (`set +e`, `export SPECS=`), a hardcoded project or
# config. Layer 2 does not model this command: it splits RUN on single spaces
# and runs THOSE words, swapping only the CI-environment-bound words (run_argv
# below) for their local equivalents and appending `--list --reporter=json`.
# Change RUN and the listing changes with it. Every word must be plain
# (^[A-Za-z0-9_./=-]+$) or exactly `--project="$PROJECT"`, so bash's argv and
# this one are the same by construction: a quote, `$`, `|`, `||`, `;`, `&`,
# `$(...)` or doubled space anywhere else fails as "RUN no longer maps", never
# as a guess.
RUN = 'bash scripts/a11y-shard-exec.sh npx playwright test --config=playwright.config.js --project="$PROJECT"'
# The step's env, exactly. Every key is pinned (not just the three selection
# inputs): playwright.config.js and Playwright itself read the environment
# (PW_TEST_*, PLAYWRIGHT_*, SW_*), so an extra key could change what runs.
STEP_ENV = {
    "SW_PORT": "${{ steps.port.outputs.port }}",
    "SW_TEST_URL": "http://127.0.0.1:${{ steps.port.outputs.port }}",
    "SPECS": "${{ matrix.specs }}",
    "SW_A11Y_SHARD": "${{ matrix.shard }}/${{ matrix.shards }}",
    "PROJECT": "${{ matrix.project }}",
}
# The step's keys, exactly: no `if` (could skip it), no `continue-on-error`
# (a failure reads green), no `shell` (a custom shell can drop the exit status),
# no `working-directory` (another checkout's exec script). With no `shell`,
# GitHub runs RUN as `bash -e {0}`; RUN is one simple command, so its exit
# status IS the step's under bash, sh or pipefail alike.
STEP_KEYS = {"name", "env", "run"}
run_bad = []
def run_of(x):
    r = x.get("run")
    return r[:-1] if isinstance(r, str) and r.endswith("\n") else r
target = [x for x in steps if run_of(x) == RUN]
# Every OTHER step may name playwright only to install browsers or their deps.
# Case-insensitive (`${PW,,}` lowercases at run time); whole-line shell comments
# are dropped first, so prose about Playwright in a comment is not a hit.
allowed_pw = re.compile(r"npx playwright install(-deps)?\b|playwright-apt\b", re.I)
# No step may write the runner's env/path files or set BASH_ENV: a fake `npx` on
# GITHUB_PATH, or a BASH_ENV defining `npx(){ :; }`, makes RUN run nothing.
env_file = re.compile(r"GITHUB_ENV|GITHUB_PATH|BASH_ENV", re.I)
for x in steps:
    nm = x.get('name') or x.get('uses')
    raw = str(x.get("run", ""))
    if env_file.search(raw):
        run_bad.append(f"a11y-test: step {nm!r} mentions GITHUB_ENV/GITHUB_PATH/BASH_ENV; no step in this job "
                       f"may change the environment later steps (the run step) start with")
    if any(x is t for t in target):
        continue
    code = "\n".join(ln for ln in raw.splitlines() if not ln.lstrip().startswith("#"))
    r = allowed_pw.sub("", code)
    if re.search(r"playwright|a11y-shard-exec", r, re.I):
        run_bad.append(f"a11y-test: step {nm!r} runs Playwright or the tokenizer; "
                       f"only the step whose run is exactly `{RUN}` may")
if len(target) != 1:
    run_bad.append(f"a11y-test: want exactly one step whose run is exactly `{RUN}` "
                   f"(nothing before or after it), found {len(target)}")
else:
    st = target[0]
    if set(st) != STEP_KEYS:
        run_bad.append(f"a11y-test: the Playwright run step must have exactly the keys {sorted(STEP_KEYS)} "
                       f"(no if/continue-on-error/shell/working-directory), got {sorted(st)}")
    if st.get("env") != STEP_ENV:
        run_bad.append(f"a11y-test: the Playwright run step's env must be exactly {STEP_ENV}, got {st.get('env')}")
if job.get("continue-on-error") not in (None, False):
    run_bad.append(f"a11y-test: job-level continue-on-error must be absent, got {job.get('continue-on-error')!r}")
# Job env reaches the run step for every key STEP_ENV does not set (NODE_OPTIONS,
# PW_TEST_*); a container or service changes the image and filesystem it runs in.
for k in ("env", "container", "services"):
    if k in job:
        run_bad.append(f"a11y-test: job-level `{k}` must be absent (it changes what the run step runs with), "
                       f"got {job.get(k)!r}")
for where, d in (("job", job), ("workflow", wf)):
    if isinstance(d, dict) and d.get("defaults") is not None:
        run_bad.append(f"a11y-test: {where}-level `defaults` must be absent (it can change the run step's "
                       f"shell or directory), got {d.get('defaults')!r}")
if set(job["strategy"]["matrix"]) != {"include"}:
    run_bad.append(f"a11y-test: the matrix must hold only `include`, got {sorted(job['strategy']['matrix'])}")

# ---- the background build's step order (see the header) -------------------
# web/static/css/styles.css and every *_templ.go are COMMITTED, and nothing else
# in CI checks styles.css is fresh. So a `go build` started before templ or
# Tailwind finished (or a generator run while it compiles) still succeeds: it
# embeds the stale committed CSS and the contrast specs test the wrong sheet,
# silently. The order is asserted here instead. A step is identified by its
# `name`; the two unnamed setup actions by their `uses` (the action, before @).
SETUP_GO, SETUP_NODE = "uses: actions/setup-go", "uses: actions/setup-node"
GO_RESTORE, TEMPL, TAILWIND = "Restore the a11y binary's Go cache", "Generate templ", "Build Tailwind CSS"
BG_START, BG_WAIT = "Start the binary build (background)", "Wait for the binary build"
GO_SAVE, SERVER = "Save the a11y binary's Go cache", "Start Stillwater (ephemeral)"
# Each consecutive pair must be in this order (strictly; each step exactly once).
BG_ORDER = [(SETUP_GO, GO_RESTORE), (GO_RESTORE, TEMPL), (TEMPL, TAILWIND), (TAILWIND, BG_START),
            (BG_START, SETUP_NODE), (BG_START, BG_WAIT), (BG_WAIT, SERVER), (BG_WAIT, GO_SAVE)]
def step_id(x):
    if x.get("name") is not None:
        return str(x["name"])
    u = x.get("uses")
    return f"uses: {str(u).split('@', 1)[0]}" if u is not None else None
ids = [step_id(x) for x in steps]
at = {}
for want in dict.fromkeys(k for pair in BG_ORDER for k in pair):
    hits = [i for i, s in enumerate(ids) if s == want]
    if len(hits) != 1:
        run_bad.append(f"a11y-test step order: want exactly one step {want!r}, found {len(hits)}"
                       + (f" (steps #{', #'.join(str(i + 1) for i in hits)})" if hits else "")
                       + "; the background build's order cannot be checked without it")
    else:
        at[want] = hits[0]
for a, b in BG_ORDER:
    if a in at and b in at and not at[a] < at[b]:
        run_bad.append(f"a11y-test step order: step {a!r} (#{at[a] + 1}) must come before step {b!r} "
                       f"(#{at[b] + 1}); go build must start only after the files it embeds are generated")
# While the build compiles, no step may (re)generate or edit what it reads. The
# check is TEXTUAL, on each step's `run` (whole-line comments dropped) and
# `uses`: any mention of web/static/, templ, tailwind (any case), go generate,
# or a *.go / *.templ file name fails, whether or not the step would write it.
gen_tok = re.compile(r"web/static/|\btempl\b|tailwind|\bgo\s+generate\b|\.go\b|\.templ\b", re.I)
if BG_START in at and BG_WAIT in at:
    for x in steps[at[BG_START] + 1:at[BG_WAIT]]:
        raw = str(x.get("run", ""))
        code = "\n".join(ln for ln in raw.splitlines() if not ln.lstrip().startswith("#"))
        hit = sorted({m.group(0) for m in gen_tok.finditer(code + "\n" + str(x.get("uses", "")))})
        if hit:
            run_bad.append(f"a11y-test step order: step {step_id(x)!r} runs between {BG_START!r} and {BG_WAIT!r} "
                           f"and mentions {hit}; nothing may regenerate or edit the build's inputs "
                           f"(web/static/, templ, Tailwind, go generate, *.go, *.templ) while it compiles")
# RUN's argv, with the four CI-bound words mapped to this checkout. Each must
# occur exactly once, so an edit to RUN that drops or repeats one fails here.
PROJECT_WORD = '--project="$PROJECT"'
PLAIN_WORD = re.compile(r"[A-Za-z0-9_./=-]+")
def run_argv(project, cfg_abs):
    words = RUN.split(" ")
    odd = [w for w in words if w != PROJECT_WORD and not PLAIN_WORD.fullmatch(w)]
    assert not odd, (f"RUN no longer maps onto a local argv: {odd} not plain words (shell syntax, quoting "
                     f"or a doubled space; only {PROJECT_WORD} may quote): {RUN!r}")
    subst = {"scripts/a11y-shard-exec.sh": [exec_sh], "npx": [], "playwright": [pw],
             "--config=playwright.config.js": [f"--config={cfg_abs}"], PROJECT_WORD: [f"--project={project}"]}
    assert all(words.count(k) == 1 for k in subst), f"RUN no longer maps onto a local argv: {words}"
    out = []
    for a in words:
        out += subst.pop(a) if a in subst else [a]
    assert out[:2] == ["bash", exec_sh], f"RUN no longer maps onto a local argv: {words}"
    return out
try:
    run_argv("x", "x")
except AssertionError as ex:
    fail(str(ex))

# ---- layer 1: structural ------------------------------------------------------
good = lambda v: type(v) is int and v > 0
by, bad, toks, tok_bad = {}, list(run_bad), {}, set()
for e in inc:
    p, specs = e.get("project"), e.get("specs")
    if not isinstance(p, str) or not good(e.get("shard")) or not good(e.get("shards")):
        bad.append(f"malformed entry (need project, positive-int shard and shards): {e}")
    elif specs is not None and (not isinstance(specs, str) or not specs):
        bad.append(f"malformed entry (specs must be a non-empty string of paths): {e}")
    else:
        if specs is not None:
            t, err = tokens(e)
            if err:
                bad.append(f"{p} shard {e['shard']}: invalid specs ({err})")
                tok_bad.add(p)
            toks[id(e)] = t or []
        by.setdefault(p, []).append(e)
for p in sorted(expected - set(by)):
    bad.append(f"{p}: declared in playwright.config.js but absent from the matrix")
for p in sorted(set(by) - expected):
    bad.append(f"{p}: in the matrix but not a project in playwright.config.js")
for p, ents in sorted(by.items()):
    totals = {e["shards"] for e in ents}
    got = sorted(e["shard"] for e in ents)
    if len(totals) != 1 or got != list(range(1, next(iter(totals)) + 1)):
        bad.append(f"{p}: shard={got} shards={sorted(totals)}")

# Spec files Playwright would discover: its default testMatch under testDir
# (the config sets neither testMatch nor testIgnore; layer 2 is the authority).
m = re.search(r"testDir:\s*'([^']+)'", cfg_src)
test_dir = os.path.normpath(os.path.join(cfg_dir, m.group(1))) if m else None
spec_re = re.compile(r"\.(spec|test)\.[cm]?[jt]sx?$")
on_disk = set()
if test_dir is None or not os.path.isdir(test_dir):
    bad.append(f"cannot find testDir in {cfg}")
else:
    for d, dirs, files in os.walk(test_dir):
        dirs[:] = [x for x in dirs if x != "node_modules"]
        on_disk |= {os.path.relpath(os.path.join(d, f), cfg_dir) for f in files if spec_re.search(f)}
mech = {}
for p, ents in sorted(by.items()):
    has = {e.get("specs") is not None for e in ents}
    if len(has) != 1:
        bad.append(f"{p}: mixes `specs` lists and --shard entries; use one mechanism per project")
        continue
    mech[p] = "specs" if has == {True} else "shard"
    if mech[p] != "specs":
        continue
    owner = collections.defaultdict(list)
    for e in ents:
        for s in toks[id(e)]:
            owner[os.path.normpath(s)].append(e["shard"])
    for s, shards in sorted(owner.items()):
        if len(shards) > 1:
            bad.append(f"{p}: {s} listed in more than one shard {shards}")
        if s not in on_disk:
            bad.append(f"{p}: shard {shards[0]} lists {s}, which is not a spec file under testDir")
    # An unreadable list owns nothing; its files are not "in no list" (noise).
    for s in sorted(on_disk - set(owner) if p not in tok_bad else ()):
        bad.append(f"{p}: spec file in no shard list: {s} (add it to a `specs` list; see the bins comment in ci.yml)")

# ---- layer 2: playwright test --list -----------------------------------------
def listing(project, e):
    """e is None: the project's WHOLE suite, independent of RUN (the ground truth:
    a selector added to RUN must show up as a gap, not shrink the baseline).
    Else entry e's selection: RUN's own argv, through the tokenizer with e's env."""
    cfg_abs = os.path.join(cfg_dir, os.path.basename(cfg))
    if e is None:
        argv, env = [pw, "test", f"--config={cfg_abs}", f"--project={project}"], None
    else:
        argv, env = run_argv(project, cfg_abs), entry_env(e)
    r = subprocess.run(argv + ["--list", "--reporter=json"], env=env, cwd=cfg_dir, capture_output=True, text=True)
    try:
        d = json.loads(r.stdout)
    except ValueError:
        return None, f"exit {r.returncode}: {(r.stderr or r.stdout).strip()[-400:]}", None
    # Every shape assumption below fails as a listing error (a FAIL line), never
    # a traceback, and never as a vacuous pass (R2-M1: no projects is an error).
    try:
        root, tests = d["config"]["rootDir"], []
        def walk(suite):
            for sp in suite.get("specs", []):
                f = os.path.relpath(os.path.realpath(os.path.join(root, sp["file"])), cfg_dir)
                for t in sp["tests"]:
                    if t["projectName"] == project:
                        tests.append((f, sp["line"], sp["column"], sp["title"], sp["id"]))
            for s in suite.get("suites", []):
                walk(s)
        for s in d.get("suites", []):
            walk(s)
        errs = "; ".join(x.get("message", "").strip().splitlines()[0] for x in d.get("errors", []) if x.get("message"))
        plist = d["config"]["projects"]
        if not isinstance(plist, list) or not plist or not all(
                isinstance(x, dict) and isinstance(x.get("name"), str) and x["name"] for x in plist):
            return None, f"unexpected --list JSON: config.projects must be a non-empty list of named projects, got {plist!r:.200}", None
        projects = {x["name"] for x in plist}
    except (KeyError, TypeError, AttributeError, IndexError) as ex:
        return None, f"unexpected --list JSON shape ({type(ex).__name__}: {ex})", None
    return tests, (f"exit {r.returncode}: {errs}" if r.returncode or errs else None), projects

listed = {}
if list_mode == "off":
    pass  # layer 1 only, at the caller's explicit request (see the header)
elif not os.access(pw, os.X_OK):
    bad.append(f"--list proof needs {pw}; run `npm ci` (or A11Y_SHARDS_LIST=off for layer 1 only)")
else:
    jobs = {}
    for p in sorted(mech):
        jobs[(p, 0)] = None
        for e in by[p]:
            jobs[(p, e["shard"])] = e
    with concurrent.futures.ThreadPoolExecutor(max_workers=4) as ex:
        res = dict(zip(jobs, ex.map(lambda k: listing(k[0], jobs[k]), jobs)))
    short = lambda t: f"{t[0]}:{t[1]} > {t[3]}"
    # The authoritative project set: what Playwright loads from the config, not
    # the layer-1 regex (which sees only single-quoted `*-a11y` names).
    loaded = set().union(*(res[(p, 0)][2] or set() for p in sorted(mech))) if mech else set()
    for p in sorted(loaded - set(by)):
        bad.append(f"{p} --list: a project Playwright loads from {os.path.basename(cfg)} but absent from the matrix")
    for p in sorted(mech):
        full, err, _ = res[(p, 0)]
        if err or not full:
            bad.append(f"{p} --list: full listing failed ({err or 'no tests'})")
            continue
        seen = collections.Counter()
        ok = True
        for e in sorted(by[p], key=lambda e: e["shard"]):
            k = e["shard"]
            tests, err, _ = res[(p, k)]
            if err or tests is None:
                bad.append(f"{p} --list: shard {k} listing failed ({err})")
                ok = False
                continue
            seen.update(t[4] for t in tests)
            listed[(p, k)] = len(tests)
            if mech[p] == "specs":
                want = {os.path.normpath(s) for s in toks[id(e)]}
                got = {t[0] for t in tests}
                if got - want:
                    bad.append(f"{p} --list: shard {k}'s filters also select files it does not list "
                               f"(filters are regex substring matches; use the full path): {sorted(got - want)}")
                if want - got:
                    bad.append(f"{p} --list: shard {k} lists files that select no tests: {sorted(want - got)}")
        if not ok:
            continue
        ids = {t[4]: t for t in full}
        dup = [ids[i] for i, n in seen.items() if n > 1 and i in ids]
        gap = [t for i, t in ids.items() if i not in seen]
        extra = [i for i in seen if i not in ids]
        if dup:
            bad.append(f"{p} --list: {len(dup)} test(s) run in more than one shard, e.g. " + "; ".join(map(short, dup[:3])))
        if gap:
            bad.append(f"{p} --list: {len(gap)} test(s) run in NO shard, e.g. " + "; ".join(map(short, gap[:3])))
        if extra:
            bad.append(f"{p} --list: {len(extra)} shard test id(s) absent from the full listing")
        if not (dup or gap or extra):
            listed[(p, 0)] = len(full)

if bad:
    fail(*bad)
def summary(p):
    s = f"{p} x{len(by[p])} ({mech[p]})"
    if (p, 0) in listed:
        parts = "+".join(str(listed[(p, e['shard'])]) for e in sorted(by[p], key=lambda e: e["shard"]))
        s += f" --list {parts}={listed[(p, 0)]} tests, no overlap"
    return s
print("OK: a11y-test matrix shards complete: " + ", ".join(summary(p) for p in sorted(by)))
PYEOF
