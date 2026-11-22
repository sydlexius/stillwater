#!/bin/bash
# test-go-mod-download-retry.sh -- hermetic tests for scripts/go-mod-download-retry.sh
# and its workflow wiring (#3619). A fake `go` on PATH counts `go mod download`
# calls and fails the first N of them; no network, no real toolchain. The
# wiring half parses the workflows with PyYAML (duplicate keys rejected) and
# checks every Go job on the PR path, each placement with its own lookup.
# Run: bash scripts/test-go-mod-download-retry.sh
set -euo pipefail
D="$(cd "$(dirname "$0")" && pwd)"
SUT="$D/go-mod-download-retry.sh"
WF="${GO_MOD_RETRY_WORKFLOWS:-$D/../.github/workflows}"
T="$(mktemp -d)"
trap 'rm -r "$T"' EXIT
rc=0
ok() { echo "PASS: $1"; }
bad() { echo "FAIL: $1"; [ -n "${2:-}" ] && printf '%s\n' "$2" | sed 's/^/      /'; rc=1; }

command -v timeout >/dev/null 2>&1 || { echo "FAIL: 'timeout' (coreutils) is required to run this test" >&2; exit 1; }
python3 -c 'import yaml' 2>/dev/null || { echo "FAIL: python3 with PyYAML is required to run this test" >&2; exit 1; }

mkdir -p "$T/bin"
# fake go: FAKE_GO_FAIL = how many `go mod download` calls fail;
# FAKE_GO_ERR = the stderr line a failing call prints.
cat >"$T/bin/go" <<'EOF'
#!/bin/bash
if [ "$1 $2" = "mod download" ]; then
  n=$(( $(cat "$FAKE_GO_COUNT") + 1 )); echo "$n" >"$FAKE_GO_COUNT"
  if [ "$n" -le "${FAKE_GO_FAIL:-0}" ]; then
    echo "${FAKE_GO_ERR:-go: golang.org/x/mod@v0.43.0: Get \"https://proxy.golang.org/golang.org/x/mod/@v/v0.43.0.zip\": net/http: TLS handshake timeout}" >&2
    exit 1
  fi
  exit 0
fi
if [ "$1 $2" = "env GOPROXY" ]; then echo "https://proxy.golang.org,direct"; exit 0; fi
echo "fake go: unexpected args: $*" >&2; exit 99
EOF
chmod +x "$T/bin/go"

# run <fail_n> [stderr line] -> sets OUT, ST, CALLS
run() {
  echo 0 >"$T/count"
  ST=0
  OUT=$(PATH="$T/bin:$PATH" FAKE_GO_COUNT="$T/count" FAKE_GO_FAIL="$1" FAKE_GO_ERR="${2:-}" \
        GO_MOD_DOWNLOAD_BACKOFF="0 0" GO_MOD_DOWNLOAD_TIMEOUT=10 bash "$SUT" 2>&1) || ST=$?
  CALLS=$(cat "$T/count")
}

run 0
if [ "$ST" -eq 0 ] && [ "$CALLS" -eq 1 ] && grep -q 'succeeded on attempt 1/3' <<<"$OUT"; then
  ok "always downloads (one call, no skip branch) and succeeds on attempt 1"
else bad "first-attempt success (st=$ST calls=$CALLS)" "$OUT"; fi

# Default schedule (no GO_MOD_DOWNLOAD_BACKOFF override) is 3 attempts. A
# first-attempt success never sleeps, so this stays fast.
echo 0 >"$T/count"; ST=0
OUT=$(PATH="$T/bin:$PATH" FAKE_GO_COUNT="$T/count" FAKE_GO_FAIL=0 bash "$SUT" 2>&1) || ST=$?
if [ "$ST" -eq 0 ] && grep -q '^go mod download: attempt 1/3 (timeout 120s)$' <<<"$OUT"; then
  ok "default schedule is 3 attempts with a 120s per-attempt timeout"
else bad "default schedule (st=$ST)" "$OUT"; fi

run 2
if [ "$ST" -eq 0 ] && [ "$CALLS" -eq 3 ] && grep -q 'succeeded on attempt 3/3' <<<"$OUT"; then
  ok "fail twice then succeed passes on attempt 3"
else bad "retry to success (st=$ST calls=$CALLS)" "$OUT"; fi

run 3
if [ "$ST" -eq 1 ] && [ "$CALLS" -eq 3 ] \
   && grep -q '^::error::go mod download failed after 3 attempts\..*proxy\.golang\.org.*the Go error above is the real cause' <<<"$OUT" \
   && grep -q 'TLS handshake timeout' <<<"$OUT"; then
  ok "three failures exit 1 with a ::error:: that does not claim the cause, Go's stderr kept, no 4th attempt"
