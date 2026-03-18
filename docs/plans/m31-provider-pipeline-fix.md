# Provider Pipeline Data Quality Fix

## Goal

Fix the provider pipeline so that stored provider-specific IDs are used during
metadata fetch, re-identification persists correctly, and providers validate
returned data against the search term. The test case is Adele: wrong MBID, correct
AudioDB ID, Genius returning Kim Kardashian's biography.

## Issues

| Issue | Title | Priority | Session |
|-------|-------|----------|---------|
| #529 | Re-identify artist does not persist selected MBID | High | 1 |
| #528 | Orchestrator uses stored provider-specific IDs for metadata fetch | Highest | 1 |
| #527 | Genius provider name validation | Medium | 2 |
| #466 | Born/formed cross-contamination rule | Medium | 3 (re-evaluate) |

## Dependency Map

```
#529 (re-identify persistence) -- independent, but must be fixed first
#528 (orchestrator provider IDs) -- independent, highest impact
#527 (Genius validation) -- independent, defense in depth
#466 (born/formed) -- may partially resolve after #528 lands
```

#529 should be investigated first because if re-identify is broken, manual
correction is impossible. #528 is the highest-impact fix and should be in the
same session. #527 is independent and can follow in a second session. #466
should be re-evaluated after #528 and #529 land.

## Session 1: Re-identify persistence + orchestrator provider IDs

### Issue #529 -- Re-identify does not persist MBID

Investigation targets:
- `handleRefreshLink` in `internal/api/handlers_refresh.go` (lines 99-157)
- Verify the database UPDATE for `musicbrainz_id` executes and commits
- Check if the subsequent `executeRefresh` overwrites the newly saved MBID
- Check for silent errors in the link handler
- Test: re-identify, verify MBID changes in database, verify refresh uses new MBID

Checklist:
- [ ] Root cause identified
- [ ] Fix implemented
- [ ] Test: re-identify persists new MBID
- [ ] Test: metadata refresh after re-identify uses the new MBID
- [ ] PR merged

### Issue #528 -- Orchestrator uses stored provider IDs

Key files:
- `internal/provider/orchestrator.go` -- `FetchMetadata()`, `getProviderResult()`
- `internal/provider/orchestrator_test.go`
- `internal/api/handlers_refresh.go` -- caller of FetchMetadata
- `internal/scanner/` -- caller of FetchMetadata
- `internal/artist/service.go` -- provider ID storage

Implementation:
1. Add provider ID map to `FetchMetadata()` signature (or an `ArtistIdentity` struct)
2. In `getProviderResult()`: try stored provider ID first, then MBID, then name
3. Mirror the approach already used in `FetchImages()` via `getProviderImageID()`
4. Update all callers to pass stored provider IDs
5. Tests for: provider ID precedence, wrong MBID + correct provider ID, no provider ID fallback

Checklist:
- [ ] `FetchMetadata()` accepts provider-specific IDs
- [ ] `getProviderResult()` lookup order: provider ID > MBID > name
- [ ] All callers updated (refresh handler, scanner, bulk operations)
- [ ] Tests for precedence and fallback behavior
- [ ] Adele test case: wrong MBID + correct AudioDB ID returns correct bio
- [ ] PR merged

### Session 1 validation

After both fixes:
1. Re-identify Adele with correct MBID -- verify it persists
2. Refresh metadata -- verify AudioDB bio wins (not Genius)
3. Verify born/formed fields are correct
4. Run `go test ./...` with race detector

## Session 2: Genius provider name validation

### Issue #527 -- Genius name validation

Key files:
- `internal/provider/genius/genius.go` -- `getArtistByName()`, `SearchArtist()`
- `internal/provider/genius/genius_test.go`

Implementation:
1. In `getArtistByName()`: after search returns results, compare each result's
   artist name to the search term using string similarity (case-insensitive,
   normalized)
2. Reject results where similarity is below a threshold (e.g., 60%)
3. Fix the hard-coded `Score: 100` in `SearchArtist()` -- use the actual Genius
   relevance score or compute name similarity
4. Consider applying the same validation to other name-lookup providers

Checklist:
- [ ] `getArtistByName()` validates returned name against search term
- [ ] Results below similarity threshold rejected
- [ ] `SearchArtist()` uses meaningful scores (not hard-coded 100)
- [ ] Test: search "Adele" does not return Kim Kardashian
- [ ] Test: search "Radiohead" still returns correct result
- [ ] Consider propagating validation to other name-lookup providers
- [ ] PR merged

## Session 3: Re-evaluate born/formed (#466)

After sessions 1-2 land on main:
1. Re-identify Adele with correct MBID
2. Refresh metadata
3. Check if born/formed fields are now correct
4. If the bug persists, proceed with #466 implementation
5. If resolved, close #466 with a note that the root cause was provider ID mismatch

## Notes

- The `FetchImages()` method already uses provider-specific IDs via
  `getProviderImageID()` -- use the same pattern for `FetchMetadata()`
- All three issues are in M31 (Provider Pipeline & Scraping)
- The Adele artist ID for testing: `78e7d8af-cda4-4d8d-a4bd-d82542cb66d5`
- Correct Adele MBID: `1f9df192-a621-4f54-8850-2c5373b7eac9`
- Wrong Adele MBID (currently stored): `cc2c9c3c-b7bc-4b8b-84d8-4fbd8779e493`
- Correct AudioDB ID (currently stored): `111493`
