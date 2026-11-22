#!/bin/bash
# go-mod-download-retry.sh -- fetch Go modules with a bounded retry before any
# go command that would otherwise fetch them itself, with no retry (#3619).
#
# Used by every CI job that runs go on the PR path, right after its setup-go
# (and, in Test Shard, after the read-only Go cache restore). On a
# dependency-bump PR the Go cache those jobs restore does not hold the bumped
# module (setup-go restores by an exact go.sum-keyed key only, so it misses
# and the job cold-downloads the whole module set; Test Shard falls back to
# main's cache and lacks the bumped module). Without this step that fetch
# happens inside `go test` / `go build` / `go tool`, once per job, with no
# retry: one TLS handshake timeout from proxy.golang.org failed #3619's rule-2
# shard before any test ran.
#
# It always runs: on a complete module cache `go mod download` needs no network
# and takes ~0.02s, so there is nothing to gain from a skip branch.
#
# Inputs (env):
#   GO_MOD_DOWNLOAD_TIMEOUT  per-attempt timeout in seconds (default 120);
#                            must be positive, since `timeout 0` disables the
#                            deadline
#   GO_MOD_DOWNLOAD_BACKOFF  space-separated sleeps between attempts
#                            (default "5 15" -> 3 attempts)
#
# Each attempt is `timeout <s> go mod download` (`gtimeout` when only
# Homebrew coreutils' prefixed name exists, as on macOS). A failure whose
# stderr shows a deterministic cause (checksum mismatch / SECURITY ERROR,
# unknown revision) is not retried: it would fail the same way again. Go's
# `invalid version:` prefix alone is NOT treated as deterministic: Go also
# wraps transient VCS fetch failures in it (`invalid version: git ls-remote
# ... exit status 128` after a DNS or TLS error). When every attempt fails it
# exits 1 with a ::error:: annotation; there is no silent fall-through to the
# next go command. The annotation never prints GOPROXY, whose value may be a
# private proxy URL carrying credentials.
#
# Exit status: 0 = downloaded (or already complete); 1 = download failed;
# 2 = bad invocation (non-numeric or zero setting, no `timeout`/`gtimeout`).
# Self-test: scripts/test-go-mod-download-retry.sh.
set -euo pipefail

per_attempt="${GO_MOD_DOWNLOAD_TIMEOUT:-120}"
backoff="${GO_MOD_DOWNLOAD_BACKOFF-5 15}"

case "$per_attempt" in
  ''|*[!0-9]*) echo "::error::go-mod-download-retry: GO_MOD_DOWNLOAD_TIMEOUT must be a whole number of seconds, got '$per_attempt'" >&2; exit 2 ;;
esac
# `timeout 0` runs the command with NO deadline, so zero (in any spelling,
# e.g. 00) would silently turn the bound off.
if [[ "$per_attempt" =~ ^0+$ ]]; then
  echo "::error::go-mod-download-retry: GO_MOD_DOWNLOAD_TIMEOUT must be a positive number of seconds, got '$per_attempt' (timeout 0 disables the deadline)" >&2
  exit 2
fi
for s in $backoff; do
  case "$s" in
    *[!0-9]*) echo "::error::go-mod-download-retry: GO_MOD_DOWNLOAD_BACKOFF must be whole seconds, got '$backoff'" >&2; exit 2 ;;
  esac
done

if command -v timeout >/dev/null 2>&1; then
  timeout_cmd=timeout
elif command -v gtimeout >/dev/null 2>&1; then
  timeout_cmd=gtimeout
else
  echo "::error::go-mod-download-retry: neither 'timeout' nor 'gtimeout' (coreutils) found; cannot bound go mod download" >&2
  exit 2
fi

errf="$(mktemp)"
trap 'rm -f "$errf"' EXIT

# shellcheck disable=SC2206  # deliberate word-splitting of the backoff list
sleeps=($backoff)
attempts=$(( ${#sleeps[@]} + 1 ))
for (( i = 1; i <= attempts; i++ )); do
  echo "go mod download: attempt $i/$attempts (timeout ${per_attempt}s)"
  st=0
  "$timeout_cmd" "$per_attempt" go mod download 2>"$errf" || st=$?
  cat "$errf" >&2
  if [ "$st" -eq 0 ]; then
    echo "OK: go mod download succeeded on attempt $i/$attempts"
    exit 0
  fi
  if [ "$st" -eq 124 ]; then
    echo "go mod download: attempt $i timed out after ${per_attempt}s" >&2
  else
    echo "go mod download: attempt $i failed (exit $st)" >&2
  fi
  if grep -qE 'SECURITY ERROR|checksum mismatch|unknown revision' "$errf"; then
    echo "::error::go mod download failed with a non-transient error (checksum mismatch or unknown revision); not retrying, a re-run will fail the same way. The Go error above is the cause." >&2
    exit 1
  fi
  if [ "$i" -lt "$attempts" ]; then
    wait_s="${sleeps[$(( i - 1 ))]}"
    echo "retrying in ${wait_s}s"
    sleep "$wait_s"
  fi
done

echo "::error::go mod download failed after $attempts attempts. A network or proxy fault (e.g. proxy.golang.org or the configured GOPROXY) is the usual cause and a re-run of the job usually clears it, but a checksum mismatch or a missing version will fail the same way again: the Go error above is the real cause." >&2
exit 1
