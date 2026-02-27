# Milestone 26 -- UX & Maintenance

## Goal

Cross-cutting UX improvements and maintenance tasks: fix the scan spinner bug,
clarify the auto-fetch setting, hot-reload artist detail after metadata refresh,
add a contextualized help overlay, and build backup management UI.

## Acceptance Criteria

- [ ] Scan Library spinner appears during scanning and disappears when complete
- [ ] Auto-fetch images setting description clearly communicates manual-only scope
- [ ] Refresh Metadata updates the artist detail page without a manual reload
- [ ] `?` key and nav button open a searchable help overlay with page-context filtering
- [ ] Backups can be deleted from the Settings UI with confirmation
- [ ] Backup retention count and max age are configurable in Settings

## Dependency Map

All five issues are independent. No inter-issue dependencies.

```
#234 (spinner bug)       -- independent
#235 (auto-fetch label)  -- independent
#236 (refresh hot-reload)-- independent
#237 (help overlay)      -- independent
#238 (backup management) -- independent
```

## Checklist

### Issue #234 -- Bug: Scan Library spinner not working on Artists page
- [ ] Debug spinner lifecycle (CSS indicator vs JS opacity conflict)
- [ ] Fix root cause in `artists.templ` (lines 64-225)
- [ ] Verify scanner status endpoint returns expected values
- [ ] PR opened (#?)
- [ ] CI passing
- [ ] PR merged

### Issue #235 -- UX: Clarify auto-fetch images setting description
- [ ] Update description text in `settings.templ` (line 451)
- [ ] PR opened (#?)
- [ ] CI passing
- [ ] PR merged

### Issue #236 -- UX: Refresh Metadata should hot-reload artist detail page
- [ ] Add `hx-swap-oob` fragments to `handleArtistRefresh()` response
- [ ] Update `artist_detail.templ` sections with swap target IDs
- [ ] Remove manual reload button from `artist_refresh.templ`
- [ ] PR opened (#?)
- [ ] CI passing
- [ ] PR merged

### Issue #237 -- Feature: Contextualized help system (searchable overlay)
- [ ] Index `guide.templ` sections into client-side searchable JSON
- [ ] Build search overlay modal component in `layout.templ`
- [ ] `?` keyboard shortcut and nav bar help button to trigger overlay
- [ ] Page-context filtering (current page prioritizes relevant sections)
- [ ] Direct links to full guide sections
- [ ] New JS in `web/static/js/`
- [ ] PR opened (#?)
- [ ] CI passing
- [ ] PR merged

### Issue #238 -- Feature: Database backup management in Settings UI
- [ ] Add `Delete()` method to `backup.Service`
- [ ] Add `DELETE /api/v1/settings/backup/{filename}` route
- [ ] Retention count + max age settings (DB settings with env var fallback)
- [ ] Delete button with confirmation in backup list table (`settings.templ`)
- [ ] Update `backup.Service.Prune()` for max age setting
- [ ] PR opened (#?)
- [ ] CI passing
- [ ] PR merged

## UAT / Merge Order

Session 1 (quick wins):
1. PR for #234 (base: main)
2. PR for #235 (base: main) -- may combine with #234
3. PR for #236 (base: main)

Session 2:
4. PR for #237 (base: main)

Session 3:
5. PR for #238 (base: main)

## Notes

- #234: Likely CSS conflict between `htmx-indicator` class and custom JS opacity manipulation
- #235: Single text change, scope: small, use Haiku
- #236: OOB swap pattern -- include updated fragments alongside the refresh result summary
- #237: Largest scope issue; plan mode recommended before implementation
- #238: `IsValidBackupFilename()` already exists for security validation on the delete endpoint
