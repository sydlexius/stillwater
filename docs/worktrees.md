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

Preferred path is `make worktree`, which creates the worktree, runs `make hooks` inside it, and inserts a row into the Active table in `memory/worktrees.md` automatically:

```bash
# Single issue:
make worktree NAME=315 BRANCH=feat/315-musicbrainz-mirror ISSUE=315

# Milestone sub-issue (with wave label):
make worktree NAME=m17-320 BRANCH=feat/320-short-desc ISSUE=320 WAVE="M17 W1"
```

`NAME` is the suffix after `stillwater-`. `BRANCH` is required. `ISSUE` and `WAVE` are optional; both default to `--` in the tracker row.

For cases the Makefile target does not cover (branching off something other than the current `HEAD`, for example a milestone umbrella branch), fall back to raw `git worktree add` and then update the Active table by hand:

```bash
git worktree add -b feat/320-short-desc ../stillwater-m17-320 feat/m17-umbrella
make -C ../stillwater-m17-320 hooks
# then manually add the row to the Active table in memory/worktrees.md
# row format: | stillwater-<NAME> | <BRANCH> | #<ISSUE> | <WAVE> | In progress |
```

## Tracking

Active worktrees are tracked in the `## Active` table at the top of `memory/worktrees.md` inside `~/.claude/projects/<project>/memory/`. `make worktree` inserts the row on create and `make remove-worktree` strips it on cleanup, so the table stays current automatically for worktrees managed through those targets. Manual edits are only needed for the fallback `git worktree add` path described above.

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

Preferred path is `make remove-worktree`, which delegates to `cleanup-worktree.sh` (removes worktree + branches + caches, prunes refs) and then strips the matching row from the Active table in `memory/worktrees.md`:

```bash
make remove-worktree NAME=1180          # single-issue
make remove-worktree NAME=m36-639       # milestone sub-issue
make remove-worktree NAME=fanart-dup    # slug
```

`NAME` is whatever follows `stillwater-` in the worktree directory name (same value passed to `make worktree`).

For repos other than Stillwater, or for invocations from outside the main checkout, the underlying script can be called directly. It is repo-agnostic (detects the repo prefix from the current main worktree's basename), but it does not know about Stillwater's tracker file, so the Active table row must be removed by hand:

```bash
bash $HOME/.claude/scripts/cleanup-worktree.sh <suffix>
# then manually delete the matching row from memory/worktrees.md
```
