#!/bin/bash
# test-check-a11y-shards.sh -- mutation tests for check-a11y-shards.sh (#3442).
#
# Every case names the message it expects, so a rejection for the WRONG reason
# (or a traceback) never passes. Structural cases run with playwright hidden;
# --list cases run Playwright against a scratch copy of the harness (config,
# package.json, tests/a11y copied; node_modules and the repo dirs the specs read
# at import time symlinked), so a mutation never touches the working tree.
# The --list cases FAIL without node_modules, unless the caller passed
# A11Y_SHARDS_LIST=off (the pre-push gate, only when no partition input changed).
set -euo pipefail
D="$(cd "$(dirname "$0")" && pwd)"
REPO="$(cd "$D/.." && pwd)"
CI="$REPO/.github/workflows/ci.yml"
CFG="$REPO/playwright.config.js"
MODE="${A11Y_SHARDS_LIST:-require}"
T="$(cd "$(mktemp -d)" && pwd -P)"; trap 'rm -rf "$T"' EXIT
rc=0
# expect <OK|FAIL> <name> <ci-file> <config> <pw-bin> [substring...]: the checker
# must print the marker, exit 0 only for OK, and print every substring.
# LIST_MODE (default off: structural cases) is the checker's A11Y_SHARDS_LIST;
# CHECKER (default the real one) is the checker script to run.
expect() {
  local want_m="$1" name="$2" ci="$3" cfg="$4" pw="$5"; shift 5
  local out s=0 want=1 miss=""
  out=$(A11Y_SHARDS_LIST="${LIST_MODE:-off}" PLAYWRIGHT_BIN="$pw" \
        bash "${CHECKER:-$D/check-a11y-shards.sh}" "$ci" "$cfg" 2>&1) || s=$?
  [ "$want_m" = "OK" ] && want=0
  for sub in "$@"; do grep -qF -- "$sub" <<<"$out" || miss="$miss [$sub]"; done
  if [ "$s" -eq "$want" ] && grep -q "^$want_m: a11y-test matrix shards" <<<"$out" && [ -z "$miss" ]; then
    echo "PASS: $name"
  else
    echo "FAIL: $name (want $want_m/$want, got $s; missing:${miss:- none})"
    printf '%s\n' "$out" | sed 's/^/      /'; rc=1
  fi
}

python3 - "$CI" "$T" "$CFG" <<'PYEOF'
import re, sys
s = open(sys.argv[1]).read(); t = sys.argv[2]
def w(name, text):
    assert text != s, f"mutation {name} changed nothing"
    open(f"{t}/{name}.yml", "w").write(text)
def sub1(old, new, text=s):
    assert text.count(old) >= 1, f"anchor not found: {old!r}"
    return text.replace(old, new, 1)
# chromium entries carry only shard/shards: the --shard mechanism.
cr = lambda k, n="3": f"          - project: chromium-a11y\n            engine: chromium\n            shard: {k}\n            shards: {n}\n"
i = s.index(cr(2)); j = s.index(cr(3))
w("missing", s[:i] + s[j:])
w("dup", sub1(cr(2), cr(1)))
w("mismatch", sub1(cr(2), cr(2, "4")))
w("bool", sub1(cr(1), cr(1, "true")))
w("zero", sub1(cr(1), cr(1, "0")))
w("nototal", sub1(cr(1), "          - project: chromium-a11y\n            engine: chromium\n            shard: 1\n"))
w("strshard", sub1(cr(1), cr("one")))
w("noshard", sub1(cr(1), "          - project: chromium-a11y\n            engine: chromium\n            shards: 3\n"))
w("noproject", re.sub(r"          - project: firefox-a11y\n(            .*\n|              .*\n)+", "", s))
w("nojob", sub1("  a11y-test:\n", "  a11y-renamed:\n"))
# specs-list mutations (firefox).
w("mixed", sub1(cr(1), cr(1) + "            specs: tests/a11y/contrast.spec.js\n"))
w("emptyspecs", sub1("            specs: >-\n              tests/a11y/contrast.spec.js\n", "            specs: ''\n"))
w("droppath", sub1("              tests/a11y/nfo-mbid.spec.js\n", ""))
w("duppath", sub1("              tests/a11y/contrast.spec.js\n",
                  "              tests/a11y/contrast.spec.js\n              tests/a11y/nfo-mbid.spec.js\n"))
