# Milestone 30 -- Metadata UX & Code Health

## Goal

Add metadata change tracking with rollback (history tab), make all 19 metadata fields
editable and always visible, improve test coverage across under-tested packages, add
artist sort controls, consolidate SQL migrations, and audit for runtime safety and
code reuse issues.

## Acceptance Criteria

- [ ] All metadata changes (manual edits, provider fetches, rule engine, scans) are recorded with field, old/new value, and source
- [ ] Image changes are archived before replacement, enabling rollback
- [ ] Artist detail page has a History tab showing a timeline of changes with rollback actions
- [ ] All 19 metadata fields (including name, sort_name, disambiguation, provider IDs) are editable via the field API
- [ ] All fields are always visible on the artist detail page (not hidden when empty)
- [ ] NFO write-back works for newly editable fields
- [ ] Code analysis findings are documented with coverage numbers
- [ ] Test coverage improved for critical packages (especially `internal/api` at 12.5%)
- [ ] Artists page has sort dropdown and order toggle in toolbar (grid and table views)
- [x] SQL migrations consolidated into single baseline file; fresh installs work cleanly
- [ ] High-severity runtime safety issues (races, leaks, deadlocks) fixed
- [x] High-impact code duplication reduced (connection clients, handler params, body decoding)
- [ ] Image caching system with settings UI; "degraded" concept removed

## Dependency Map

```
#321 (history tab)  ----\
                         +--> mostly independent, merge in order below
#322 (universal edit) --/
#323 (code analysis) -------> no runtime changes, merge first
#349 (artist sort) ----------> independent UI feature
#350 (SQL squash) -----------> independent, merge early (foundation)
#351 (runtime safety) -------> independent audit + fixes
#352 (refactoring audit) ----> independent audit + refactoring
#360 (image caching) --------> depends on #352 (shared write path abstraction)
```

Most issues are independent. #322 (universal editing) informs #321 (history) since
history needs to track edits on newly-editable fields. #350 (SQL squash) should merge
early since it touches the migration foundation. #360 (image caching) benefits from
#352 (refactoring) landing first since both touch image write paths and handler structure.
#351 and #352 are audit-style work that can proceed in any order.

## Checklist

### Issue #321 -- Add history tab with full metadata change tracking and rollback
- [ ] Migration: `metadata_changes` table (id, artist_id, field, old_value, new_value, source, created_at)
- [ ] `internal/artist/history.go`: HistoryService with Record() and List() methods
- [ ] Image archiving before replacement (`internal/image/history.go`)
- [ ] Hook into UpdateField/ClearField to record changes
- [ ] Hook into provider fetch, rule engine, and scan paths
- [ ] API endpoints: `GET /api/v1/artists/{id}/history`, `POST /api/v1/artists/{id}/history/{changeID}/rollback`
- [ ] History tab on artist detail page with timeline UI
- [ ] Rollback action per change entry
- [ ] Band member and provider ID change tracking
- [ ] Tests

### Issue #322 -- Make all metadata fields editable/fetchable and always visible
- [ ] Extend `fieldColumnMap` with name, sort_name, disambiguation, musicbrainz_id, audiodb_id, discogs_id, wikidata_id, deezer_id
- [ ] Extend `FieldValueFromArtist` switch for new fields
- [ ] Validation for critical fields (name non-empty, provider ID format checks)
- [ ] NFO write-back for newly editable fields
- [ ] Update artist detail template to always show all fields (not hidden when empty)
- [ ] Provider fetch support for disambiguation (MusicBrainz), provider IDs (respective providers)
- [ ] Tests

### Issue #323 -- Run code analysis, document findings, and improve test coverage
- [ ] Document current coverage numbers per package
- [ ] Identify critical gaps (internal/api at 12.5%, auth/config/database/encryption at 0%)
- [ ] Add API handler tests using existing testRouter pattern
- [ ] Add tests for auth, config, database, encryption packages
- [ ] Improve filesystem and middleware coverage
- [ ] Tests pass with race detector

### Issue #349 -- Add sort controls to Artists page grid and table views
- [ ] Sort dropdown in toolbar (name, sort_name, health_score, updated_at, created_at)
- [ ] Asc/desc toggle button
- [ ] Clickable column headers in table view
- [ ] Handler test for sort/order param passthrough

### Issue #350 -- Consolidate SQL migrations into single baseline file
- [x] Merge ALTER TABLE / CREATE TABLE from 002-012 into 001_initial.sql
- [x] Delete migration files 002-012
- [x] Verify goose handles existing databases gracefully
- [x] Fresh-start UAT (delete DB, restart, verify clean single-migration startup)
- [x] All existing tests pass with consolidated migration
- [x] Done (merged)

### Issue #351 -- Audit codebase for memory leaks, race conditions, deadlocks, and dead code
- [ ] Fix watcher map race condition (lock released between reads)
- [ ] Fix watcher deadlock risk (lock held during filesystem ops)
- [ ] Fix scanner goroutine leak (context.WithoutCancel)
- [ ] Fix HTTP response body not drained on non-200
- [ ] Fix webhook dispatcher semaphore not recovered on panic
- [ ] Fix event bus silent event drops
- [ ] Fix router state accessed with inconsistent locking
- [ ] Document medium-severity findings as follow-up issues
- [ ] go test -race ./... passes

### Issue #352 -- Audit and improve code reuse and maintainability
- [x] Extract shared BaseClient for connection clients (emby, jellyfin, lidarr)
- [x] Extract RequirePathParam helper for handler parameter extraction
- [x] Extract DecodeBody helper for JSON/form body decoding
- [x] Document medium-impact findings (Phase 2) for follow-up
- [x] Tests
- [x] Done (merged, phase 1)

