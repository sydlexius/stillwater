# Milestone 17 -- Library Management (v0.17.0)

## Goal
Add multi-library support, canonical image naming (deduplication), expanded Emby/Jellyfin metadata push with image upload, and an artist grid/card view.

## Acceptance Criteria
- [ ] Artist list page supports table and grid/card views with toggle persistence
- [ ] Image saving writes exactly one canonical filename per type; extraneous files detected by rule
- [ ] Emby/Jellyfin push includes all metadata fields plus image upload capability
- [ ] Multiple music library paths supported with per-library type (regular/classical)
- [ ] Degraded mode for libraries with no valid path

## Dependency Map
```
#179 (Artist grid view)         -- independent, UI-only
#161 (Image deduplication)      -- independent, migration 002
#160 (Push expansion + upload)  -- independent, no migration
#159 (Multi-library support)    -- migration 003, largest scope
```
No issue blocks another. #161 gets migration 002, #159 gets migration 003.

## Checklist

### Issue #179 -- Artist View Selector (grid/card view)
- [ ] Implementation
- [ ] Tests (manual UAT)
- [ ] PR opened (#?)
- [ ] CI passing
- [ ] PR merged

### Issue #161 -- Image Deduplication: Canonical Filenames
- [ ] Implementation
- [ ] Tests
- [ ] PR opened (#?)
- [ ] CI passing
- [ ] PR merged

### Issue #160 -- Emby/Jellyfin Push Expansion + Image Upload
- [ ] Implementation
- [ ] Tests
- [ ] PR opened (#?)
- [ ] CI passing
- [ ] PR merged

### Issue #159 -- Multi-Library Support
- [ ] Phase 1: DB Schema + Libraries API
- [ ] Phase 2: Onboarding + Settings UI
- [ ] Phase 3: Scanner Multi-Library + Classical Mode
- [ ] Phase 4: Degraded Mode UX
- [ ] Tests
- [ ] PR opened (#?)
- [ ] CI passing
- [ ] PR merged

## UAT / Merge Order
1. PR for #179 (base: main) -- UI-only, safe first
2. PR for #161 (base: main) -- migration 002
3. PR for #160 (base: main) -- no migration, additive
4. PR for #159 (base: main) -- migration 003, largest scope

## Notes
- 2026-02-25: Plan file created, starting implementation with #179
