#!/bin/bash
# check-a11y-shards.sh -- assert the a11y-test matrix in ci.yml covers every
# Playwright shard (#3442). Per project, every entry must carry `shard` and
# `shards` as positive integers, `shards` constant, the entry count equal to it,
# and the `shard` values exactly 1..N. A missing index, a duplicate, or `shards`
# bumped without a new entry would otherwise pass green while silently dropping
# specs. The expected project set is the `*-a11y` project names declared in
# playwright.config.js, so a project removed from the matrix is caught too.
# Malformed input fails with the FAIL line, never a traceback.
# Usage: bash scripts/check-a11y-shards.sh [ci.yml [playwright.config.js]]
set -euo pipefail
ROOT="$(cd "$(dirname "$0")/.." && pwd)"
CI="${1:-$ROOT/.github/workflows/ci.yml}"
CFG="${2:-$ROOT/playwright.config.js}"
python3 -c 'import yaml' 2>/dev/null || { echo "check-a11y-shards: python3 with PyYAML required" >&2; exit 2; }
python3 - "$CI" "$CFG" <<'PYEOF'
import re, sys, yaml
def fail(*msgs):
    print("FAIL: a11y-test matrix shards incomplete:\n  " + "\n  ".join(msgs))
    sys.exit(1)
try:
    expected = set(re.findall(r"name:\s*'([\w-]+-a11y)'", open(sys.argv[2]).read()))
    inc = yaml.safe_load(open(sys.argv[1]))["jobs"]["a11y-test"]["strategy"]["matrix"]["include"]
    assert isinstance(inc, list) and all(isinstance(e, dict) for e in inc)
except Exception as e:
    fail(f"cannot read the matrix or project list ({type(e).__name__}: {e})")
if not expected:
    fail("no *-a11y projects found in " + sys.argv[2])
good = lambda v: type(v) is int and v > 0
by, bad = {}, []
for e in inc:
    p = e.get("project")
    if not isinstance(p, str) or not good(e.get("shard")) or not good(e.get("shards")):
        bad.append(f"malformed entry (need project, positive-int shard and shards): {e}")
    else:
        by.setdefault(p, []).append((e["shard"], e["shards"]))
for p in sorted(expected - set(by)):
    bad.append(f"{p}: declared in playwright.config.js but absent from the matrix")
for p in sorted(set(by) - expected):
    bad.append(f"{p}: in the matrix but not a project in playwright.config.js")
for p, ents in sorted(by.items()):
    totals = {t for _, t in ents}
    got = sorted(s for s, _ in ents)
    if len(totals) != 1 or got != list(range(1, next(iter(totals)) + 1)):
        bad.append(f"{p}: shard={got} shards={sorted(totals)}")
if bad:
    fail(*bad)
print("OK: a11y-test matrix shards complete: " + ", ".join(f"{p} x{len(v)}" for p, v in sorted(by.items())))
PYEOF