else bad "exhausted retries (st=$ST calls=$CALLS)" "$OUT"; fi

run 3 'verifying golang.org/x/mod@v0.43.0: checksum mismatch'
if [ "$ST" -eq 1 ] && [ "$CALLS" -eq 1 ] && grep -q '^::error::go mod download failed with a non-transient error' <<<"$OUT" \
   && grep -q 'checksum mismatch' <<<"$OUT"; then
  ok "a checksum mismatch is not retried and exits 1 with a ::error::"
else bad "non-transient short-circuit (st=$ST calls=$CALLS)" "$OUT"; fi

run 3 'go: example.com/m@v9.9.9: invalid version: unknown revision v9.9.9'
if [ "$ST" -eq 1 ] && [ "$CALLS" -eq 1 ]; then ok "an unknown revision / invalid version is not retried"
else bad "unknown revision short-circuit (st=$ST calls=$CALLS)" "$OUT"; fi

# Per-attempt timeout: a hung `go mod download` is killed and counted as a failed attempt.
cat >"$T/bin/go" <<'EOF'
#!/bin/bash
if [ "$1 $2" = "mod download" ]; then n=$(( $(cat "$FAKE_GO_COUNT") + 1 )); echo "$n" >"$FAKE_GO_COUNT"; exec sleep 30; fi
[ "$1 $2" = "env GOPROXY" ] && echo "https://proxy.golang.org,direct"
EOF
echo 0 >"$T/count"; ST=0
OUT=$(PATH="$T/bin:$PATH" FAKE_GO_COUNT="$T/count" \
      GO_MOD_DOWNLOAD_BACKOFF="0" GO_MOD_DOWNLOAD_TIMEOUT=1 bash "$SUT" 2>&1) || ST=$?
if [ "$ST" -eq 1 ] && [ "$(cat "$T/count")" -eq 2 ] && grep -q 'timed out after 1s' <<<"$OUT"; then
  ok "a hung attempt is killed by the per-attempt timeout and retried"
else bad "per-attempt timeout (st=$ST)" "$OUT"; fi

# Missing `timeout`: a PATH holding only bash and the fake go (no coreutils).
mkdir -p "$T/notimeout"
ln -s "$(command -v bash)" "$T/notimeout/bash"
ln -s "$T/bin/go" "$T/notimeout/go"
echo 0 >"$T/count"; ST=0
OUT=$(PATH="$T/notimeout" FAKE_GO_COUNT="$T/count" "$T/notimeout/bash" "$SUT" 2>&1) || ST=$?
if [ "$ST" -eq 2 ] && [ "$(cat "$T/count")" -eq 0 ] && grep -q "^::error::.*'timeout' (coreutils) not found" <<<"$OUT"; then
  ok "missing timeout exits 2 loudly without an unbounded download"
else bad "missing timeout (st=$ST)" "$OUT"; fi

# --- workflow wiring ---------------------------------------------------------
cat >"$T/wire.py" <<'PYEOF'
import os, re, sys, yaml

wf_dir = sys.argv[1]
STEP = "Download Go modules (bounded retry)"
RUN = "bash scripts/go-mod-download-retry.sh"
# (workflow, job id) -> a step that runs go out of sight (inside a script) or
# that the placement is pinned against by name; the step must come before it
# AND before the first step whose own `run:` holds a go command. None = only
# the latter.
JOBS = {
    ("ci.yml", "test"): "Resolve shard package list",
    ("ci.yml", "lint"): None,
    ("ci.yml", "build-matrix"): None,
    ("ci.yml", "coverage-upload"): None,
    ("ci.yml", "coverage-floor"): None,
    ("gate.yml", "generated-files"): None,
    ("gate.yml", "provider-failure-smoke"): "Run provider failure smoke test",
    ("gate.yml", "gate-invariant"): None,
    ("bruno-ci.yml", "bruno"): None,
    ("codeql.yml", "analyze"): None,
}
# Cached-Go PR-path jobs deliberately not covered here, with the reason.
EXEMPT = {
    ("ci.yml", "a11y-test"): "owned by the a11y shard branch; check-a11y-shards.sh pins that job",
}
GO_CMD = re.compile(r"(^|[\s;&|(`])go\s+(mod|test|build|run|list|vet|tool|generate|install|env|get)\b", re.M)
GO_ACTIONS = ("golangci/golangci-lint-action@",)

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

fails = []
def uses(step, prefix):
    return str(step.get("uses", "")).startswith(prefix)
def runs_go(step):
    return bool(GO_CMD.search(str(step.get("run", "")))) or any(uses(step, a) for a in GO_ACTIONS)
def first(steps, pred, start=0):
    return next((i for i in range(start, len(steps)) if pred(steps[i])), None)