### Issue #360 -- Image caching strategy for platform-sourced artists
- [ ] Remove "degraded" concept (`IsDegraded()`, all UI references, conditional logic)
- [ ] Add System settings tab (move Behavior and Logging from General)
- [ ] Add Cache settings section with connection source cache and provider image cache
- [ ] Unify image write path: local save + platform push via single abstraction
- [ ] Image upload/replace pushes to connected platform automatically
- [ ] Cache path defaults to `/data/cache/images`, configurable via settings
- [ ] Connection source cache mandatory when no filesystem path
- [ ] Provider image cache toggleable with configurable size
- [ ] Tests

### Issue #516 -- Replace Refresh Metadata ellipsis menu with dropdown split button
- [ ] Split button: primary "Refresh Metadata" + dropdown arrow with "Re-identify Artist"
- [ ] Dropdown arrow clearly visible (chevron, not "...")
- [ ] Keyboard-accessible (Enter, arrow keys, Escape)
- [ ] Tests
- [ ] PR merged

### Issue #501 -- Aggregate metadata completeness reports
- [ ] Completeness calculation: populated fields / applicable fields per artist type
- [ ] Per-field breakdown with count, total, percentage
- [ ] Per-library breakdown
- [ ] Scanner exclusion list artists excluded from metrics
- [ ] "Lowest completeness" bottom-N artists table
- [ ] API endpoint: `GET /api/v1/reports/metadata-completeness`
- [ ] Dashboard widget with Chart.js
- [ ] Tests
- [ ] PR merged

### Issue #504 -- Archive and purge revoked API tokens
- [ ] Token lifecycle: active -> revoked -> archived -> deleted
- [ ] "Archive" button, "Show Archived" toggle, "Delete" with confirmation
- [ ] Audit log entries anonymized on delete
- [ ] API endpoints with proper status codes
- [ ] Tests
- [ ] PR merged

### Issue #508 -- Audit and complete Bruno API test collections
- [ ] Compare all routes in router.go against Bruno requests
- [ ] Add missing endpoint requests
- [ ] Add error case requests for key endpoints
- [ ] Collection runs end-to-end without failures
- [ ] PR merged

### Issue #510 -- Codebase documentation audit
- [ ] All exported types, interfaces, functions have doc comments
- [ ] Package-level documentation for all packages in `internal/`
- [ ] `go doc` output clean for each package
- [ ] Tests
- [ ] PR merged

### Issue #511 -- Performance profiling via fuzzing and load testing
- [ ] Fuzz tests for NFO parser, JSON handlers, image upload
- [ ] Load test scripts (k6 or vegeta) for key API endpoints
- [ ] pprof enabled in debug mode
- [ ] Slow queries identified via EXPLAIN QUERY PLAN
- [ ] Findings documented
- [ ] PR merged

### Issue #524 -- smoke.sh configurable music path parameter
- [ ] `--music-path` CLI argument and `SW_MUSIC_PATH` env var
- [ ] Precedence: CLI > env var > Stillwater API query
- [ ] Updated usage/help text
- [ ] Tests
- [ ] PR merged

## Worktrees

| Directory | Branch | Issue | Status |
|-----------|--------|-------|--------|
| (created when work begins) | | | |

## UAT / Merge Order

Session 1 (foundation -- completed):
1. #350 (SQL squash) -- MERGED
2. #352 (refactoring) -- MERGED (phase 1)

Session 2 (analysis + safety):
3. #323 (code analysis) -- no runtime changes
4. #351 (runtime safety) -- fixes across multiple packages

Session 3 (quick wins):
5. #524 (smoke.sh path parameter) -- small, independent
6. #516 (refresh metadata dropdown) -- UX improvement

Session 4 (UI features):
7. #349 (artist sort) -- UI feature
8. #501 (metadata completeness reports) -- reports/dashboard

Session 5 (field system + settings):
9. #322 (universal field editing) -- extends existing field system
10. #504 (API token lifecycle) -- settings feature
11. #321 (history tab) -- builds on final field set from #322

Session 6 (code health):
12. #508 (Bruno audit) -- testing infrastructure
13. #510 (code docs audit) -- documentation

Session 7 (performance + caching):
14. #511 (perf profiling) -- fuzzing, load testing
15. #360 (image caching) -- after #352 refactoring lands

## Notes

- 2026-03-01: Plan file created.
- 2026-03-02: Added #349, #350, #351, #352 to milestone scope.
- 2026-03-02: Added #360 (image caching). Depends on #352.
- 2026-03-18: Added #501, #504, #508, #510, #511, #516, #524 from batch issue creation.
- #321 is `scope: large` with `[mode: plan] [model: opus]`; #322 is `scope: medium` with `[mode: plan] [model: sonnet]`; #323 is `scope: medium` with `[mode: direct] [model: sonnet]`.
- #349 is `scope: small` with `[mode: direct] [model: sonnet]`; #350 is `scope: medium` with `[mode: plan] [model: sonnet]`; #351 is `scope: medium` with `[mode: direct] [model: opus]`; #352 is `scope: medium` with `[mode: plan] [model: sonnet]`.
- #511 is `scope: large` with `[mode: plan] [model: opus] [effort: high]`.
- Existing `nfo_snapshots` table tracks full NFO XML; new `metadata_changes` table tracks individual field-level changes -- these are complementary, not replacements.
- Image archive directory: `{dataDir}/image_archive/{artistID}/` with timestamped filenames.
