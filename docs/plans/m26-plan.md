# Milestone 26 -- UX & Maintenance

## Goal

Cross-cutting UX improvements and maintenance tasks: fix the scan spinner bug,
clarify the auto-fetch setting, hot-reload artist detail after metadata refresh,
make Run Rules async, add backup management UI, and build a contextualized help overlay.

## Acceptance Criteria

- [ ] Scan Library spinner appears during scanning and disappears when complete
- [ ] Auto-fetch images setting description clearly communicates manual-only scope
- [ ] Refresh Metadata updates the artist detail page without a manual reload
- [ ] Run Rules button is fully async with polling progress and toast feedback
- [ ] Backups can be deleted from the Settings UI with confirmation
- [ ] Backup retention count and max age are configurable in Settings
- [ ] `?` key and nav button open a searchable help overlay with page-context filtering

## Dependency Map

All six issues are independent. No inter-issue dependencies.

```
#234 (spinner bug)       -- independent
#235 (auto-fetch label)  -- independent
#236 (refresh hot-reload)-- independent
#237 (help overlay)      -- independent
#238 (backup management) -- independent
#291 (async run rules)   -- independent
```

## PR Strategy

4 PRs, not stacked (all based on main):

| PR | Issues | Branch | Scope |
|----|--------|--------|-------|
| 1 | #234, #235, #236 | `fix/m26-ux-small-fixes` | Small UX fixes |
| 2 | #238 | `feat/m26-backup-management` | Backup management |
| 3 | #291 | `fix/m26-async-run-rules` | Async Run Rules |
| 4 | #237 | `feat/m26-help-overlay` | Help overlay |

## Checklist

### PR 1 -- Small UX Fixes (#234, #235, #236)

#### Issue #234 -- Bug: Scan Library spinner not working
- [x] Implementation
- [x] PR opened (#293)
- [ ] CI passing
- [ ] PR merged

#### Issue #235 -- UX: Clarify auto-fetch images setting description
- [x] Implementation
- [x] PR opened (#293)
- [ ] CI passing
- [ ] PR merged

#### Issue #236 -- UX: Refresh Metadata should hot-reload artist detail page
- [x] Implementation
- [x] PR opened (#293)
- [ ] CI passing
- [ ] PR merged

#### Copilot review fixes (round 1)
- [x] Fix duplicate OOB IDs in RefreshOOBFragments (selector-based syntax)
- [x] Fix unchecked render errors in renderRefreshWithOOB

#### Copilot review fixes (round 2)
- [x] Skip OOB fragments when ListMembersByArtistID fails
- [x] Add handler test for HTMX refresh OOB swap targets
- [ ] OOB wrapper nesting on repeated refreshes (tracked as follow-up)

### PR 2 -- Backup Management (#238)
- [x] Implementation
- [x] Tests added
- [x] PR opened (#294)
- [ ] CI passing
- [ ] PR merged

#### Copilot review fixes (round 1)
- [x] NaN validation in saveBackupRetention() JS
- [x] Load DB-persisted backup settings on startup
- [x] Data race fix (sync.RWMutex on retention/maxAgeDays)
- [x] Return 404 for missing backup file in DELETE handler
- [x] Validate backup settings before persisting in handleUpdateSettings
- [x] Add DELETE handler tests

#### Copilot review fixes (round 2)
- [x] Use lock-guarded Retention() getter in StartScheduler

#### Copilot review fixes (round 3)
- [x] Guard against negative retention in SetRetention (clamp to 0)

#### Copilot review fixes (round 4)
- [x] Clamp negative values to 0 in SetMaxAgeDays() for consistency
- [x] Update backup helper text to mention both count and age pruning
- [x] Rename saveBackupRetention() to saveBackupSettings()

### PR 3 -- Async Run Rules (#291)
- [x] Implementation
- [x] PR opened (#295)
- [ ] CI passing
- [ ] PR merged

#### Copilot review fixes (round 1)
- [x] Data race fix in handleRunAllRulesStatus (value copy under lock)
- [x] context.Background() to context.WithoutCancel
- [x] pollRuleStatus() idle/error handling

#### Copilot review fixes (round 2)
- [x] Panic recovery in background goroutine
- [x] Poll timeout safeguard (150 attempts / ~5 min)
- [x] Toast message in fetch .catch() handler

#### Copilot review fixes (round 3)
- [x] Stop polling on non-OK HTTP response (clearInterval + resetRuleButton)

#### Copilot review fixes (round 4)
- [x] Release mutex before logging in error path (match panic recovery pattern)

### PR 4 -- Help Overlay (#237)
- [x] Implementation
- [x] PR opened (#296)
- [ ] CI passing
- [ ] PR merged

#### Copilot review fixes (round 2)
- [x] Fix /artists vs /artists/ path matching in isPageMatch()
- [x] Add aria-label to close button
- [x] Add aria-label to search input

#### Copilot review fixes (round 3)
- [x] Use aria-labelledby pointing at heading ID for modal consistency

#### Copilot review fixes (round 4)
- [x] Use encodeURIComponent for fragment ID in help overlay href

## UAT / Merge Order

1. PR #293 (smallest, most confidence)
2. PR #294 and PR #295 (any order)
3. PR #296 last (largest)

## Notes

- #234: CSS conflict between `htmx-indicator` class and custom JS opacity manipulation
- #235: Single text change
- #236: OOB swap pattern for in-place section updates after refresh
- #237: Largest scope; client-side search over structured guide sections
- #238: `IsValidBackupFilename()` already exists for delete endpoint validation
- #291: Follow scanner polling pattern with in-memory state tracking on Router
- 2026-02-27: All 4 PRs pushed with Copilot review feedback addressed; PR #296 opened for help overlay
- 2026-02-27: Round 2 Copilot review fixes pushed to all 4 PRs (9 actionable items addressed, 2 skipped as out-of-scope)
- 2026-02-27: Round 3 Copilot review fixes pushed to PRs #294, #295, #296 (3 actionable items; PR #293 had no new actionable items)
- 2026-02-27: Round 4 Copilot review fixes pushed to PRs #294, #295, #296 (5 actionable items across 3 PRs)
