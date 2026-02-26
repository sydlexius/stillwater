# Milestone 17 -- Library Management (v0.17.0)

## Goal
Add multi-library support, canonical image naming (deduplication), expanded Emby/Jellyfin metadata push with image upload, and an artist grid/card view.

## Acceptance Criteria
- [x] Artist list page supports table and grid/card views with toggle persistence
- [x] Image saving writes exactly one canonical filename per type; extraneous files detected by rule
- [x] Emby/Jellyfin push includes all metadata fields plus image upload capability
- [x] Multiple music library paths supported with per-library type (regular/classical)
- [x] Degraded mode for libraries with no valid path
- [ ] Libraries can be imported from Emby/Jellyfin connections
- [ ] Imported libraries show source platform indicator

## Dependency Map
```
#179 (Artist grid view)         -- DONE (merged via PR #183)
#161 (Image deduplication)      -- DONE (merged via PR #185, migration 002)
#160 (Push expansion + upload)  -- DONE (merged via PR #185)
#159 (Multi-library support)    -- Phase 1-2 DONE (#186, #187), Phase 3/4 in PR #189
#190 (Library import)           -- depends on #159 merge; extends degraded mode
```

## Checklist

### Issue #179 -- Artist View Selector (grid/card view)
- [x] Implementation
- [x] Tests (manual UAT)
- [x] PR opened (#183)
- [x] CI passing
- [x] PR merged

### Issue #161 -- Image Deduplication: Canonical Filenames
- [x] Implementation
- [x] Tests
- [x] PR opened (#185, combined with #160)
- [x] CI passing
- [x] PR merged

### Issue #160 -- Emby/Jellyfin Push Expansion + Image Upload
- [x] Implementation
- [x] Tests
- [x] PR opened (#185, combined with #161)
- [x] CI passing
- [x] PR merged

### Issue #159 -- Multi-Library Support
- [x] Phase 1: DB Schema + Libraries API (PR #186)
- [x] Phase 2: Onboarding + Settings UI (PR #186, #187)
- [x] Phase 3: Scanner Multi-Library (PR #186; classical derivation dropped -- library type is authoritative)
- [x] Phase 4: Degraded Mode UX + API guards
- [x] Tests
- [x] PR opened (#189)
- [ ] CI passing
- [ ] PR merged

### Issue #190 -- Import Music Libraries from Connections
- [ ] Phase 1: Schema + Discovery API
- [ ] Phase 2: UI (Discover Libraries, source badges)
- [ ] Phase 3: Artist Population
- [ ] PR opened (#?)
- [ ] CI passing
- [ ] PR merged

## UAT / Merge Order
1. ~~PR #183 for #179 (base: main) -- UI-only, safe first~~ MERGED
2. ~~PR #185 for #161 + #160 (base: main) -- combined dedup + push~~ MERGED
3. ~~PR #186 for #159 Phase 1 (base: main) -- migration 003~~ MERGED
4. ~~PR #187 for #159 Phase 2 (base: main) -- library selector~~ MERGED
5. PR #189 for #159 Phase 3/4 (base: main) -- degraded mode guards + UX
6. PR for #190 (base: main) -- library import from connections

---

## Implementation Details

### Issue #161 -- Image Deduplication

Migration 002 already exists on disk (`internal/database/migrations/002_image_naming_canonical.sql`). It converts platform profile image_naming from array JSON to single-string JSON and seeds the `extraneous_images` rule. `UnmarshalImageNaming()` in `internal/platform/model.go:74` handles both formats via legacy fallback (wraps strings into 1-element arrays).

#### Files to modify
| File | Change |
|------|--------|
| `internal/api/handlers_image.go` | `getActiveNamingConfig()` returns only `[]string{profile.ImageNaming.PrimaryName(imageType)}` instead of all names |
| `internal/rule/service.go` | Add `RuleExtraneousImages = "extraneous_images"` constant; add to `defaultRules` slice |
| `internal/rule/engine.go` | Add `platformService *platform.Service` param to `NewEngine()`; register `extraneous_images` checker as closure |
| `internal/rule/checkers.go` | Add `makeExtraneousImagesChecker()` method on Engine returning a `Checker` closure |
| `internal/rule/fixers.go` | Add `ExtraneousImagesFixer` struct with `CanFix()`/`Fix()` methods |
| `cmd/stillwater/main.go` | Pass `platformService` to `NewEngine()`; register `ExtraneousImagesFixer` |
| `internal/rule/checkers_test.go` | Tests for extraneous images detection |

#### Checker logic (`checkExtraneousImages`)
1. Get active platform profile via `platformService.GetActive()` to get canonical names
2. Build expected set: for each type (thumb, fanart, logo, banner), include primary name + alternate extension variant (e.g. folder.jpg and folder.png)
3. Also include `artist.nfo` as expected
4. Read all files in `a.Path`
5. Flag any image file (.jpg/.jpeg/.png) not in the expected set as extraneous
6. Fixable: true (auto-fix deletes extraneous files)

#### Key design decision
Keep `ImageNaming` struct as `[]string` fields. The migration stores single strings that `UnmarshalImageNaming` wraps into 1-element arrays. `PrimaryName()` already exists at `model.go:41`. The change is surgical: only `getActiveNamingConfig()` callers change, `Save()` itself stays the same.

---

### Issue #160 -- Emby/Jellyfin Push Expansion + Image Upload

#### Files to modify
| File | Change |
|------|--------|
| `internal/connection/push.go` | Expand `ArtistPushData` with Styles, Moods, Disambiguation, Born, Formed, Died, Disbanded, YearsActive, MusicBrainzID; add `ImageUploader` interface |
| `internal/connection/emby/push.go` | Expand `itemUpdateBody` with Tags/ProviderIds/PremiereDate/EndDate; add `UploadImage()` method; add `mapImageType()` helper |
| `internal/connection/jellyfin/push.go` | Mirror emby changes |
| `internal/api/handlers_push.go` | Populate all new `ArtistPushData` fields from artist model; add `handlePushImages` handler |
| `internal/api/router.go` | Register `POST /api/v1/artists/{id}/push/images` |
| `web/templates/onboarding.templ` | Update step 3 connection capability descriptions to mention image upload |
| `internal/connection/emby/client_test.go` | Tests for expanded push + image upload |
| `internal/connection/jellyfin/client_test.go` | Mirror emby tests |

#### API field mapping (Emby/Jellyfin)
| Stillwater field | Emby/Jellyfin field |
|-----------------|---------------------|
| Name | Name |
| SortName | ForcedSortName |
| Biography | Overview |
| Genres | Genres |
| Styles | Tags |
| Born/Formed | PremiereDate |
| Died/Disbanded | EndDate |
| MusicBrainzID | ProviderIds.MusicBrainzArtist |
| Moods | No direct mapping (document limitation) |
| Disambiguation | No direct mapping (document limitation) |
| YearsActive | No direct mapping (document limitation) |

#### Image upload
- Endpoint: `POST /Items/{id}/Images/{type}` with raw bytes + Content-Type header
- Type mapping: thumb=Primary, fanart=Backdrop, logo=Logo, banner=Banner
- Format: image/jpeg for JPG, image/png for PNG

---

### Issue #159 -- Multi-Library Support (4 phases, largest scope)

#### Phase 1: DB Schema + Libraries API

**New migration** `internal/database/migrations/003_multi_library.sql`:
- Create `libraries` table: id, name, path, type (regular|classical), created_at, updated_at
- `ALTER TABLE artists ADD COLUMN library_id TEXT REFERENCES libraries(id)`

**New package** `internal/library/`:
- `model.go` -- Library struct
- `service.go` -- CRUD: Create, GetByID, List, Update, Delete

**New handler file** `internal/api/handlers_library.go`:
- GET/POST `/api/v1/libraries`
- GET/PUT/DELETE `/api/v1/libraries/{id}`

**Startup backfill** in `cmd/stillwater/main.go`:
- After migrations, if no libraries exist, create "Default" from `cfg.Music.LibraryPath`
- Backfill all existing artists with that library_id
- Preserves Docker `SW_MUSIC_PATH` backward compat

**Artist model changes**:
- `internal/artist/model.go` -- add `LibraryID string` field
- `internal/artist/service.go` -- add `library_id` to all queries; add `ListParams.LibraryID` filter

#### Phase 2: Onboarding + Settings UI

**Onboarding**: Add step 3 "Libraries" between Providers (step 2) and Connections (now step 4):
- `web/templates/onboarding.templ` -- update `totalSteps = 4`
- Library card form: Name (text), Path (text + browse), Type (regular/classical select)

**Directory browser**:
- New file `internal/api/handlers_filesystem.go`
- `GET /api/v1/filesystem/browse?path=/music` -- returns subdirectories as HTMX partial
- Security: restrict to allowed root paths

**Settings**: Add "Libraries" section to `web/templates/settings.templ` -- list/add/edit/remove

#### Phase 3: Scanner Multi-Library + Classical Mode

**Scanner refactor** (`internal/scanner/scanner.go`):
- Replace `libraryPath string` field with `libraryService *library.Service`
- `runScan()` iterates over all libraries; skips those with empty path
- `processDirectory()` receives library ID, sets `a.LibraryID`
- Sets `a.IsClassical = (lib.Type == "classical")` during scan

**Classical mode** (`internal/rule/classical.go`):
- Keep `is_classical` on artist model, but derive from library type during scan
- Global `rule.classical_mode` setting stays for skip/composer/performer behavior
- Remove need to manually set is_classical per artist

#### Phase 4: Degraded Mode UX

Libraries with `path = ""` operate in degraded (API-only) mode:
- Banner/badge on library cards in settings UI
- Scanner skip for degraded libraries
- Image and NFO operations disabled for artists in degraded libraries
- Guard checks in handlers that require filesystem access

#### Key files for #159
| File | Change |
|------|--------|
| `internal/database/migrations/003_multi_library.sql` | NEW: libraries table + artist FK |
| `internal/library/model.go` | NEW: Library struct |
| `internal/library/service.go` | NEW: CRUD service |
| `internal/api/handlers_library.go` | NEW: library CRUD handlers |
| `internal/api/handlers_filesystem.go` | NEW: directory browser handler |
| `internal/artist/model.go` | Add LibraryID field |
| `internal/artist/service.go` | Add library_id to queries, add filter |
| `internal/scanner/scanner.go` | Multi-library iteration, remove single libraryPath |
| `internal/rule/classical.go` | Derive from library type |
| `internal/api/router.go` | Register library + filesystem routes |
| `cmd/stillwater/main.go` | Wire library service, startup backfill |
| `web/templates/onboarding.templ` | Add Libraries step (step 3 of 4) |
| `web/templates/settings.templ` | Libraries management section |
| `internal/config/config.go` | Keep MusicConfig.LibraryPath for backward compat |

---

## Risk Areas

1. **#161**: Adding `platformService` to `Engine` changes constructor signature; all callers (main.go + tests) need updating
2. **#160**: Styles/Moods/YearsActive have no direct Emby/Jellyfin API equivalent; document limitations
3. **#159**: `ALTER TABLE artists ADD COLUMN library_id` is safe in SQLite 3.35+ (modernc bundles 3.41+)
4. **#159**: Scanner refactor is significant; existing tests that construct `scanner.NewService()` need updating
5. **#159**: `SW_MUSIC_PATH` env var backward compat via auto-creating "Default" library on first startup

## Verification

- `gofmt -d .` and `go test ./...` before every commit
- UAT via `.\setupdocker.ps1` (PowerShell) for container testing
- After #161: save an image, verify only one file written; enable extraneous_images rule, verify detection
- After #160: push metadata to Emby/Jellyfin, verify expanded fields; upload image via API
- After #159: add multiple libraries, scan each, verify per-library artist listing and classical derivation

## Notes
- 2026-02-25: Plan file created, starting implementation with #179
- 2026-02-25: #179 merged via PR #183
- 2026-02-25: Detailed implementation plan added for #161, #160, #159
- 2026-02-25: #161 + #160 implemented, combined in PR #185 (was #184, renumbered)
- Migration 002 committed with PR #185
- Issue #158 (migration consolidation) is CLOSED; baseline is `001_initial.sql`
- 2026-02-25: #159 Phase 1 merged via PR #186, Phase 2 via PR #187
- 2026-02-25: #159 gap analysis: degraded mode API guards (409 for pathless artists), degraded mode UX badges, artist detail library context, scanner logs for skipped degraded libraries. Classical derivation dropped per review -- library type is authoritative.
- 2026-02-25: PR #188 closed (superseded). Phase 3/4 consolidated into PR #189.
- 2026-02-25: Created #190 (library import from connections) to complete the degraded mode story -- degraded libraries have no creation mechanism without connection import.