w("barename", sub1("              tests/a11y/contrast.spec.js\n", "              contrast.spec.js\n"))
w("ghost", sub1("              tests/a11y/contrast.spec.js\n",
                "              tests/a11y/contrast.spec.js\n              tests/a11y/no-such-file.spec.js\n"))
# C1 (#3442): the tokenizer, shared by the guard and CI's run step.
b2 = "            specs: >-\n              tests/a11y/base-path-filter-idiom.spec.js\n"
w("literal", sub1(b2, b2.replace(">-", "|")))
w("nbsp", sub1("              tests/a11y/base-path-filter-idiom.spec.js\n              ",
               "              tests/a11y/base-path-filter-idiom.spec.js\u00a0"))
w("badchar", sub1("              tests/a11y/contrast.spec.js\n", "              tests/a11y/contrast.spec.js$\n"))
w("dashtoken", sub1("              tests/a11y/contrast.spec.js\n", "              tests/a11y/contrast.spec.js --headed\n"))
w("blankspecs", sub1("            specs: >-\n              tests/a11y/contrast.spec.js\n", "            specs: '   '\n"))
run = "          bash scripts/a11y-shard-exec.sh npx playwright test --config=playwright.config.js --project=\"$PROJECT\"\n"
w("oldrun", sub1(run, '          read -ra sel <<<"$SPECS"\n          npx playwright test --project="$PROJECT" "${sel[@]}"\n'))
w("envdrift", sub1("          SPECS: ${{ matrix.specs }}\n", "          SPECS: ${{ matrix.files }}\n"))
# R2-C1: the run step is pinned exactly. Each of these passed a prefix match.
r1 = run.rstrip("\n")
for name, text in {
    "or-true": r1 + " || true\n",
    "pipe-tee": r1 + " | tee x\n",
    "semi-exit0": r1 + "; exit 0\n",
    "and-true": r1 + " && true\n",
    "set-e-off": "          set +e\n" + run,
    "extra-shard": r1 + " --shard=1/2\n",
    "hard-project": run.replace('--project="$PROJECT"', "--project=firefox-a11y"),
    "grep": r1 + " --grep=@x\n",
    "other-config": run.replace("--config=playwright.config.js", "--config=other.config.js"),
    "export-specs": "          export SPECS=tests/a11y/contrast.spec.js\n" + run,
}.items():
    w(name, sub1(run, text))
step = "        run: |\n" + run
w("step-coe", sub1(step, "        continue-on-error: true\n" + step))
w("step-if", sub1(step, "        if: ${{ false }}\n" + step))
w("step-shell", sub1(step, "        shell: sh -c 'true' {0}\n" + step))
w("job-coe", sub1("  a11y-test:\n", "  a11y-test:\n    continue-on-error: true\n"))
w("job-defaults", sub1("  a11y-test:\n", "  a11y-test:\n    defaults:\n      run:\n        working-directory: other\n"))
w("projdrift", sub1("          PROJECT: ${{ matrix.project }}\n", "          PROJECT: firefox-a11y\n"))
w("extraenv", sub1("          PROJECT: ${{ matrix.project }}\n",
                   "          PROJECT: ${{ matrix.project }}\n          PWTEST_SKIP_ALL: '1'\n"))
w("otherstep", sub1("      - name: Stop Stillwater\n",
                    "      - name: Extra\n        run: PW=playwright; npx $PW test --project=chromium-a11y\n\n      - name: Stop Stillwater\n"))
