#!/bin/bash
# a11y-bg-build.sh -- build the a11y harness's Stillwater binary in the
# BACKGROUND, so ci.yml's a11y-test job can overlap the ~30-50s `go build` with
# its browser and apt setup, then collect the result LOUDLY before the server
# starts.
#
#   bash scripts/a11y-bg-build.sh start <state-dir>
#   bash scripts/a11y-bg-build.sh wait  <state-dir>
#
# `start` launches `go build -o ./stillwater ./cmd/stillwater` (same flags as
# the old foreground step) detached from the step's shell and returns at once.
# `wait` blocks until that build exits, prints its full output (module
# downloads and compiler errors alike), and fails unless it exited 0 AND left
# an executable ./stillwater.
#
# State passes between the two steps through files in <state-dir> (CI uses
# $RUNNER_TEMP/a11y-build), never the runner's env files: the a11y shard guard
# (scripts/check-a11y-shards.sh) rejects any a11y-test step that mentions them,
# and a file is all `wait` needs:
#   pid       the build's process-group leader (`start` runs it under set -m)
#   deadline  epoch second after which `wait` kills the build and fails
#   log       the build's combined stdout+stderr
#   status    the build's exit code, written (atomically, via rename) only
#             once it has exited; its absence means "still running" or
#             "killed before it could report"
#
# Fail-closed by construction -- every path but "status is 0 and the binary
# exists" exits non-zero with the log shown:
#   - compile error / module fetch failure: non-zero status -> exit 1.
#   - hung build: the deadline (A11Y_BUILD_TIMEOUT seconds after `start`,
#     default 600) kills the build's whole process group -> exit 1. The CI
#     step's timeout-minutes is the backstop for `wait` itself.
#   - build killed from outside (OOM, signal): process gone, no status -> exit 1.
#   - `wait` without `start`, or a second `start` into the same state dir:
#     exit 2 (a usage error must never read as a finished build).
#   - exit 0 but no ./stillwater: exit 1. `start` deletes any existing binary
#     first, so a stale one can never stand in for a failed build.
#
# GO overrides the go binary (the self-test, scripts/test-a11y-bg-build.sh,
# points it at a fake). Run from the repository root.
set -euo pipefail

usage() { echo "usage: $0 start|wait <state-dir>" >&2; exit 2; }
[ $# -eq 2 ] || usage
cmd=$1 dir=$2
GO=${GO:-go}
budget=${A11Y_BUILD_TIMEOUT:-600}
case "$budget" in ''|*[!0-9]*) echo "a11y-bg-build: A11Y_BUILD_TIMEOUT must be a whole number of seconds, got '$budget'" >&2; exit 2 ;; esac

show_log() {
  echo "----- go build output ($dir/log) -----"
  cat "$dir/log" 2>/dev/null || echo "(no build log)"
  echo "----- end go build output -----"
}

case "$cmd" in
start)
  if [ -e "$dir/pid" ]; then
    echo "a11y-bg-build: a build was already started in $dir; refusing to start a second" >&2
    exit 2
  fi
  mkdir -p "$dir"
  rm -f stillwater "$dir/status" "$dir/status.tmp"
  VERSION=$(git describe --tags --always --dirty 2>/dev/null || echo "0.1.0-dev")
  COMMIT=$(git rev-parse --short HEAD)
  DATE=$(date -u +"%Y-%m-%dT%H:%M:%SZ")
  PKG=github.com/sydlexius/stillwater/internal/version
  echo $(( $(date +%s) + budget )) > "$dir/deadline"
  # set -m gives the background job its own process group (pgid == its pid),
  # so `wait` can kill the go command AND its compiler children on timeout.
  # Every stream is redirected: a background job holding the step's stdout
  # open would keep the step from finishing.
  set -m
  (
    rc=0
    CGO_ENABLED=0 "$GO" build \
      -trimpath \
      -ldflags="-s -w -X ${PKG}.Version=${VERSION} -X ${PKG}.Commit=${COMMIT} -X ${PKG}.Date=${DATE}" \
      -o stillwater ./cmd/stillwater || rc=$?
    echo "$rc" > "$dir/status.tmp"
    mv "$dir/status.tmp" "$dir/status"
  ) </dev/null >"$dir/log" 2>&1 &
  echo "$!" > "$dir/pid"
  echo "a11y-bg-build: started go build in the background (pid $!, budget ${budget}s, log $dir/log)"
  ;;
wait)
  if [ ! -s "$dir/pid" ] || [ ! -s "$dir/deadline" ]; then
    echo "::error::a11y-bg-build: no build was started in $dir (run '$0 start $dir' first)"
    exit 2
  fi
  pid=$(cat "$dir/pid")
  deadline=$(cat "$dir/deadline")
  started=$(date +%s)
  while [ ! -e "$dir/status" ]; do
    if [ "$(date +%s)" -ge "$deadline" ]; then
      echo "::error::a11y-bg-build: go build still running at its deadline (${budget}s after start); killing it"
      kill -TERM -- "-$pid" 2>/dev/null || true
      sleep 5
      kill -KILL -- "-$pid" 2>/dev/null || true
      show_log
      exit 1
    fi
    if ! kill -0 "$pid" 2>/dev/null; then
      # The job writes status before it exits, so re-check once: it may have
      # finished between the loop test and kill -0.
      [ -e "$dir/status" ] && break
      echo "::error::a11y-bg-build: the go build process ($pid) is gone but reported no exit status (killed?)"
      show_log
      exit 1
    fi
    sleep 1
  done
  rc=$(cat "$dir/status")
  echo "a11y-bg-build: build finished with status '$rc' (waited $(( $(date +%s) - started ))s)"
  show_log
  case "$rc" in ''|*[!0-9]*) echo "::error::a11y-bg-build: unreadable build status '$rc'"; exit 1 ;; esac
  if [ "$rc" -ne 0 ]; then
    echo "::error::a11y-bg-build: go build failed (exit $rc); its output is above"
    exit 1
  fi
  if [ ! -x stillwater ]; then
    echo "::error::a11y-bg-build: go build exited 0 but left no executable ./stillwater"
    exit 1
  fi
  ;;
*)
  usage
  ;;
esac