loaded = {}
def load(name):
    if name not in loaded:
        loaded[name] = yaml.load(open(os.path.join(wf_dir, name)), Loader=UniqueKeyLoader)
    return loaded[name]

for (wfname, jid), first_go_name in JOBS.items():
    tag = f"{wfname}:{jid}"
    job = load(wfname).get("jobs", {}).get(jid)
    if not job:
        fails.append(f"{tag}: job not found"); continue
    steps = job.get("steps") or []
    fetch = [i for i, s in enumerate(steps) if s.get("name") == STEP]
    if len(fetch) != 1:
        fails.append(f"{tag}: expected exactly one '{STEP}' step, found {len(fetch)}"); continue
    fi = fetch[0]; fs = steps[fi]
    if fs.get("run") != RUN:
        fails.append(f"{tag}: '{STEP}' run is {fs.get('run')!r}, expected {RUN!r}")
    if "continue-on-error" in fs:
        fails.append(f"{tag}: '{STEP}' must not set continue-on-error")
    extra = sorted(set(fs) - {"name", "run", "if"})
    if extra:
        fails.append(f"{tag}: '{STEP}' has unexpected keys {extra}")
    co = first(steps, lambda s: uses(s, "actions/checkout@"))
    if co is None or co > fi:
        fails.append(f"{tag}: '{STEP}' runs before (or without) actions/checkout, so the script is absent")
    sg = first(steps, lambda s: uses(s, "actions/setup-go@"))
    if sg is None:
        fails.append(f"{tag}: no actions/setup-go step"); continue
    if fi <= sg:
        fails.append(f"{tag}: '{STEP}' (step {fi}) must come after setup-go (step {sg})")
    # No `if:` of its own: it may only carry exactly the setup-go step's `if:`
    # (a job whose Go setup is path-gated), so it runs whenever Go is set up.
    sg_if = steps[sg].get("if")
    if ("if" in fs) != ("if" in steps[sg]) or fs.get("if") != sg_if:
        fails.append(f"{tag}: '{STEP}' if: {fs.get('if')!r} must equal setup-go's if: {sg_if!r} (absent when absent)")
    if jid == "test" and wfname == "ci.yml":
        rs = first(steps, lambda s: uses(s, "actions/cache/restore@"))
        if rs is None or fi <= rs:
            fails.append(f"{tag}: '{STEP}' must come after the Go cache restore (restore={rs}, fetch={fi})")
    cands = [first(steps, lambda s: s.get("name") != STEP and runs_go(s), sg + 1)]
    if first_go_name is not None:
        named = first(steps, lambda s: s.get("name") == first_go_name)
        if named is None:
            fails.append(f"{tag}: pinned go step {first_go_name!r} not found"); continue
        cands.append(named)
    cands = [c for c in cands if c is not None]
    if not cands:
        fails.append(f"{tag}: no go-running step after setup-go (job list is stale?)"); continue
    go_i = min(cands)
    if fi >= go_i:
        fails.append(f"{tag}: '{STEP}' (step {fi}) must come before the first go-running step {steps[go_i].get('name')!r} (step {go_i})")

# Completeness: every PR-path job with a cached setup-go that runs go is listed.
for fn in sorted(os.listdir(wf_dir)):
    if not fn.endswith((".yml", ".yaml")):
        continue
    wf = load(fn)
    on = wf.get("on", wf.get(True)) or {}
    if not (on == "pull_request" or (isinstance(on, (list, dict)) and "pull_request" in on)):
        continue
    for jid, job in (wf.get("jobs") or {}).items():
        steps = job.get("steps") or []
        sg = first(steps, lambda s: uses(s, "actions/setup-go@"))
        if sg is None or str((steps[sg].get("with") or {}).get("cache", True)).lower() == "false":
            continue
        if not any(runs_go(s) for s in steps[sg + 1:]) and (fn, jid) not in JOBS:
            continue
        if (fn, jid) not in JOBS and (fn, jid) not in EXEMPT:
            fails.append(f"{fn}:{jid}: runs go after a cached setup-go on the PR path but has no pinned '{STEP}' step")

if fails:
    print("\n".join(fails)); sys.exit(1)
print(f"{len(JOBS)} jobs wired; completeness scan found no unlisted cached Go job")
PYEOF
WIRE_ST=0
WIRE_OUT=$(python3 "$T/wire.py" "$WF" 2>&1) || WIRE_ST=$?
if [ "$WIRE_ST" -eq 0 ]; then
  ok "workflow wiring: one 'Download Go modules (bounded retry)' step per Go job, after setup-go, before the first go command ($WIRE_OUT)"
else bad "workflow wiring" "$WIRE_OUT"; fi

[ "$rc" -eq 0 ] && echo "All go-mod-download-retry tests passed." || echo "go-mod-download-retry tests FAILED."
exit "$rc"
