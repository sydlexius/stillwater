#!/bin/bash
# test-a11y-bg-build.sh -- hermetic tests for a11y-bg-build.sh, the background
# build ci.yml's a11y-test job overlaps with its browser/apt setup. Every
# failure class must fail `wait` LOUDLY (non-zero, build output shown), never
# leave a silent missing or stale binary.
# Run: bash scripts/test-a11y-bg-build.sh
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
SUT="$SCRIPT_DIR/a11y-bg-build.sh"
# The pre-push hook exports GIT_DIR; without this the fixture `git init` below
# (and the SUT's git describe) would act on the invoking repository (#3051).
# shellcheck source=scripts/lib/git-clean-env.sh
. "$SCRIPT_DIR/lib/git-clean-env.sh"
git_clean_env_unset
# Every SUT call runs under a watchdog, so a regression that makes `wait` hang
# (a dropped deadline or no-status check) FAILS that case in CASE_LIMIT seconds
# instead of stalling the gate for the SUT's 600s default deadline.
CASE_LIMIT=20
if command -v timeout >/dev/null 2>&1; then TO=timeout
elif command -v gtimeout >/dev/null 2>&1; then TO=gtimeout
else echo "test-a11y-bg-build: needs timeout(1) or gtimeout(1) (GNU coreutils) for its per-case watchdog" >&2; exit 2; fi
wd() { "$TO" -k 5 "$CASE_LIMIT" "$@"; }
TMP="$(mktemp -d)"
# The fake's process groups outlive a watchdog kill of `wait` (start detaches
# them), so the EXIT trap kills every group a case started (state/pid is its
# leader's pid, set -m in the SUT) and every fake go and its children, before
# removing $TMP.
cleanup() {
  local f p
  for f in "$TMP"/*/state/pid "$TMP"/*/go.pid; do
    [ -s "$f" ] || continue
    p=$(cat "$f")
    case "$p" in ''|*[!0-9]*) continue ;; esac
    kill -KILL -- "-$p" 2>/dev/null || true
    pkill -KILL -P "$p" 2>/dev/null || true
    kill -KILL "$p" 2>/dev/null || true
  done
  rm -rf "$TMP"
}
trap cleanup EXIT

pass=0
fail=0
ok() { pass=$((pass + 1)); echo "PASS: $1"; }
bad() { fail=$((fail + 1)); echo "FAIL: $1"; [ -z "${2:-}" ] || printf '%s\n' "$2" | sed 's/^/      /'; }

# A fake `go`: FAKE_GO_MODE picks the behavior. It records its own pid so the
# hang case can prove the kill reached the go process, not just the wrapper.
FAKE="$TMP/fake-go"
cat > "$FAKE" <<'EOF'
#!/bin/bash
echo $$ > "$FAKE_GO_PIDFILE"
out=""
while [ $# -gt 0 ]; do [ "$1" = "-o" ] && out=$2; shift; done
case "$FAKE_GO_MODE" in
ok)     echo "go: downloading example.com/mod v1.0.0"; printf '#!/bin/sh\n' > "$out"; chmod +x "$out" ;;
fail)   echo "cmd/stillwater/main.go:3:1: syntax error: non-declaration statement outside function body" >&2; exit 1 ;;
# hang ignores SIGTERM (exec keeps the ignore), so only the SUT's SIGKILL
# escalation ends it, and sleeps longer than CASE_LIMIT. exec: the recorded pid
# IS the sleep, so neither the test nor the EXIT trap can miss a child of it.
hang)   trap '' TERM; echo "go: downloading example.com/slow v1.0.0"; exec sleep 60 ;;
nobin)  echo "pretend success" ;;
killed) kill -KILL "$PPID"; sleep 2 ;;
esac
EOF
chmod +x "$FAKE"

# repo <name> : a throwaway git checkout to run the build "from the root" in
# (start calls git describe/rev-parse). Signing is off: this repo is scratch.
repo() {
  local d="$TMP/$1"
  mkdir -p "$d"
  git -C "$d" init -q
  git -C "$d" -c user.name=t -c user.email=t@t -c commit.gpgsign=false commit -q --allow-empty -m init
  echo "$d"
}

# run <name> <mode> [env...] : start + wait in a fresh repo; sets OUT and RC.
run() {
  local name=$1 mode=$2; shift 2
  local d; d=$(repo "$name")
  OUT=$(cd "$d" && wd env GO="$FAKE" FAKE_GO_MODE="$mode" FAKE_GO_PIDFILE="$d/go.pid" "$@" bash "$SUT" start "$d/state" 2>&1 \
        && wd env "$@" bash "$SUT" wait "$d/state" 2>&1) && RC=0 || RC=$?
  [ "$RC" -ne 124 ] || OUT="$OUT
(watchdog: killed after ${CASE_LIMIT}s)"
  DIR=$d
}

