# Milestone Work Protocol

When asked to work on a milestone (e.g. "implement Milestone 14"), follow this process.

## 1. Scope Assessment

Before writing any code:
- Read the umbrella issue and all sub-issues on GitHub.
- Identify the dependency order among sub-issues.
- Note any `[mode:]` and `[model:]` hints in issue bodies.
- Check the current state of `main` and any in-progress branches.
- Check `memory/worktrees.md` for any active worktrees that might overlap.

## 2. Plan File

Create `docs/plans/m<N>-plan.md` (e.g. `docs/plans/m14-plan.md`) before starting. The plan file must include:
- Milestone goal and acceptance criteria (summarised from the umbrella issue)
- Sub-issue dependency map (which issues block which)
- A checklist for every sub-issue: use `- [ ]` for pending, `- [x]` for done
- A notes/observations section for decisions, blockers, and findings discovered during work
- The UAT and merge order (which issues are implemented/merged first, which are stacked)

**Do NOT include PR numbers in plan files.** Referencing PR numbers forces a
commit-then-update cycle every time a PR is created, which wastes time and
resources. Track issues by number only; PR linkage lives in GitHub, not in the
plan file.

Commit the plan file to `main` before opening any feature branches so it survives context resets.

Example structure:

```markdown
# Milestone N -- <Title>

## Goal
<one-paragraph summary>

## Acceptance Criteria
- [ ] criterion one
- [ ] criterion two

## Dependency Map
#X --> #Y --> #Z
#W (parallel)

## Checklist
### Issue #X -- <title>
- [ ] Implementation
- [ ] Tests
- [ ] PR merged

### Issue #Y -- <title>
...

## Worktrees
| Directory              | Branch              | Issue | Status  |
|------------------------|---------------------|-------|---------|
| stillwater-m{N}-{issue}| feat/{issue}-desc   | #X    | pending |

## UAT / Merge Order
1. #X (base: main)
2. #Y (stacked on #X)

## Notes
- <date>: <observation or decision>
```

## 3. During Work

- Create a worktree for each sub-issue before starting code (see "Parallel Work" in CLAUDE.md).
- Update the plan file checklist and worktree table as work progresses.
- Update `memory/worktrees.md` whenever a worktree is created or removed.
- Run `gofmt -d` and `go test ./...` before every commit. Do not push code that fails either.
- Use `docker-compose.uat.yml` for UAT builds whenever the PR/check cycle warrants a container test.
- After addressing PR review feedback, update the relevant checklist items.

## 4. Documentation Updates

When any change touches user-facing behavior, check whether it affects existing documentation and update accordingly.

**When to update docs:**
- The change alters UI layout, navigation, or workflows described in the user guide or wiki
- The change adds a new feature, setting, or page that users need to know about
- The change renames, moves, or removes something that existing docs reference
- The change introduces a concept or behavior that would not be self-evident to a user

**What to update:**
- In-app guide (`web/templates/guide.templ` and `/guide` route) -- once it exists
- GitHub wiki pages -- the wiki is a separate repo (clone alongside main as `../stillwater.wiki/`), push directly to `master`:
  - [Architecture](https://github.com/sydlexius/stillwater/wiki/Architecture) -- subsystem, provider, event type, middleware, or core interface changes
  - [Contributing](https://github.com/sydlexius/stillwater/wiki/Contributing) -- linting rules, pre-commit hooks, test patterns, commit conventions, or PR process changes
  - [Developer Guide](https://github.com/sydlexius/stillwater/wiki/Developer-Guide) -- new top-level package, tech stack change, or modified design principle
  - User-facing wiki pages -- UI, settings, or setup step changes
- OOBE step content if onboarding references the changed behavior
- CLAUDE.md if the change affects architecture, commands, or conventions

**How:**
- Documentation changes ship in the same PR as the code change, not as a follow-up
- In the PR description, note which doc pages were updated and why
- Wiki updates are pushed separately (wiki is a different git repo) but as part of the same PR workflow

**Wiki update checklist (evaluate for every PR):**
- [ ] Does this PR add, remove, or change a provider, event type, or core interface? Update [Architecture](https://github.com/sydlexius/stillwater/wiki/Architecture)
- [ ] Does this PR change linting, hooks, test patterns, or contribution workflow? Update [Contributing](https://github.com/sydlexius/stillwater/wiki/Contributing)
- [ ] Does this PR add a package, change the tech stack, or alter a design principle? Update [Developer Guide](https://github.com/sydlexius/stillwater/wiki/Developer-Guide)
- [ ] Does this PR change user-facing behavior, settings, or setup? Update the relevant user-facing wiki page

**During milestone planning:**
- The plan file should list which wiki/guide pages are affected by each sub-issue
- The milestone checklist should include a `- [ ] Docs updated` item for any issue that touches user-facing behavior

## 5. Cleanup (After All PRs Are Merged)

Once every sub-issue PR is merged to `main`:
1. Post findings comments to all research/analysis issues and close them.
2. Post a summary comment to the umbrella issue and close it.
3. Remove all worktrees: run `git worktree list` then `bash scripts/cleanup-worktree.sh <issue>` for each (or remove manually with `git worktree remove <path>`).
4. Run `git fetch --prune` to remove stale tracking refs.
5. Delete the plan file: `git rm docs/plans/m<N>-plan.md` and commit directly to `main`.
6. Update `memory/worktrees.md` to move entries to "Completed" or remove them.