# R3-I1a: job-level keys that reach the run step.
w("job-env", sub1("  a11y-test:\n", "  a11y-test:\n    env:\n      NODE_OPTIONS: --require ./x.js\n"))
w("job-container", sub1("  a11y-test:\n", "  a11y-test:\n    container: node:22\n"))
w("job-services", sub1("  a11y-test:\n", "  a11y-test:\n    services:\n      db:\n        image: redis\n"))
# R3-I1b: another step changing the run step's environment, or naming Playwright
# in a case the old substring test missed.
stop = "      - name: Stop Stillwater\n"
pw_step = "      - name: Run Playwright a11y tests"
def before_run(name, body):
    return sub1(pw_step, f"      - name: {name}\n        run: {body}\n\n" + pw_step)
w("ghpath", before_run("Tooling", 'echo "$RUNNER_TEMP/bin" >> "$GITHUB_PATH"'))
w("ghenv", before_run("Env", 'echo "NODE_OPTIONS=--require=$PWD/x.js" >> "$GITHUB_ENV"  # PLAYWRIGHT'))
w("bashenv", before_run("Shell", "echo BASH_ENV=/tmp/rc > /tmp/env"))
w("upper-pw", sub1(stop, "      - name: Extra\n        run: PW=PLAYWRIGHT; npx ${PW,,} test\n\n" + stop))
# R3-M1: a duplicated mapping key (PyYAML is last-wins; GitHub rejects it).
w("dup-run", sub1(step, "        run: |\n" + run.rstrip("\n") + " || true\n" + step))
# R3-M2: workflow-level defaults, a matrix exclude, a step working-directory.
w("wf-defaults", sub1("\npermissions:", "\ndefaults:\n  run:\n    shell: sh\npermissions:"))
w("matrix-exclude", sub1("        include:\n", "        exclude:\n          - project: chromium-a11y\n            shard: 2\n        include:\n"))
w("step-wd", sub1(step, "        working-directory: .\n" + step))
open(f"{t}/extra.js", "w").write(open(sys.argv[3]).read() + "\n  { name: 'webkit-a11y' },\n")
PYEOF

NOPW="$T/no-playwright"
echo "-- structural (layer 1; playwright hidden)"
expect OK "real ci.yml" "$CI" "$CFG" "$NOPW" "firefox-a11y x4 (specs)" "chromium-a11y x3 (shard)"
expect FAIL "rejects missing" "$T/missing.yml" "$CFG" "$NOPW" "chromium-a11y: shard=[1, 3] shards=[3]"
expect FAIL "rejects dup" "$T/dup.yml" "$CFG" "$NOPW" "chromium-a11y: shard=[1, 1, 3]"
expect FAIL "rejects mismatch" "$T/mismatch.yml" "$CFG" "$NOPW" "shards=[3, 4]"
for c in bool zero nototal strshard noshard; do
  expect FAIL "rejects $c" "$T/$c.yml" "$CFG" "$NOPW" "malformed entry (need project, positive-int shard and shards)"
done
expect FAIL "rejects noproject" "$T/noproject.yml" "$CFG" "$NOPW" "firefox-a11y: declared in playwright.config.js but absent from the matrix"
expect FAIL "rejects nojob" "$T/nojob.yml" "$CFG" "$NOPW" "cannot read the matrix or project list"
expect FAIL "rejects a config project absent from the matrix" "$CI" "$T/extra.js" "$NOPW" "webkit-a11y: declared in playwright.config.js"
expect FAIL "rejects mixed mechanisms" "$T/mixed.yml" "$CFG" "$NOPW" "chromium-a11y: mixes \`specs\` lists and --shard entries"
expect FAIL "rejects empty specs" "$T/emptyspecs.yml" "$CFG" "$NOPW" "specs must be a non-empty string of paths"
expect FAIL "rejects a spec dropped from every list" "$T/droppath.yml" "$CFG" "$NOPW" "spec file in no shard list: tests/a11y/nfo-mbid.spec.js"
expect FAIL "rejects a spec in two lists" "$T/duppath.yml" "$CFG" "$NOPW" "tests/a11y/nfo-mbid.spec.js listed in more than one shard [1, 2]"
expect FAIL "rejects a bare file name" "$T/barename.yml" "$CFG" "$NOPW" "lists contrast.spec.js, which is not a spec file"
expect FAIL "rejects a path that is not a file" "$T/ghost.yml" "$CFG" "$NOPW" "lists tests/a11y/no-such-file.spec.js, which is not a spec file"
expect FAIL "rejects a \`specs: |\` multi-line block (CI's read saw line 1 only)" "$T/literal.yml" "$CFG" "$NOPW" \
  "firefox-a11y shard 2: invalid specs" "on ONE line (use a folded \`>-\` scalar, not \`|\`)"
