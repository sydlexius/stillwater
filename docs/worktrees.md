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

## Hook installation per worktree

`git worktree add` does not copy or re-apply hook configuration. Each worktree starts with
whatever `core.hooksPath` value its local config inherits from the shared repository config,
which may be stale or absolute if the original install used an older pattern.

`make worktree` delegates the in-worktree setup to `make hooks` (chmod, unset stale
`core.hooksPath`, set the canonical relative `.githooks` value, then verify with
`scripts/check-hooks.sh`), so worktrees created through that target need no manual step.

For worktrees created another way (a direct `git worktree add`, or one that predates the
delegation), run `make hooks` inside the worktree before the first push. `make doctor`
confirms the wiring without modifying anything.

## Docker UAT in worktrees

`setupdocker.sh` lives in the main repo root only. To run UAT from a worktree, copy it in or run from main repo.

## Parallel rule PRs

Multiple rule PRs conflict on merge (all modify `engine.go`, `service.go`, `checkers.go`, `engine_test.go`). Merge sequentially; the second PR needs a rebase. Engine tests use relative assertions so new rules do not break existing tests -- verify the rebase did not drop code.

## Cleanup after merge

```bash
bash $HOME/.claude/scripts/cleanup-worktree.sh <suffix>
```

`<suffix>` is whatever follows `stillwater-` in the worktree directory name. Examples (one per worktree shape above):

- `1180` for `stillwater-1180` (single-issue)
- `m36` for `stillwater-m36` (milestone umbrella)
- `m36-639` for `stillwater-m36-639` (milestone sub-issue)
- `fanart-dup` for `stillwater-fanart-dup` (slug)

The helper is repo-agnostic: it detects the repo prefix from the current main worktree's basename, so the same script works from any checkout. It removes the worktree, deletes local and remote branches, and prunes stale refs. Then update `memory/worktrees.md`.