run ok ok
if [ "$RC" -eq 0 ] && [ -x "$DIR/stillwater" ] && grep -q 'go: downloading example.com/mod' <<<"$OUT"; then
  ok "successful build: wait exits 0, binary present, build output shown"
else bad "successful build (rc=$RC)" "$OUT"; fi

run fail fail
if [ "$RC" -eq 1 ] && grep -q 'syntax error: non-declaration' <<<"$OUT" && grep -q '::error::.*go build failed (exit 1)' <<<"$OUT"; then
  ok "compile error: wait exits 1 and shows the compiler output"
else bad "compile error (rc=$RC)" "$OUT"; fi

# A stale binary from earlier must not stand in for a failed build.
d=$(repo stale); printf '#!/bin/sh\n' > "$d/stillwater"; chmod +x "$d/stillwater"
OUT=$(cd "$d" && wd env GO="$FAKE" FAKE_GO_MODE=fail FAKE_GO_PIDFILE="$d/go.pid" bash "$SUT" start "$d/state" >/dev/null 2>&1 \
      && wd bash "$SUT" wait "$d/state" 2>&1) && RC=0 || RC=$?
if [ "$RC" -eq 1 ] && [ ! -e "$d/stillwater" ]; then
  ok "failed build with a stale ./stillwater present: start removed it, wait exits 1"
else bad "stale binary (rc=$RC)" "$OUT"; fi

t0=$(date +%s)
run hang hang A11Y_BUILD_TIMEOUT=2
elapsed=$(( $(date +%s) - t0 ))
gopid=$(cat "$DIR/go.pid" 2>/dev/null || echo "")
if [ "$RC" -eq 1 ] && grep -q 'still running at its deadline' <<<"$OUT" && grep -q 'example.com/slow' <<<"$OUT" \
   && [ "$elapsed" -lt "$CASE_LIMIT" ] && [ -n "$gopid" ] && ! kill -0 "$gopid" 2>/dev/null; then
  ok "hung build: bounded by A11Y_BUILD_TIMEOUT, go process killed, output shown (${elapsed}s)"
else bad "hung build (rc=$RC, ${elapsed}s, go pid ${gopid:-?} alive=$(kill -0 "${gopid:-0}" 2>/dev/null && echo yes || echo no))" "$OUT"; fi

run nobin nobin
if [ "$RC" -eq 1 ] && grep -q 'exited 0 but left no executable' <<<"$OUT"; then
  ok "exit 0 without a binary: wait exits 1"
else bad "exit 0 without a binary (rc=$RC)" "$OUT"; fi

run killed killed
if [ "$RC" -eq 1 ] && grep -q 'gone but reported no exit status' <<<"$OUT"; then
  ok "build killed from outside (no status written): wait exits 1"
else bad "killed build (rc=$RC)" "$OUT"; fi

d=$(repo nostart)
OUT=$(cd "$d" && wd bash "$SUT" wait "$d/state" 2>&1) && RC=0 || RC=$?
if [ "$RC" -eq 2 ] && grep -q 'no build was started' <<<"$OUT"; then
  ok "wait without start: exit 2"
else bad "wait without start (rc=$RC)" "$OUT"; fi

d=$(repo twice)
OUT=$(cd "$d" && wd env GO="$FAKE" FAKE_GO_MODE=ok FAKE_GO_PIDFILE="$d/go.pid" bash "$SUT" start "$d/state" >/dev/null 2>&1 \
      && wd env GO="$FAKE" FAKE_GO_MODE=ok FAKE_GO_PIDFILE="$d/go.pid" bash "$SUT" start "$d/state" 2>&1) && RC=0 || RC=$?
(cd "$d" && wd bash "$SUT" wait "$d/state" >/dev/null 2>&1) || true
if [ "$RC" -eq 2 ] && grep -q 'already started' <<<"$OUT"; then
  ok "second start into the same state dir: exit 2"
else bad "second start (rc=$RC)" "$OUT"; fi

d=$(repo badtimeout)
OUT=$(cd "$d" && wd env A11Y_BUILD_TIMEOUT=10m GO="$FAKE" bash "$SUT" start "$d/state" 2>&1) && RC=0 || RC=$?
if [ "$RC" -eq 2 ] && [ ! -e "$d/state/pid" ]; then
  ok "non-numeric A11Y_BUILD_TIMEOUT: exit 2, nothing started"
else bad "bad timeout (rc=$RC)" "$OUT"; fi

OUT=$(wd bash "$SUT" build "$TMP/x" 2>&1) && RC=0 || RC=$?
if [ "$RC" -eq 2 ]; then ok "unknown subcommand: exit 2"; else bad "unknown subcommand (rc=$RC)" "$OUT"; fi

echo ""
echo "test-a11y-bg-build: $pass passed, $fail failed"
[ "$fail" -eq 0 ]
