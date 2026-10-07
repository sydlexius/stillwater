#!/bin/bash
# check-a11y-shards.sh -- assert the a11y-test matrix in ci.yml covers every
# Playwright shard (#3442). Per project, the `shard` values must be exactly
# 1..N with N constant across that project's entries. A missing index, a
# duplicate, or `shards` bumped without a new entry would otherwise pass green
# while silently dropping specs. Usage: bash scripts/check-a11y-shards.sh [ci.yml]
set -euo pipefail
CI="${1:-$(cd "$(dirname "$0")/.." && pwd)/.github/workflows/ci.yml}"
python3 -c 'import yaml' 2>/dev/null || { echo "check-a11y-shards: python3 with PyYAML required" >&2; exit 2; }
python3 - "$CI" <<'PYEOF'
import sys, yaml
inc = yaml.safe_load(open(sys.argv[1]))["jobs"]["a11y-test"]["strategy"]["matrix"]["include"]
by = {}
for e in inc:
    by.setdefault(e["project"], []).append((e.get("shard"), e.get("shards")))
bad = []
for proj, ents in sorted(by.items()):
    totals = {t for _, t in ents}
    got = sorted(s for s, _ in ents if isinstance(s, int))
    if len(totals) != 1 or got != list(range(1, next(iter(totals)) + 1)):
        bad.append(f"{proj}: shard={got} shards={sorted(totals, key=str)}")
if bad or not by:
    print("FAIL: a11y-test matrix shards incomplete:\n  " + "\n  ".join(bad or ["no entries"]))
    sys.exit(1)
print("OK: a11y-test matrix shards complete: " + ", ".join(f"{p} x{len(v)}" for p, v in sorted(by.items())))
PYEOF
