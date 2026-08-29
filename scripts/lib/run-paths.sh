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

. "$HOME/.claude/scripts/run-paths.sh"
# SC2153: CC_RUN_ROOT/CC_RUN_DIR are assigned by the producer sourced above. The
# linter cannot follow a source in its file-at-a-time mode (the same limitation
# that put SC1091 in .shellcheckrc). Suppressed HERE rather than repo-wide: these
# two lines are the only place the cross-source assignment is invisible, and reviewdog
# concludes `failure` on a finding of ANY severity, so an info notice would redden
# CI on every PR touching a shell script.
# shellcheck disable=SC2153
SW_RUN_ROOT="$CC_RUN_ROOT"
# shellcheck disable=SC2153
SW_RUN_DIR="$CC_RUN_DIR"
export SW_RUN_ROOT SW_RUN_DIR