expect FAIL "rejects a U+00A0 separator (bash does not split on it)" "$T/nbsp.yml" "$CFG" "$NOPW" \
  "firefox-a11y shard 2: invalid specs" "302 240"
expect FAIL "rejects a disallowed character in a token" "$T/badchar.yml" "$CFG" "$NOPW" \
  "firefox-a11y shard 1: invalid specs" "found: \$"
expect FAIL "rejects a token that is a flag" "$T/dashtoken.yml" "$CFG" "$NOPW" \
  "firefox-a11y shard 1: invalid specs" "token '--headed' starts with '-'"
expect FAIL "rejects whitespace-only specs" "$T/blankspecs.yml" "$CFG" "$NOPW" \
  "firefox-a11y shard 1: invalid specs" "specs is non-empty but holds no paths"
EXACT="want exactly one step whose run is exactly"
expect FAIL "rejects a run step that bypasses a11y-shard-exec.sh" "$T/oldrun.yml" "$CFG" "$NOPW" "$EXACT" \
  "runs Playwright or the tokenizer"
# shellcheck disable=SC2016 # the expected message is a literal "${{ matrix.files }}"
expect FAIL "rejects a run step fed something other than matrix.specs" "$T/envdrift.yml" "$CFG" "$NOPW" \
  "the Playwright run step's env must be exactly" 'got {' "'SPECS': '\${{ matrix.files }}'"
# R2-C1: any edit to the pinned run line fails, whatever it appends or prepends.
for c in or-true pipe-tee semi-exit0 and-true set-e-off extra-shard hard-project grep other-config export-specs; do
  expect FAIL "rejects a run step edited: $c" "$T/$c.yml" "$CFG" "$NOPW" "$EXACT" "runs Playwright or the tokenizer"
done
KEYS="the Playwright run step must have exactly the keys ['env', 'name', 'run']"
expect FAIL "rejects step continue-on-error" "$T/step-coe.yml" "$CFG" "$NOPW" "$KEYS" "'continue-on-error'"
expect FAIL "rejects step if: false" "$T/step-if.yml" "$CFG" "$NOPW" "$KEYS" "'if'"
expect FAIL "rejects step shell override" "$T/step-shell.yml" "$CFG" "$NOPW" "$KEYS" "'shell'"
expect FAIL "rejects job continue-on-error" "$T/job-coe.yml" "$CFG" "$NOPW" "job-level continue-on-error must be absent, got True"
expect FAIL "rejects job defaults" "$T/job-defaults.yml" "$CFG" "$NOPW" "job-level \`defaults\` must be absent"
expect FAIL "rejects PROJECT env drift" "$T/projdrift.yml" "$CFG" "$NOPW" "the Playwright run step's env must be exactly" "'PROJECT': 'firefox-a11y'"
expect FAIL "rejects an extra env key on the run step" "$T/extraenv.yml" "$CFG" "$NOPW" "the Playwright run step's env must be exactly" "PWTEST_SKIP_ALL"
expect FAIL "rejects playwright in another step" "$T/otherstep.yml" "$CFG" "$NOPW" "step 'Extra' runs Playwright or the tokenizer"
expect FAIL "rejects PLAYWRIGHT (any case) in another step" "$T/upper-pw.yml" "$CFG" "$NOPW" "step 'Extra' runs Playwright or the tokenizer"
for c in env container services; do
  expect FAIL "rejects job-level $c" "$T/job-$c.yml" "$CFG" "$NOPW" "a11y-test: job-level \`$c\` must be absent"
