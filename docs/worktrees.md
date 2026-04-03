# Worktree Protocol

## Naming convention

```
../stillwater/              # main repo, main branch (coordination only)
../stillwater-{issue}/      # single-issue worktree
../stillwater-m{N}/         # milestone umbrella worktree
../stillwater-m{N}-{issue}/ # milestone sub-issue worktree
```

Branch naming:
- Features: `feat/{issue}-short-desc`
- Fixes: `fix/{issue}-short-desc`
- Milestone umbrella: `feat/m{N}-umbrella`

## Creating a worktree

```bash
# Single issue:
git worktree add -b feat/315-musicbrainz-mirror ../stillwater-315 main

# Milestone sub-issue (branching from umbrella):
git worktree add -b feat/320-short-desc ../stillwater-m17-320 feat/m17-umbrella
```

## Tracking

Active worktrees are tracked in `memory/worktrees.md` inside `~/.claude/projects/<project>/memory/`. Update it whenever a worktree is created or removed.

## Docker UAT in worktrees

`setupdocker.sh` lives in the main repo root only. To run UAT from a worktree, copy it in or run from main repo.

## Parallel rule PRs

Multiple rule PRs conflict on merge (all modify `engine.go`, `service.go`, `checkers.go`, `engine_test.go`). Merge sequentially; the second PR needs a rebase. Engine tests use relative assertions so new rules do not break existing tests -- verify the rebase did not drop code.

## Cleanup after merge

```bash
bash scripts/cleanup-worktree.sh <issue>
```

This removes the worktree, deletes local and remote branches, and prunes stale refs. Then update `memory/worktrees.md`.
