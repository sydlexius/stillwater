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
- [ ] PR opened (#?)
- [ ] CI passing
- [ ] PR merged

#### Issue #235 -- UX: Clarify auto-fetch images setting description
- [x] Implementation
- [ ] PR opened (#?)
- [ ] CI passing
- [ ] PR merged

#### Issue #236 -- UX: Refresh Metadata should hot-reload artist detail page
- [x] Implementation
- [ ] PR opened (#?)
- [ ] CI passing
- [ ] PR merged

### PR 2 -- Backup Management (#238)
- [ ] Implementation
- [ ] Tests added
- [ ] PR opened (#?)
- [ ] CI passing
- [ ] PR merged

### PR 3 -- Async Run Rules (#291)
- [ ] Implementation
- [ ] PR opened (#?)
- [ ] CI passing
- [ ] PR merged

### PR 4 -- Help Overlay (#237)
- [ ] Implementation
- [ ] PR opened (#?)
- [ ] CI passing
- [ ] PR merged

## UAT / Merge Order

1. PR 1 (smallest, most confidence)
2. PR 2 and PR 3 (any order)
3. PR 4 last (largest)

## Notes

- #234: CSS conflict between `htmx-indicator` class and custom JS opacity manipulation
- #235: Single text change
- #236: OOB swap pattern for in-place section updates after refresh
- #237: Largest scope; client-side search over structured guide sections
- #238: `IsValidBackupFilename()` already exists for delete endpoint validation
- #291: Follow scanner polling pattern with in-memory state tracking on Router
