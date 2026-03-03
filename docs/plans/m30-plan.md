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
- [ ] SQL migrations consolidated into single baseline file; fresh installs work cleanly
- [ ] High-severity runtime safety issues (races, leaks, deadlocks) fixed
- [ ] High-impact code duplication reduced (connection clients, handler params, body decoding)

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
early since it touches the migration foundation. #360 (image caching) depends on
#352 (refactoring) since it uses the shared write path abstraction and both touch
image write paths and handler structure.
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
- [ ] PR merged

### Issue #322 -- Make all metadata fields editable/fetchable and always visible
- [ ] Extend `fieldColumnMap` with name, sort_name, disambiguation, musicbrainz_id, audiodb_id, discogs_id, wikidata_id, deezer_id
- [ ] Extend `FieldValueFromArtist` switch for new fields
- [ ] Validation for critical fields (name non-empty, provider ID format checks)
- [ ] NFO write-back for newly editable fields
- [ ] Update artist detail template to always show all fields (not hidden when empty)
- [ ] Provider fetch support for disambiguation (MusicBrainz), provider IDs (respective providers)
- [ ] Tests
- [ ] PR merged

### Issue #323 -- Run code analysis, document findings, and improve test coverage
- [ ] Document current coverage numbers per package
- [ ] Identify critical gaps (internal/api at 12.5%, auth/config/database/encryption at 0%)
- [ ] Add API handler tests using existing testRouter pattern
- [ ] Add tests for auth, config, database, encryption packages
- [ ] Improve filesystem and middleware coverage
- [ ] Tests pass with race detector
- [ ] PR merged

### Issue #349 -- Add sort controls to Artists page grid and table views
- [ ] Sort dropdown in toolbar (name, sort_name, health_score, updated_at, created_at)
- [ ] Asc/desc toggle button
- [ ] Clickable column headers in table view
- [ ] Handler test for sort/order param passthrough
- [ ] PR merged

### Issue #350 -- Consolidate SQL migrations into single baseline file
- [ ] Merge ALTER TABLE / CREATE TABLE from 002-012 into 001_initial.sql
- [ ] Delete migration files 002-012
- [ ] Verify goose handles existing databases gracefully
- [ ] Fresh-start UAT (delete DB, restart, verify clean single-migration startup)
- [ ] All existing tests pass with consolidated migration
- [ ] PR merged

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
- [ ] PR merged

### Issue #352 -- Audit and improve code reuse and maintainability
- [ ] Extract shared BaseClient for connection clients (emby, jellyfin, lidarr)
- [ ] Extract RequirePathParam helper for handler parameter extraction
- [ ] Extract DecodeBody helper for JSON/form body decoding
- [ ] Document medium-impact findings (Phase 2) for follow-up
- [ ] Tests
- [ ] PR merged

### Issue #360 -- Image caching strategy for platform-sourced artists
- [ ] Remove "degraded" concept (`IsDegraded()`, all UI references, conditional logic)
- [ ] Add System settings tab (move Behavior and Logging from General)
- [ ] Add Cache settings section with connection source cache and provider image cache
- [ ] Unify image write path: local save + platform push via single abstraction
- [ ] Image upload/replace pushes to connected platform automatically
- [ ] Cache path defaults to `/cache` (Docker volume mount), configurable via settings
- [ ] Connection source cache mandatory when no filesystem path
- [ ] Provider image cache toggleable with configurable size
- [ ] Tests
- [ ] PR merged

## UAT / Merge Order

1. #350 (SQL squash) -- foundation change, merge first
2. #323 (code analysis) -- no runtime changes
3. #351 (runtime safety) -- fixes across multiple packages
4. #352 (refactoring) -- structural improvements
5. #349 (artist sort) -- UI feature
6. #322 (universal field editing) -- extends existing field system
7. #321 (history tab) -- builds on final field set from #322
8. #360 (image caching) -- after #352 refactoring lands

## Notes

- 2026-03-01: Plan file created. Issues and PR numbers reserved; implementation PRs are not yet opened and will start as design/analysis docs only.
- 2026-03-02: Added #349 (artist sort), #350 (SQL squash), #351 (runtime safety), #352 (refactoring audit) to milestone scope.
- #321 is `scope: large` with `[mode: plan] [model: opus]`; #322 is `scope: medium` with `[mode: plan] [model: sonnet]`; #323 is `scope: medium` with `[mode: direct] [model: sonnet]`.
- #349 is `scope: small` with `[mode: direct] [model: sonnet]`; #350 is `scope: medium` with `[mode: plan] [model: sonnet]`; #351 is `scope: medium` with `[mode: direct] [model: opus]`; #352 is `scope: medium` with `[mode: plan] [model: sonnet]`.
- Existing `nfo_snapshots` table tracks full NFO XML; new `metadata_changes` table tracks individual field-level changes -- these are complementary, not replacements.
- Image archive directory: `{dataDir}/image_archive/{artistID}/` with timestamped filenames.
