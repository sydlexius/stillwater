#!/usr/bin/env bash
# scripts/lib/run-paths.sh
#
# SHIM. The run-path convention now has a SINGLE upstream producer:
# ~/.claude/scripts/run-paths.sh (cc-orchestrator #303). This file exists only to
# keep stillwater's own $SW_RUN_* interface, so every consumer here
# (pre-push-gate.sh, smoke.sh, smoke-version-injection.sh,
# smoke-provider-failure.sh, dev-restart.sh) is unchanged.
#
# WHY A SINGLE PRODUCER. The path contract used to live in a comment, with a
# per-repo producer on one side and a shared consumer (cleanup-worktree.sh) on
# the other. They drifted: the producer wrote <basename>-<sha12> while the
# consumer removed <basename>, so removal never matched and 170+ stale run dirs
# (922M) accumulated. One producer makes that drift unrepresentable rather than
# merely detectable.
#
# WHAT THIS BUYS STILLWATER, beyond the shared contract: the upstream producer
# also exports a PER-WORKTREE GOLANGCI_LINT_CACHE. golangci-lint's default cache
# is machine-global and keyed by path, so it replays findings from worktrees that
# no longer exist -- a clean branch failing its gate with issues in files the diff
# never touched, naming a deleted directory (observed 4x in one session on #3093).
# A per-worktree cache makes that cross-worktree consultation IMPOSSIBLE rather
# than cleaned-up-after: removal timing stops mattering because there is no
# shared keyspace for a dead path to persist in.
#
# PATHS ARE UNCHANGED BY THIS SWAP. Verified before the cutover: both producers
# compute $HOME/.cache/stillwater-run/stillwater-3345cf5ebdbd for this worktree.
# The upstream producer hashes the string git RECORDS for the worktree, not
# `git rev-parse --show-toplevel` (which realpath-resolves and diverges on macOS
# between /tmp and /private/tmp) -- the same string this file used to hash. So no
# artifact directory is orphaned and no migration is needed.
#
# SOURCING CONTRACT (inherited, and the reason this stays a plain source): the
# producer never leaks set -e/-u/pipefail into the caller, never aborts the
# sourcing shell, and degrades every failure to an EMPTY run dir that each
# consumer's `[ -d ]` guard skips. Do not add error handling around it here; that
# would defeat the degradation it is built to provide.
#
# Exports (unchanged interface):
#   SW_RUN_ROOT -- ${XDG_CACHE_HOME:-$HOME/.cache}/stillwater-run
#   SW_RUN_DIR  -- $SW_RUN_ROOT/<worktree-basename>-<sha12>, 0700
# Also exported by the producer, and now honored by the gate:
#   GOLANGCI_LINT_CACHE -- $CC_RUN_DIR/golangci
#
# See ~/.claude/scripts/run-paths.sh for the full rationale, the two resolution
# modes (CC_RUN_WORKTREE set vs unset), and CC_RUN_NO_MKDIR.

_sw_producer="$HOME/.claude/scripts/run-paths.sh"
if [ -r "$_sw_producer" ]; then
  # SC1090: the path is machine-dependent by design (a developer's deployed copy),
  # so it cannot be a constant the linter follows. Guarded by the -r test above,
  # and `|| true` keeps a failing source from aborting a `set -e` caller.
  # shellcheck source=/dev/null
  . "$_sw_producer" || true
fi

# VALIDATE THE OUTCOME, DO NOT TRUST THE CALL. Two distinct ways the producer can
# fail to hand us a usable path, and only the first is an absent file:
#
#   - IT IS NOT INSTALLED. `configure` deploys it to ~/.claude/scripts/ on a
#     developer machine; it is absent on every CI runner, since nothing in
#     .github/workflows installs it. An unguarded source fails under `bash -e`
#     and takes the job down - that broke Provider Failure Smoke on this branch's
#     first push, and release.yml and release-nightly.yml reach this file through
#     smoke-version-injection.sh, so the same gap would have broken a RELEASE.
#
#   - IT IS PRESENT AND DEGRADES. Its documented contract is to degrade every
#     internal failure to an EMPTY CC_RUN_DIR rather than abort the caller. That
#     contract assumes consumers guard with `[ -d ]`. OURS DO NOT: they
#     interpolate the value directly, so an empty one yields LOCK_DIR=/.gate-lock,
#     COVER_OUT=/cover.out and `GOBIN="" go install` - writes at the filesystem
#     root. An empty path must never reach a consumer.
#
# Hence the test below is on the RESULT rather than on the source's exit status:
# a real directory, or we compute it ourselves. Testing the call would catch the
# first failure and sail straight past the second.
if [ -z "${CC_RUN_DIR:-}" ] || [ ! -d "${CC_RUN_DIR:-}" ]; then
  # The pre-shim computation, preserved verbatim in behavior. It agrees with the
  # producer by construction - same root, same <basename>-<sha12> form, same
  # hashed string - so artifacts land in one place either way.
  _sw_root="$(git rev-parse --show-toplevel 2>/dev/null || pwd)"
  _sw_base="$(basename "$_sw_root")"
  if [ -z "$_sw_base" ] || [ "$_sw_base" = "/" ]; then
    echo "scripts/lib/run-paths.sh: cannot derive worktree basename from '$_sw_root'" >&2
    unset _sw_producer _sw_root _sw_base
    # SC2317: the linter statically sees only one of these; the pair is the
    # sourced-vs-executed guard the pre-shim file carried, and both are live.
    # shellcheck disable=SC2317
    return 1 2>/dev/null || exit 1
  fi
  # sha12 of the worktree path, so two checkouts sharing a basename do not collide.
  # shasum on macOS, sha256sum on a Linux runner: the producer probes for both and
  # this must too, or the fallback dies exactly where it is needed most.
  if command -v shasum >/dev/null 2>&1; then
    _sw_id="$(printf '%s' "$_sw_root" | shasum -a 256 | awk '{print substr($1,1,12)}')"
  else
    _sw_id="$(printf '%s' "$_sw_root" | sha256sum | awk '{print substr($1,1,12)}')"
  fi
  CC_RUN_ROOT="${XDG_CACHE_HOME:-$HOME/.cache}/stillwater-run"
  CC_RUN_DIR="$CC_RUN_ROOT/${_sw_base}-${_sw_id}"
  mkdir -p "$CC_RUN_DIR"
  # 0700 before any caller writes secrets (smoke.sh cookie jars, coverage
  # profiles): mkdir -p inherits the caller's umask, typically 0755.
  chmod 700 "$CC_RUN_DIR"
  # The whole point of the shim - set it here too, or CI silently reverts to the
  # machine-global lint cache this change exists to retire.
  GOLANGCI_LINT_CACHE="$CC_RUN_DIR/golangci"
  export GOLANGCI_LINT_CACHE
  unset _sw_root _sw_base _sw_id
fi
unset _sw_producer

SW_RUN_ROOT="$CC_RUN_ROOT"
SW_RUN_DIR="$CC_RUN_DIR"
export SW_RUN_ROOT SW_RUN_DIR
