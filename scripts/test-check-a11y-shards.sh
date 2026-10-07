#!/bin/bash
# test-check-a11y-shards.sh -- mutation tests for check-a11y-shards.sh (#3442).
set -euo pipefail
D="$(cd "$(dirname "$0")" && pwd)"
CI="$D/../.github/workflows/ci.yml"
T="$(mktemp -d)"; trap 'rm -rf "$T"' EXIT
rc=0
# expect <want-exit> <name> <file>
expect() {
  local s=0; bash "$D/check-a11y-shards.sh" "$3" >/dev/null 2>&1 || s=$?
  if [ "$s" -eq "$1" ]; then echo "PASS: $2"; else echo "FAIL: $2 (want $1, got $s)"; rc=1; fi
}
expect 0 "real ci.yml" "$CI"
# missing index: drop firefox 2/3 (the second `shard: 2` / `shards: 3` pair)
python3 - "$CI" "$T" <<'PYEOF'
import sys, re
s = open(sys.argv[1]).read(); t = sys.argv[2]
a = "            shard: 2\n            shards: 3\n"
assert a in s
open(t + "/dup.yml", "w").write(s.replace(a, "            shard: 1\n            shards: 3\n"))
open(t + "/mismatch.yml", "w").write(s.replace(a, "            shard: 2\n            shards: 4\n"))
i = s.index("          - project: firefox-a11y\n            engine: firefox\n            shard: 2")
j = s.index("          - project: firefox-a11y", i + 10)
open(t + "/missing.yml", "w").write(s[:i] + s[j:])
PYEOF
expect 1 "missing index" "$T/missing.yml"
expect 1 "duplicate index" "$T/dup.yml"
expect 1 "shards mismatch" "$T/mismatch.yml"
exit "$rc"
