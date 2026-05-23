#!/usr/bin/env bash
#
# safe-push.sh -- run `git push` and verify the remote actually received it.
#
# The pre-push gate is well-isolated for parallel agents (per-worktree run
# dirs, per-worktree gate lock, golangci-lint allow-parallel-runners). What
# is NOT robust is the common invocation pattern:
#
#   git push -u origin <branch> 2>&1 | tail -30
#
# Without `set -o pipefail`, the pipeline returns tail's exit code, which
# is virtually always 0 -- masking a real git push failure (transient SSH
# blip, remote ref rejection, hook abort, network drop). The visible output
# then looks identical to a quiet success, and the agent moves on to open a
# PR against a branch that never reached the remote.
#
# This wrapper:
#   1. Resolves the local HEAD and the branch (or current symbolic-ref).
#   2. Runs `git push` with full output captured to <git-dir>/safe-push.log
#      and mirrored to stderr so the caller can stream it.
#   3. After push returns, queries `git ls-remote origin <branch>` and
#      verifies the remote SHA matches local HEAD.
#   4. Exits non-zero with a clear message if push's exit code OR the post-push
#      ref check disagrees.
#
# Usage:
#   bash scripts/safe-push.sh                  # push current branch -u origin
#   bash scripts/safe-push.sh <branch>         # push named branch -u origin
#   bash scripts/safe-push.sh <branch> --force # forwarded to git push
#
# Exit codes:
#   0 -- push succeeded AND the remote ref matches local HEAD
#   1 -- push exited non-zero, or the remote ref does not match local HEAD
#   2 -- invalid invocation / cannot resolve branch or worktree

set -euo pipefail

# Repo-agnostic log location. `git rev-parse --git-dir` resolves correctly
# for the main worktree (.git), linked worktrees (.git/worktrees/<name>),
# and submodules. Previously sourced lib/run-paths.sh for $SW_RUN_DIR --
# dropped so this wrapper has no project-specific dependency and can be
# kept in sync with the repo-agnostic gist copy at
# ~/.claude/scripts/safe-push.sh.
git_dir=$(git rev-parse --git-dir 2>/dev/null || true)
if [ -z "$git_dir" ]; then
  echo "safe-push: not inside a git repository" >&2
  exit 2
fi
LOG="$git_dir/safe-push.log"

branch="${1:-}"
shift_count=0
if [ -n "$branch" ] && [ "${branch#-}" = "$branch" ]; then
  shift_count=1
else
  branch=""
fi

if [ -z "$branch" ]; then
  branch=$(git symbolic-ref --quiet --short HEAD 2>/dev/null || true)
  if [ -z "$branch" ]; then
    echo "safe-push: HEAD is detached and no branch argument given" >&2
    exit 2
  fi
fi

# Drop the consumed positional so the remaining "$@" can flow into git push
# as extra flags (--force-with-lease, --no-verify, etc.). Quoted so flags with
# spaces survive intact.
if [ "$shift_count" -gt 0 ]; then
  shift
fi

local_sha=$(git rev-parse --verify "refs/heads/$branch" 2>/dev/null || true)
if [ -z "$local_sha" ]; then
  echo "safe-push: local branch 'refs/heads/$branch' does not exist" >&2
  exit 2
fi

# Capture full output to a log AND mirror to stderr. `tee` writes to both;
# `set -o pipefail` ensures git push's non-zero exit propagates through the
# pipeline rather than being hidden by tee's exit (which is the bug this
# wrapper exists to prevent).
echo "safe-push: pushing $branch ($local_sha) to origin" >&2
push_status=0
set -o pipefail
# Capture git push's real exit code. `if ! cmd; then` would set $? to the
# negated value (0) inside the then-block, masking the actual failure --
# the exact silent-failure mode this wrapper exists to prevent.
if git push -u origin "$branch" "$@" 2>&1 | tee "$LOG" >&2; then
  push_status=0
else
  push_status=$?
fi
set +o pipefail

# Independent verification: read the remote ref directly. ls-remote bypasses
# any local cache (no `git fetch` needed) and returns the authoritative SHA
# from GitHub. A successful push that somehow didn't update the ref (the
# silent-failure mode this wrapper guards against) will show here.
remote_line=$(git ls-remote origin "refs/heads/$branch" 2>/dev/null || true)
remote_sha=${remote_line%%$'\t'*}

if [ "$push_status" -ne 0 ]; then
  echo "safe-push: git push exited $push_status -- see $LOG" >&2
  exit 1
fi

if [ -z "$remote_sha" ]; then
  echo "safe-push: git push exited 0 but origin has no '$branch' ref" >&2
  echo "          local HEAD: $local_sha" >&2
  echo "          full log:   $LOG" >&2
  exit 1
fi

if [ "$remote_sha" != "$local_sha" ]; then
  echo "safe-push: git push exited 0 but origin/'$branch' does not match local HEAD" >&2
  echo "          local:  $local_sha" >&2
  echo "          remote: $remote_sha" >&2
  echo "          full log: $LOG" >&2
  exit 1
fi

echo "safe-push: verified origin/$branch -> $remote_sha" >&2
exit 0
