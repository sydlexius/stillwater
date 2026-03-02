# Milestone 30 -- Metadata UX & Code Health

## Goal

Add metadata change tracking with rollback (history tab), make all 19 metadata fields
editable and always visible, and improve test coverage across under-tested packages.

## Acceptance Criteria

- [ ] All metadata changes (manual edits, provider fetches, rule engine, scans) are recorded with field, old/new value, and source
- [ ] Image changes are archived before replacement, enabling rollback
- [ ] Artist detail page has a History tab showing a timeline of changes with rollback actions
- [ ] All 19 metadata fields (including name, sort_name, disambiguation, provider IDs) are editable via the field API
- [ ] All fields are always visible on the artist detail page (not hidden when empty)
- [ ] NFO write-back works for newly editable fields
- [ ] Code analysis findings are documented with coverage numbers
- [ ] Test coverage improved for critical packages (especially `internal/api` at 12.5%)

## Dependency Map

```
#321 (history tab)  ----\
                         +--> all independent, merge in order below
#322 (universal edit) --/
#323 (code analysis) -------> no runtime changes, merge first
```

All three issues are independent -- no blocking relationships. #322 (universal editing)
informs #321 (history) since history needs to track edits on newly-editable fields, but
they can be implemented in parallel as long as history hooks are added to the existing
`UpdateField`/`ClearField` paths which both issues share.

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
- [ ] PR opened (#324)
- [ ] CI passing
- [ ] PR merged

### Issue #322 -- Make all metadata fields editable/fetchable and always visible
- [ ] Extend `fieldColumnMap` with name, sort_name, disambiguation, musicbrainz_id, audiodb_id, discogs_id, wikidata_id, deezer_id
- [ ] Extend `FieldValueFromArtist` switch for new fields
- [ ] Validation for critical fields (name non-empty, provider ID format checks)
- [ ] NFO write-back for newly editable fields
- [ ] Update artist detail template to always show all fields (not hidden when empty)
- [ ] Provider fetch support for disambiguation (MusicBrainz), provider IDs (respective providers)
- [ ] Tests
- [ ] PR opened (#325)
- [ ] CI passing
- [ ] PR merged

### Issue #323 -- Run code analysis, document findings, and improve test coverage
- [ ] Document current coverage numbers per package
- [ ] Identify critical gaps (internal/api at 12.5%, auth/config/database/encryption at 0%)
- [ ] Add API handler tests using existing testRouter pattern
- [ ] Add tests for auth, config, database, encryption packages
- [ ] Improve filesystem and middleware coverage
- [ ] Tests pass with race detector
- [ ] PR opened (#326)
- [ ] CI passing
- [ ] PR merged

## UAT / Merge Order

1. PR #326 for #323 (code analysis) -- no runtime changes, merge first
2. PR #325 for #322 (universal field editing) -- extends existing field system
3. PR #324 for #321 (history tab) -- builds on final field set from #322

## Notes

- 2026-03-01: Plan file created. Issues and PR numbers reserved; implementation PRs are not yet opened and will start as design/analysis docs only.
- #321 is `scope: large` with `[mode: plan] [model: opus]`; #322 is `scope: medium` with `[mode: plan] [model: sonnet]`; #323 is `scope: medium` with `[mode: direct] [model: sonnet]`.
- Existing `nfo_snapshots` table tracks full NFO XML; new `metadata_changes` table tracks individual field-level changes -- these are complementary, not replacements.
- Image archive directory: `{dataDir}/image_archive/{artistID}/` with timestamped filenames.