done
ENVW="mentions GITHUB_ENV/GITHUB_PATH/BASH_ENV; no step in this job may change the environment"
expect FAIL "rejects a step writing GITHUB_PATH" "$T/ghpath.yml" "$CFG" "$NOPW" "step 'Tooling' $ENVW"
expect FAIL "rejects a step writing GITHUB_ENV" "$T/ghenv.yml" "$CFG" "$NOPW" "step 'Env' $ENVW"
expect FAIL "rejects a step setting BASH_ENV" "$T/bashenv.yml" "$CFG" "$NOPW" "step 'Shell' $ENVW"
expect FAIL "rejects a duplicated run key" "$T/dup-run.yml" "$CFG" "$NOPW" "cannot read the matrix or project list (ConstructorError" "duplicate key 'run'"
expect FAIL "rejects workflow-level defaults" "$T/wf-defaults.yml" "$CFG" "$NOPW" "workflow-level \`defaults\` must be absent"
expect FAIL "rejects a matrix exclude" "$T/matrix-exclude.yml" "$CFG" "$NOPW" "the matrix must hold only \`include\`, got ['exclude', 'include']"
expect FAIL "rejects step working-directory" "$T/step-wd.yml" "$CFG" "$NOPW" "$KEYS" "'working-directory'"
# R3-I2/M2: a COHERENT edit of RUN (checker constant and ci.yml line together).
# A scratch copy of the checker (with the tokenizer beside it) carries the new RUN.
# shellcheck disable=SC2016 # RUN's literal text: $PROJECT is expanded by CI's bash, not here
base='bash scripts/a11y-shard-exec.sh npx playwright test --config=playwright.config.js --project="$PROJECT"'
runedit() { # <name> <new RUN> [drop-specs]: writes $T/re-<name>/scripts/check-a11y-shards.sh and $T/re-<name>.yml
  mkdir -p "$T/re-$1/scripts"
  cp "$D/a11y-shard-exec.sh" "$T/re-$1/scripts/"
  NEW="$2" BASE="$base" DROP="${3:-}" python3 - "$D/check-a11y-shards.sh" "$CI" "$T/re-$1/scripts/check-a11y-shards.sh" "$T/re-$1.yml" <<'PYEOF'
import os, re, sys
new, base = os.environ["NEW"], os.environ["BASE"]
chk, ci = open(sys.argv[1]).read(), open(sys.argv[2]).read()
a, b = "RUN = '%s'\n" % base, "          %s\n" % base
assert chk.count(a) == 1 and ci.count(b) == 1, "RUN anchors not found"
chk = chk.replace(a, "RUN = %r\n" % new)
ci = ci.replace(b, "          %s\n" % new)
if os.environ["DROP"]:
    ci, n = re.subn(r"            specs: >-\n(              .*\n)+", "", ci)
    assert n == 4, n
open(sys.argv[3], "w").write(chk); open(sys.argv[4], "w").write(ci)
PYEOF
}
MAPS="RUN no longer maps onto a local argv"
runedit or-true "$base || true"
CHECKER="$T/re-or-true/scripts/check-a11y-shards.sh" expect FAIL "rejects a coherent RUN edit: || true" \
  "$T/re-or-true.yml" "$CFG" "$NOPW" "$MAPS" "['||'] not plain words"
runedit or-true-nospecs "$base || true" drop
CHECKER="$T/re-or-true-nospecs/scripts/check-a11y-shards.sh" expect FAIL "rejects a coherent RUN edit: || true, every project on --shard" \
  "$T/re-or-true-nospecs.yml" "$CFG" "$NOPW" "$MAPS"
