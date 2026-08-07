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

Preferred path is `make worktree`, which creates the worktree, runs `make hooks` inside it, links the agent permission settings (see below), and inserts a row into the Active table in `memory/worktrees.md` automatically:

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
./scripts/link-worktree-settings.sh ../stillwater-m17-320
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

## Agent permission settings per worktree

Claude Code reads project-local permission grants from `<repo>/.claude/settings.local.json`.
That path is gitignored, so a worktree checkout never receives one and an agent working there
falls back to the much smaller user-global rule set.

The symptom is easy to misread: a permission prompt for a command the repository demonstrably
already grants. A backgrounded agent cannot answer a prompt, so it stalls indefinitely with no
error in any log. Five agents were lost to this overnight before the cause was found (#2879),
and the first diagnosis blamed the permission matcher and proposed a new blanket allow-rule --
which would have granted `gh pr merge` and `rm -rf` as a side effect. The bug was propagation,
not matching.

`make worktree` now runs `scripts/link-worktree-settings.sh`, which symlinks the new worktree's
`.claude/settings.local.json` at the main repo's copy. A symlink rather than a copy so there is
one source of truth: a grant added later in the main repo reaches every existing worktree with
no further action, and no worktree accumulates its own unaudited grants from an "always allow"
click.

For a worktree created another way, run it directly from the main repo:

```bash
./scripts/link-worktree-settings.sh ../stillwater-<slug>
```

It is idempotent, and it **refuses** rather than overwriting if the destination is a real file
rather than a symlink -- that file may hold grants that diverged, and deleting it silently would
destroy the only evidence. Inspect and remove it by hand if the shared file is what you want.

A refusal is fatal to `make worktree`, deliberately: proceeding would leave an agent running on
the wrong grants, which is the failure this whole mechanism exists to prevent. The link step runs
last, after the tracker row is inserted, so a refusal leaves a complete, tracked worktree that is
merely missing its link -- re-run the script above once the file is resolved.

The link is relative whenever the two trees share a parent directory anywhere above them -- not
just the sibling layout `make worktree` produces -- so moving or renaming the tree does not break
it. The one exception is a worktree and main repo that share nothing above `/`, where the target
is absolute and a move would leave it dangling; `make worktree` cannot produce that layout.

If the main repo has no `.claude/settings.local.json` at all (a fresh clone that has never granted
anything), the script reports `SKIP` and exits successfully. That is not a failure: `make worktree`
completes normally and the worktree simply uses the user-global grants.

Verify from inside the worktree with `python3 ~/.claude/scripts/orchestrate-setup.py doctor`,
which reports whether any settings-cascade rule shadows the `gh pr merge` gate.

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
