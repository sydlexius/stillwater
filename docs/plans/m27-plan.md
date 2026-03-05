# Milestone 27 -- Emby/Jellyfin Import & Sync

## Goal

Import metadata and images from Emby/Jellyfin during library populate, view platform
state on artist detail pages, and manage (delete) platform images. Closes the gap
where only push direction works but pull does not.

## Acceptance Criteria

- [x] Library populate imports biography, genres, and dates from Emby/Jellyfin
- [x] Library populate downloads Primary/Backdrop/Logo/Banner images from platforms
- [x] Image download skips if local image already exists
- [x] Artist detail page shows platform state with side-by-side comparison
- [x] Users can delete individual images from Emby/Jellyfin via the UI
- [x] Both Emby and Jellyfin clients support all new methods

## Dependency Map

```
#230 (metadata import) --\
                          +--> #232 (view platform state) --> #233 (delete images)
#231 (image import)   --/
  \--> #357 (multiple backdrops)
```

#230 and #231 are independent of each other.
#231 blocks #357 (backdrops use the image download infrastructure from #231).
#232 depends on #230 and #231 (uses expanded fields and image methods).
#233 depends on #232 (delete button lives in the platform state view).

## Checklist

### Issue #230 -- Emby/Jellyfin metadata not imported during library populate
- [x] Expand `ArtistItem` struct in `emby/types.go` (Overview, Genres, Tags, SortName, PremiereDate, EndDate)
- [x] Update `GetArtists()` query fields in `emby/client.go`
- [x] Update `populateFromEmbyCtx()` in `handlers_connection_library.go`
- [x] Mirror changes in `jellyfin/types.go` and `jellyfin/client.go`
- [x] Update `populateFromJellyfinCtx()` in `handlers_connection_library.go`
- [x] Investigate additional fields (Tags, SortName) from real server data
- [x] Switch from `/Artists` to `/Artists/AlbumArtists` endpoint (matches folder structure)
- [x] Tests
- [x] Done (merged)

### Issue #231 -- Emby/Jellyfin images not downloaded during import
- [x] Add `GetArtistImage(ctx, artistID, imageType) ([]byte, string, error)` to Emby client
- [x] Add same method to Jellyfin client
- [x] Download images during populate for each artist
- [x] Save using active naming config via `image.Save()`
- [x] Skip download if local image already exists
- [x] Update artist image flags in database after download
- [x] Add image cache directory fallback for artists without filesystem paths
- [x] Backfill MusicBrainzID from platform ProviderIds onto existing artists
- [x] Tests
- [x] Done (merged)

### Issue #357 -- Support multiple backdrop images from Emby/Jellyfin
- [ ] Add `BackdropImageTags []string` to `ArtistItem` in both `emby/types.go` and `jellyfin/types.go`
- [ ] Update `downloadPlatformImages` to handle `BackdropImageTags` separately from `ImageTags`
- [ ] Construct indexed URLs (`/Images/Backdrop/0`, `/Images/Backdrop/1`, etc.)
- [ ] Save with correct naming (fanart.jpg, fanart1.jpg, fanart2.jpg, ...)
- [ ] Update `FanartCount` on artist records
- [ ] Tests

### Issue #232 -- View platform state on artist detail page
- [x] Add `GetArtistDetail(ctx, artistID) (*ArtistDetail, error)` to Emby client
- [x] Add same method to Jellyfin client
- [x] New API endpoint for fetching platform state
- [x] New section on `artist_detail.templ` with side-by-side comparison
- [x] Visual indicators for mismatched fields
- [x] Push/pull action buttons per field
- [x] Tests
- [x] Done (merged)

### Issue #233 -- Delete images from Emby/Jellyfin
- [x] Add `delete()` HTTP helper to Emby client (alongside `get()` and `post()`)
- [x] Add `DeleteImage(ctx, artistID, imageType) error` to Emby client
- [x] Add same methods to Jellyfin client
- [x] Add `ImageDeleter` interface to `connection/push.go`
- [x] Add `DELETE /api/v1/artists/{id}/push/images/{type}` endpoint
- [x] Add delete button to platform state view from #232
- [x] Tests
- [x] Done (merged)

## UAT / Merge Order

Session 1 (import):
1. #230 -- metadata import -- MERGED
2. #231 -- image import -- MERGED

Session 2 (backdrops):
3. #357 -- multiple backdrop images (after #231 merges)

Session 3 (platform state + delete):
4. #232 -- view platform state -- MERGED
5. #233 -- delete platform images -- MERGED

## Notes

- Emby API: `GET /Items/{id}/Images/{type}` returns raw image bytes
- Jellyfin API uses the same endpoint pattern
- Import policy: never overwrite existing local images (user changes take priority)
- `delete()` HTTP helper needed because only `get()` and `post()` exist in current clients
- Backdrops are in `BackdropImageTags` (array), not `ImageTags` (map) -- discovered during #231 UAT

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
