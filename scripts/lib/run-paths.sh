#!/usr/bin/env bash
# scripts/lib/run-paths.sh
#
# Deterministic, per-worktree run-artifact paths. Sourced by pre-push-gate.sh,
# smoke.sh, and dev-restart.sh so every script writes coverage profiles, log
# files, and similar transient artifacts under a single predictable directory.
#
# Why not /tmp:
#   - A stray `rm -f /tmp/stillwater-*` from any agent improvisation can wipe
#     a sibling worktree's in-flight artifacts. Cache-dir paths (defaulting to
#     ~/.cache/stillwater-run/<worktree>) are out of that blast radius.
#   - macOS rotates /tmp aggressively; ~/.cache survives reboots.
#
# Why deterministic (no mktemp XXXXXX):
#   - Same worktree always writes the same path -- trivial to inspect, diff
#     across runs, and clean up (`rm -rf $SW_RUN_DIR`).
#   - Same-worktree concurrent runs WOULD clobber each other, but
#     `feedback_no_parallel_heavy_gates` already bans that; the scope of
#     "multi-agent safe" we want is ACROSS worktrees, which this delivers.
#
# Why source instead of inline:
#   - Single place to evolve the convention. Future scripts get the same
#     behaviour by sourcing this file; reviewers grep one location to find
#     every script that participates.
#
# Exports:
#   SW_RUN_ROOT  -- ${XDG_CACHE_HOME:-$HOME/.cache}/stillwater-run
#   SW_RUN_DIR   -- $SW_RUN_ROOT/<worktree-basename>, mkdir -p applied
#
# Worktree basename:
#   - Uses `git rev-parse --show-toplevel` so a script invoked from a
#     subdirectory still writes to the worktree-root-keyed path.
#   - Falls back to $PWD when not inside a git repo (e.g. one-off invocations
#     against a checkout of the scripts directory). The fallback is not a
#     failure mode -- the scripts are expected to run from a worktree.

# Resolve the worktree root. `git rev-parse` is preferred so cd'ing into a
# subdirectory still yields the worktree root, but fall back to $PWD for the
# rare case where this is sourced outside a git checkout.
sw_worktree_root="$(git rev-parse --show-toplevel 2>/dev/null || pwd)"
sw_worktree_basename="$(basename "$sw_worktree_root")"

# Guard against an empty basename. `basename /` returns "/" which would make
# the run dir `$SW_RUN_ROOT//` and silently merge artifacts from any "root"
# invocation into the same bucket. Surface the misconfiguration loudly.
if [ -z "$sw_worktree_basename" ] || [ "$sw_worktree_basename" = "/" ]; then
  echo "scripts/lib/run-paths.sh: cannot derive worktree basename from '$sw_worktree_root'" >&2
  return 1 2>/dev/null || exit 1
fi

# Append a short hash of the absolute worktree path so two checkouts that
# happen to share a basename (e.g. `stillwater` checked out in two parent
# directories) do not collide on the same SW_RUN_DIR. The hash keeps the
# basename for human readability while making the path component unique.
sw_worktree_id="$(printf '%s' "$sw_worktree_root" | shasum -a 256 | awk '{print substr($1,1,12)}')"

SW_RUN_ROOT="${XDG_CACHE_HOME:-$HOME/.cache}/stillwater-run"
SW_RUN_DIR="$SW_RUN_ROOT/${sw_worktree_basename}-${sw_worktree_id}"
mkdir -p "$SW_RUN_DIR"
# Lock the dir to 0700 before any caller writes secrets (cookie jars from
# smoke.sh, coverage profiles, future log dumps). mkdir -p inherits the
# caller's umask, typically 0755, which would leave these artifacts
# readable by other local users on permissive home directories.
chmod 700 "$SW_RUN_DIR"

unset sw_worktree_root sw_worktree_basename sw_worktree_id

export SW_RUN_ROOT SW_RUN_DIR
