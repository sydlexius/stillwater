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
#518 (nomenclature) --> #506 (platform badges use platform-specific terms)
#507 (auto-crop) -- independent
```

#518 (nomenclature) should land before #506 (badges) so badges use the correct terms.
#507 (auto-crop) is fully independent.

## Checklist

### Issue #518 -- Platform-specific image terminology in UI
- [ ] Terminology mapping: Kodi (fanart, folder.jpg) vs Emby/Jellyfin (backdrop, primary)
- [ ] Dynamic labels based on active platform profile
- [ ] Multi-platform display includes platform attribution
- [ ] Tests for terminology mapping

### Issue #506 -- Lidarr and filesystem platform badges on artist list
- [ ] Lidarr badge when artist has Lidarr platform ID
- [ ] Filesystem badge when artist is from filesystem library scan
- [ ] Badges in both grid and table views
- [ ] Tooltips show source details
- [ ] Consistent badge ordering
- [ ] Tests

### Issue #507 -- Auto-crop workflow for wrong-geometry uploads
- [ ] Image upload detects dimension mismatch with slot requirements
- [ ] Crop tool opens with correct aspect ratio (1:1 for thumb, 16:9 for backdrop, etc.)
- [ ] Rule-defined geometry requirements consulted
- [ ] Works for all image slots
- [ ] User can cancel and discard
- [ ] Tests

## UAT / Merge Order

Session 1 (foundation):
1. PR for #518 (base: main) -- platform-specific terminology

Session 2 (badges):
2. PR for #506 (base: main, after #518) -- platform badges

Session 3 (crop workflow):
3. PR for #507 (base: main) -- auto-crop

## Notes

- #518: `[mode: direct] [model: sonnet] [effort: medium]`
- #506: `[mode: direct] [model: sonnet] [effort: medium]`
- #507: `[mode: plan] [model: sonnet] [effort: medium]`
- Cropper.js is already vendored in `web/static/js/`
- Platform naming: Kodi thumb=folder.jpg, fanart=fanart.jpg, logo=logo.png, banner=banner.jpg
