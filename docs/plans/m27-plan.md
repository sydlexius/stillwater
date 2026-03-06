# Milestone 27 -- Emby/Jellyfin Import & Sync

## Goal

Full import and CRUD parity between local library artists and Emby/Jellyfin-connected
artists. Library populate imports metadata and images from platforms. Image and metadata
management on the artist detail page works identically regardless of library type -- no
"degraded mode", no explicit push step required from users.

## Acceptance Criteria

- [x] Library populate imports biography, genres, and dates from Emby/Jellyfin
- [x] Library populate downloads Primary/Backdrop/Logo/Banner images from platforms
- [x] Image download skips if local image already exists
- [ ] Image upload/replace/delete for platform artists routes to platform API (same UI as local)
- [ ] Metadata field saves for platform artists auto-push to platform immediately
- [ ] Platform State comparison section removed from artist detail page (moved to opt-in debug panel)
- [ ] Both Emby and Jellyfin clients support all new methods

## Dependency Map

```
#230 (metadata import) --\
                          +--> #232 (view platform state) --> #233 (delete images)
#231 (image import)   --/
  \--> #357 (multiple backdrops)

#408 (unified image CRUD) --> #410 (remove Platform State section) --> #411 (debug panel)
#409 (auto-push metadata)  [parallel with #408]
```

## Checklist

### Issue #230 -- Emby/Jellyfin metadata not imported during library populate
- [x] Done (merged)

### Issue #231 -- Emby/Jellyfin images not downloaded during import
- [x] Done (merged)

### Issue #357 -- Support multiple backdrop images from Emby/Jellyfin
- [ ] Add `BackdropImageTags []string` to `ArtistItem` in both `emby/types.go` and `jellyfin/types.go`
- [ ] Update `downloadPlatformImages` to handle `BackdropImageTags` separately from `ImageTags`
- [ ] Construct indexed URLs (`/Images/Backdrop/0`, `/Images/Backdrop/1`, etc.)
- [ ] Save with correct naming (fanart.jpg, fanart1.jpg, fanart2.jpg, ...)
- [ ] Update `FanartCount` on artist records
- [ ] Tests

### Issue #232 -- View platform state on artist detail page
- [x] Done (merged)

### Issue #233 -- Delete images from Emby/Jellyfin
- [x] Done (merged)

### Issue #408 -- Unified image CRUD: route upload/delete to platform API
- [ ] Image upload/replace routes to platform API for platform artists
- [ ] Image delete routes to platform API for platform artists
- [ ] Images section renders identically for local and platform artists
- [ ] All "degraded mode" UI references removed
- [ ] Tests
- [ ] Docs updated

### Issue #409 -- Auto-push metadata changes to Emby/Jellyfin on save
- [ ] Metadata field save triggers async PushMetadata for platform artists
- [ ] Emby "refresh metadata" behavior investigated and handled if needed
- [ ] Tests
- [ ] Docs updated

### Issue #410 -- Remove Platform State section from artist detail page
- [ ] Platform State section removed from artist_detail.templ
- [ ] API endpoint and handler retained for debug panel use
- [ ] Build and tests pass
- [ ] Must be done after #408

### Issue #411 -- Platform State debug panel (opt-in advanced setting)
- [ ] Advanced settings toggle added
- [ ] Debug Info section on artist page when toggle enabled
- [ ] Read-only platform state view
- [ ] Must be done after #410

## UAT / Merge Order

Session 1 (import) -- complete:
1. #230 -- metadata import -- MERGED
2. #231 -- image import -- MERGED

Session 2 (backdrops):
3. #357 -- multiple backdrop images (after #231)

Session 3 (platform state + delete) -- complete:
4. #232 -- view platform state -- MERGED
5. #233 -- delete platform images -- MERGED

Session 4 (unified CRUD):
6. #408 -- unified image CRUD (base: main)
7. #409 -- auto-push metadata (parallel with #408, base: main)
8. #410 -- remove Platform State section (after #408 merges)
9. #411 -- debug panel (after #410 merges)

## Notes

- Emby API: `GET /Items/{id}/Images/{type}` returns raw image bytes
- Jellyfin API uses the same endpoint pattern
- Import policy: never overwrite existing local images (user changes take priority)
- Image cache directory (`/data/cache/images/{artistID}/`) created automatically for platform artists
- #385 closed as wontfix -- adding push buttons to the Platform State view was the wrong direction
- The correct model: platform connections are transparent to users; Stillwater routes to the right backend

### #230 investigation findings (2026-03-02)

- Switched from `/Artists` to `/Artists/AlbumArtists` endpoint. AlbumArtists matches
  folder structure and excludes featured/track-only artists.
- The AlbumArtists list endpoint returns Overview when present but does not return
  Tags, PremiereDate, or EndDate even when requested via the Fields parameter.
  Full metadata is only available via per-item queries (`/Users/{userId}/Items/{itemId}`).
- Genres from the list endpoint are aggregated from audio file tags (child items),
  not from the artist item's own metadata. This is still useful data.
- Emby's NFO reader may not map `<biography>` to Overview for artist items, or
  requires a manual metadata refresh to pick up NFO data for existing artists.
- NFO date fields (born, formed, died, disbanded) often contain free-text strings
  like "2006 in Cardiff, CA" which Emby cannot parse as dates (expects yyyy-MM-dd).
- Follow-up idea: add a tooltip or info indicator on the Platform Profile page
  showing which metadata fields can be written to each platform (Emby, Jellyfin, Kodi).
- #355 opened for date format normalization (free-text dates silently dropped by Emby).

### #231 implementation findings (2026-03-02)

- Emby does not return a `Path` field for artists via the AlbumArtists endpoint.
  All Emby-sourced artists have empty `Path`. Image cache dir is the fallback.
- `ProviderIds` must be explicitly requested in `Fields=` query parameter.
- MBID backfill added: when an existing artist lacks an MBID but the platform provides
  one, the local record is updated during populate.
- Image cache directory (`/data/cache/images/{artistID}/`) created automatically when
  `artist.Path` is empty. Uses `imageDir(a)` helper throughout handlers.

## Cleanup

When all checklist items are complete and all PRs are merged:
1. Post a summary comment on each completed issue and close it.
2. `git rm docs/plans/m27-plan.md` and commit directly to `main`.
3. Remove all M27 worktrees and delete their branches (remote and local).
4. Update `memory/worktrees.md`.
