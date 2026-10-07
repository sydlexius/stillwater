#!/bin/bash
# test-check-a11y-shards.sh -- mutation tests for check-a11y-shards.sh (#3442).
set -euo pipefail
D="$(cd "$(dirname "$0")" && pwd)"
CI="$D/../.github/workflows/ci.yml"
CFG="$D/../playwright.config.js"
T="$(mktemp -d)"; trap 'rm -rf "$T"' EXIT
rc=0
# expect <marker> <name> <ci-file> [config]: the checker must print the marker
# (so a traceback never passes as a rejection) and exit 0 only for OK.
expect() {
  local out s=0 want=1
  out=$(bash "$D/check-a11y-shards.sh" "$3" "${4:-$CFG}" 2>&1) || s=$?
  [ "$1" = "OK" ] && want=0
  if [ "$s" -eq "$want" ] && grep -q "^$1: a11y-test matrix shards" <<<"$out"; then
    echo "PASS: $2"
  else echo "FAIL: $2 (want $1/$want, got $s)"; printf '%s\n' "$out" | sed 's/^/      /'; rc=1; fi
}
python3 - "$CI" "$T" "$CFG" <<'PYEOF'
import re, sys
s = open(sys.argv[1]).read(); t = sys.argv[2]
ff = re.compile(r"          - project: firefox-a11y\n            engine: firefox\n            shard: (\d)\n            shards: 3\n")
def w(name, text): open(f"{t}/{name}.yml", "w").write(text)
def only(entry):  # one firefox entry, as written, others dropped
    n = [0]
    def f(m):
        n[0] += 1
        return entry if n[0] == 1 else ""
    return ff.sub(f, s)
ent = lambda body: "          - project: firefox-a11y\n            engine: firefox\n" + body
i = s.index(ent("            shard: 2")); j = s.index("          - project: firefox-a11y", i + 10)
w("missing", s[:i] + s[j:])
w("dup", s.replace("            shard: 2\n            shards: 3\n", "            shard: 1\n            shards: 3\n"))
w("mismatch", s.replace("            shard: 2\n            shards: 3\n", "            shard: 2\n            shards: 4\n"))
w("bool", only(ent("            shard: 1\n            shards: true\n")))
w("zero", only(ent("            shard: 1\n            shards: 0\n")))
w("nototal", only(ent("            shard: 1\n")))
w("strshard", only(ent("            shard: one\n            shards: 1\n")))
w("noshard", only(ent("            shards: 1\n")))
w("noproject", ff.sub("", s))
w("nojob", s.replace("  a11y-test:\n", "  a11y-renamed:\n", 1))
open(f"{t}/extra.js", "w").write(open(sys.argv[3]).read() + "\n  { name: 'webkit-a11y' },\n")
PYEOF
expect OK "real ci.yml" "$CI"
for c in missing dup mismatch bool zero nototal strshard noshard noproject nojob; do
  expect FAIL "rejects $c" "$T/$c.yml"
done
expect FAIL "rejects a config project absent from the matrix" "$CI" "$T/extra.js"
exit "$rc"
