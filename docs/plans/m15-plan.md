# Milestone 15 -- Code Health & Audits

## Goal

Fix high-severity runtime safety issues (memory leaks, race conditions, deadlocks) and reduce
the highest-impact code duplication (connection client boilerplate, handler parameter extraction,
request body decoding). Both issues are audit-driven: findings were documented first, then the
highest-severity/highest-impact items selected for implementation in this milestone.

## Acceptance Criteria

- [ ] All high-severity runtime safety issues fixed (watcher races, scanner leak, etc.)
- [ ] `go test -race ./...` passes after #351 fixes
- [ ] Shared BaseClient extracted for emby/jellyfin/lidarr connection clients
- [ ] RequirePathParam helper extracted and applied across handler files
- [ ] DecodeJSON helper extracted and applied across handler files
- [ ] All tests pass after #352 refactoring

## Dependency Map

#351 and #352 are independent -- can be worked in parallel or any order.

## Checklist

### Issue #351 -- Audit codebase for memory leaks, race conditions, deadlocks, and dead code

`[mode: direct] [model: opus]`

- [ ] Fix watcher map race condition (watcher.go:395-474)
- [ ] Fix watcher deadlock risk (watcher.go:248-308)
- [ ] Fix scanner goroutine leak (scanner.go:102 -- context.WithoutCancel)
- [ ] Fix HTTP response body not drained on non-200 (image/processor.go:30-42)
- [ ] Fix webhook dispatcher semaphore not recovered on panic (webhook/dispatcher.go:50-68)
- [ ] Fix event bus silent event drops (event/bus.go:67-76)
- [ ] Fix router state accessed with inconsistent locking (api/router.go:98-101)
- [ ] Document medium-severity findings as follow-up issues
- [ ] `go test -race ./...` passes
- [ ] PR merged

### Issue #352 -- Audit and improve code reuse and maintainability (Phase 1)

`[mode: plan] [model: sonnet]`

- [ ] Create `internal/connection/httpclient/` package with BaseClient and tests
- [ ] Migrate emby/jellyfin/lidarr clients to embed BaseClient
- [ ] Create `internal/api/params.go` with RequirePathParam and DecodeJSON helpers
- [ ] Update handler files to use RequirePathParam (~27 sites)
- [ ] Update handler files to use DecodeJSON (~13 sites, pure-JSON handlers only)
- [ ] `go test -race ./...` passes
- [ ] Docker UAT clean
- [ ] PR merged

## Worktrees

| Directory              | Branch                    | Issue | Status  |
|------------------------|---------------------------|-------|---------|
| stillwater-352         | feat/352-phase1-refactor  | #352  | pending |

## UAT / Merge Order

1. #351 (independent -- Opus, direct mode)
2. #352 (independent -- Sonnet, plan mode)

## Cleanup

After the last PR is merged (whichever of #351 or #352 merges last):

1. Delete this plan file: `git rm docs/plans/m15-plan.md` and commit to `main`
2. Remove all worktrees: `git worktree list` then `git worktree remove` for each m15 worktree
3. Delete merged feature branches (remote + local)
4. Run `git fetch --prune`
5. Update `memory/worktrees.md`
6. Post summary comment to each issue and close them
