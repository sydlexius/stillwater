# Milestone 35 -- Image & Platform UX

## Goal

Improve image management UX with platform-specific terminology, source badges on
the artist list, and automatic crop workflow for wrong-geometry uploads.

## Acceptance Criteria

- [ ] Lidarr and filesystem source badges on artist list (grid and table views)
- [ ] Image upload/fetch detects geometry mismatch and transitions to crop tool
- [ ] Crop tool opens with correct aspect ratio pre-selected per image slot
- [ ] UI uses platform-specific image terminology (fanart vs backdrop, etc.)

## Dependency Map

```
#518 (nomenclature) -- independent
#506 (platform badges) -- independent
#507 (auto-crop) -- independent
```

All three issues are independent and can proceed in parallel.

## Checklist

### Issue #518 -- Platform-specific image terminology in UI
- [ ] Terminology mapping: Kodi (fanart, folder.jpg) vs Emby/Jellyfin (backdrop, primary)
- [ ] Dynamic labels based on active platform profile
- [ ] Multi-platform display includes platform attribution
- [ ] Tests for terminology mapping
- [ ] PR merged

### Issue #506 -- Lidarr and filesystem platform badges on artist list
- [ ] Lidarr badge when artist has Lidarr platform ID
- [ ] Filesystem badge when artist is from filesystem library scan
- [ ] Badges in both grid and table views
- [ ] Tooltips show source details
- [ ] Consistent badge ordering
- [ ] Tests
- [ ] PR merged

### Issue #507 -- Auto-crop workflow for wrong-geometry uploads
- [ ] Image upload detects dimension mismatch with slot requirements
- [ ] Crop tool opens with correct aspect ratio (1:1 for thumb, 16:9 for backdrop, etc.)
- [ ] Rule-defined geometry requirements consulted
- [ ] Works for all image slots
- [ ] User can cancel and discard
- [ ] Tests
- [ ] PR merged

## Worktrees

| Directory | Branch | Issue | Status |
|-----------|--------|-------|--------|
| (created when work begins) | | | |

## UAT / Merge Order

All three are independent and can be worked in any order:
1. PR for #518 (base: main) -- platform-specific terminology
2. PR for #506 (base: main) -- platform badges
3. PR for #507 (base: main) -- auto-crop workflow

## Notes

- #518: `[mode: direct] [model: sonnet] [effort: medium]`
- #506: `[mode: direct] [model: sonnet] [effort: medium]`
- #507: `[mode: plan] [model: sonnet] [effort: medium]`
- Cropper.js is already vendored in `web/static/js/`
- Platform naming: Kodi thumb=folder.jpg, fanart=fanart.jpg, logo=logo.png, banner=banner.jpg