runedit subshell "$base \$(true)"
CHECKER="$T/re-subshell/scripts/check-a11y-shards.sh" expect FAIL "rejects a coherent RUN edit: \$(true)" \
  "$T/re-subshell.yml" "$CFG" "$NOPW" "$MAPS" "['\$(true)'] not plain words"
# The CI side of the same value: the run step's own command must refuse it too,
# never run a partial selection.
lit=$(python3 -c 'import sys,yaml; inc=yaml.safe_load(open(sys.argv[1]))["jobs"]["a11y-test"]["strategy"]["matrix"]["include"]; sys.stdout.write([e for e in inc if e["project"]=="firefox-a11y" and e["shard"]==2][0]["specs"])' "$T/literal.yml")
s=0; SPECS="$lit" SW_A11Y_SHARD=2/4 bash "$D/a11y-shard-exec.sh" true 2>/dev/null || s=$?
n=$(printf '%s\n' "$lit" | wc -l | tr -d ' ')
if [ "$s" -eq 1 ] && [ "$n" -gt 1 ]; then echo "PASS: run step refuses the $n-line \`|\` value (exit 1, nothing run)"
else echo "FAIL: run step on the \`|\` value: exit $s, lines $n"; rc=1; fi
s=0; SPECS="" SW_A11Y_SHARD="" bash "$D/a11y-shard-exec.sh" true 2>/dev/null || s=$?
if [ "$s" -eq 1 ]; then echo "PASS: run step refuses empty specs with no shard"; else echo "FAIL: empty specs+shard exit $s"; rc=1; fi
out=$(SPECS="tests/a11y/a.spec.js tests/a11y/b.spec.js" SW_A11Y_SHARD=2/4 bash "$D/a11y-shard-exec.sh" printf '<%s>' 2>&1)
if [ "$out" = "a11y-shard-exec: shard 2/4: 2 spec file filter(s): tests/a11y/a.spec.js tests/a11y/b.spec.js
<tests/a11y/a.spec.js><tests/a11y/b.spec.js>" ]; then echo "PASS: run step logs and passes exactly the listed files"
else echo "FAIL: run step argv: $out"; rc=1; fi
# R2-M4: the exec script refuses on its own what only the checker caught before.
for v in . .. / /etc/passwd tests/../x.spec.js tests/a11y/ tests//a.spec.js ./tests/a.spec.js; do
  s=0; out=$(SPECS="tests/a11y/a.spec.js $v" SW_A11Y_SHARD=1/4 bash "$D/a11y-shard-exec.sh" true 2>&1) || s=$?
  if [ "$s" -eq 1 ] && grep -qF "specs token '$v' is not a normalized repo-relative path" <<<"$out"; then
    echo "PASS: run step refuses token '$v'"
  else echo "FAIL: token '$v': exit $s: $out"; rc=1; fi
done
for v in "5/3:exceeds shards 3" "0/3:is not <shard>/<shards>" "3:is not <shard>/<shards>" "1234567/9999999:is not <shard>/<shards>"; do
  for sp in "" tests/a11y/a.spec.js; do
    s=0; out=$(SPECS="$sp" SW_A11Y_SHARD="${v%%:*}" bash "$D/a11y-shard-exec.sh" true 2>&1) || s=$?
    if [ "$s" -eq 1 ] && grep -qF "${v#*:}" <<<"$out"; then echo "PASS: run step refuses SW_A11Y_SHARD=${v%%:*} (specs='$sp')"
    else echo "FAIL: SW_A11Y_SHARD=${v%%:*} specs='$sp': exit $s: $out"; rc=1; fi
  done
done
# R2-M3: the caller's LC_ALL reaches the command unchanged: set, set-empty, unset.
# shellcheck disable=SC2016 # expanded by the child sh, not here
lc='printf "<%s>" "${LC_ALL-UNSET}"'
got_set=$(LC_ALL=en_US.UTF-8 SPECS='' SW_A11Y_SHARD=1/1 bash "$D/a11y-shard-exec.sh" sh -c "$lc" 2>/dev/null)
got_empty=$(LC_ALL='' SPECS='' SW_A11Y_SHARD=1/1 bash "$D/a11y-shard-exec.sh" sh -c "$lc" 2>/dev/null)
got_unset=$(env -u LC_ALL SPECS='' SW_A11Y_SHARD=1/1 bash "$D/a11y-shard-exec.sh" sh -c "$lc" 2>/dev/null)
if [ "$got_set|$got_empty|$got_unset" = "<en_US.UTF-8>|<>|<UNSET>" ]; then
  echo "PASS: run step restores the caller's LC_ALL (set, set-empty, unset)"
else echo "FAIL: LC_ALL restore: set=$got_set empty=$got_empty unset=$got_unset"; rc=1; fi
LIST_MODE=require expect FAIL "the --list layer is required by default: no playwright FAILS" "$CI" "$CFG" "$NOPW" "--list proof needs $NOPW; run \`npm ci\`"
s=0; A11Y_SHARDS_LIST=maybe bash "$D/check-a11y-shards.sh" "$CI" "$CFG" >/dev/null 2>&1 || s=$?
if [ "$s" -eq 2 ]; then echo "PASS: rejects an unknown A11Y_SHARDS_LIST with exit 2"; else echo "FAIL: A11Y_SHARDS_LIST=maybe exit $s, want 2"; rc=1; fi

echo "-- --list (layer 2)"
PW="$REPO/node_modules/.bin/playwright"
if [ "$MODE" = "off" ]; then
  echo "SKIP: --list cases (A11Y_SHARDS_LIST=off from the caller); CI's required 'Gate Invariant' job runs them"
  exit "$rc"
fi
if [ ! -x "$PW" ]; then echo "FAIL: --list cases need $PW; run npm ci"; exit 1; fi
C="$T/repo"; mkdir -p "$C/tests"
cp "$CFG" "$REPO/package.json" "$C/"
cp -R "$REPO/tests/a11y" "$C/tests/a11y"
for l in node_modules internal web; do ln -s "$REPO/$l" "$C/$l"; done
CC="$C/playwright.config.js"
LIST_MODE=require expect OK "real ci.yml: --list union equals the full list" "$CI" "$CC" "$PW" \
  "firefox-a11y x4 (specs) --list" "chromium-a11y x3 (shard) --list" "no overlap"
# A NEW spec that no list names: both layers must report it.
cat > "$C/tests/a11y/zz-shard-guard-dummy.spec.js" <<'JS'
import { test } from 'playwright/test';
test('shard guard dummy', async () => {});
JS
LIST_MODE=require expect FAIL "rejects a new spec in no list" "$CI" "$CC" "$PW" \
  "spec file in no shard list: tests/a11y/zz-shard-guard-dummy.spec.js" \
  "firefox-a11y --list: 1 test(s) run in NO shard, e.g. tests/a11y/zz-shard-guard-dummy.spec.js"
rm "$C/tests/a11y/zz-shard-guard-dummy.spec.js"
LIST_MODE=require expect FAIL "--list: rejects a spec dropped from every list" "$T/droppath.yml" "$CC" "$PW" \
  "firefox-a11y --list:" "run in NO shard, e.g. tests/a11y/nfo-mbid.spec.js"
LIST_MODE=require expect FAIL "--list: rejects a spec in two lists" "$T/duppath.yml" "$CC" "$PW" \
  "run in more than one shard, e.g. tests/a11y/nfo-mbid.spec.js"
LIST_MODE=require expect FAIL "--list: rejects a bare name (regex substring match)" "$T/barename.yml" "$CC" "$PW" \
  "shard 1's filters also select files it does not list" "tests/a11y/muted-text-contrast.spec.js" \
  "run in more than one shard"
LIST_MODE=require expect FAIL "--list: rejects a filter that selects nothing" "$T/ghost.yml" "$CC" "$PW" \
  "shard 1 lists files that select no tests: ['tests/a11y/no-such-file.spec.js']"
# M1: a project Playwright loads but the layer-1 regex cannot see (double
# quotes) must fail via the authoritative config.projects of the listing.
python3 - "$CC" "$T/dq.js" <<'PYEOF'
import sys
s = open(sys.argv[1]).read(); a = "  projects: [\n"
assert s.count(a) == 1
open(sys.argv[2], "w").write(s.replace(a, a + '    { name: "webkit-a11y", use: { ...devices[\'Desktop Firefox\'] } },\n'))
PYEOF
cp "$T/dq.js" "$C/dq.config.js"
expect OK "layer 1 alone cannot see a double-quoted project (why layer 2 checks it)" "$CI" "$C/dq.config.js" "$NOPW"
LIST_MODE=require expect FAIL "--list: rejects a loaded project absent from the matrix" "$CI" "$C/dq.config.js" "$PW" \
  "webkit-a11y --list: a project Playwright loads from dq.config.js but absent from the matrix"
# R3-M2: layer 2 runs RUN's own words. A plain-word selector added coherently to
# RUN passes layer 1 and must show up as tests in no shard (the full listing is
# RUN-independent); a layer 2 that ran a fixed argv would print OK here.
runedit grep-invert "$base --grep-invert=contrast"
LIST_MODE=require CHECKER="$T/re-grep-invert/scripts/check-a11y-shards.sh" expect FAIL \
  "--list: a coherent RUN edit that narrows the selection shows as a gap" "$T/re-grep-invert.yml" "$CC" "$PW" \
  "chromium-a11y --list:" "test(s) run in NO shard" "firefox-a11y --list:"
LIST_MODE=require expect FAIL "--list: rejects a \`specs: |\` block" "$T/literal.yml" "$CC" "$PW" \
  "firefox-a11y shard 2: invalid specs" "firefox-a11y --list: shard 2 listing failed"
# R2-M1/M2: a Playwright JSON shape change fails with the FAIL line, never a
# traceback and never a vacuous pass. Each wrapper rewrites the real --list JSON.
cat > "$T/jsonfilt.py" <<'PYEOF'
import json, sys
d = json.load(sys.stdin)
exec(sys.argv[1])
print(json.dumps(d))
PYEOF
mkpw() { # <name> <python statement applied to d>
  cat > "$T/pw-$1" <<SH
#!/bin/bash
out=\$("$PW" "\$@"); rc=\$?
printf '%s' "\$out" | python3 -I "$T/jsonfilt.py" '$2'
exit \$rc
SH
  chmod +x "$T/pw-$1"
}
mkpw noprojects 'd["config"].pop("projects")'
mkpw emptyprojects 'd["config"]["projects"] = []'
mkpw strprojects 'd["config"]["projects"] = [p["name"] for p in d["config"]["projects"]]'
mkpw noconfig 'd.pop("config")'
mkpw null 'd = None'
LIST_MODE=require expect FAIL "--list: fails closed on JSON noprojects (no config.projects key)" "$CI" "$CC" "$T/pw-noprojects" \
  "--list: full listing failed (unexpected --list JSON shape (KeyError: 'projects'))"
for c in emptyprojects strprojects; do
  LIST_MODE=require expect FAIL "--list: fails closed on JSON $c" "$CI" "$CC" "$T/pw-$c" \
    "--list: full listing failed (unexpected --list JSON: config.projects must be a non-empty list of named projects"
done
for c in noconfig null; do
  LIST_MODE=require expect FAIL "--list: FAIL line, not a traceback, on JSON $c" "$CI" "$CC" "$T/pw-$c" \
    "--list: full listing failed (unexpected --list JSON shape"
done
exit "$rc"
