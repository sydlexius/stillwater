# Milestone 27 -- Emby/Jellyfin Import & Sync

## Goal

Import metadata and images from Emby/Jellyfin during library populate, view platform
state on artist detail pages, and manage (delete) platform images. Closes the gap
where only push direction works but pull does not.

## Acceptance Criteria

- [ ] Library populate imports biography, genres, and dates from Emby/Jellyfin
- [ ] Library populate downloads Primary/Backdrop/Logo/Banner images from platforms
- [ ] Image download skips if local image already exists
- [ ] Artist detail page shows platform state with side-by-side comparison
- [ ] Users can delete individual images from Emby/Jellyfin via the UI
- [ ] Both Emby and Jellyfin clients support all new methods

## Dependency Map

```
#230 (metadata import) --\
                          +--> #232 (view platform state) --> #233 (delete images)
#231 (image import)   --/
```

#230 and #231 are independent of each other.
#232 depends on #230 and #231 (uses expanded fields and image methods).
#233 depends on #232 (delete button lives in the platform state view).

## Checklist

### Issue #230 -- Emby/Jellyfin metadata not imported during library populate
- [ ] Expand `ArtistItem` struct in `emby/types.go` (Overview, Genres, PremiereDate, EndDate)
- [ ] Update `GetArtists()` query fields in `emby/client.go`
- [ ] Update `populateFromEmbyCtx()` in `handlers_connection_library.go`
- [ ] Mirror changes in `jellyfin/types.go` and `jellyfin/client.go`
- [ ] Update `populateFromJellyfinCtx()` in `handlers_connection_library.go`
- [ ] Investigate additional fields (Tags, SortName) from real server data
- [ ] Tests
- [ ] PR opened (#?)
- [ ] CI passing
- [ ] PR merged

### Issue #231 -- Emby/Jellyfin images not downloaded during import
- [ ] Add `GetArtistImage(ctx, artistID, imageType) ([]byte, string, error)` to Emby client
- [ ] Add same method to Jellyfin client
- [ ] Download images during populate for each artist
- [ ] Save using active naming config via `image.Save()`
- [ ] Skip download if local image already exists
- [ ] Update artist image flags in database after download
- [ ] Tests
- [ ] PR opened (#?)
- [ ] CI passing
- [ ] PR merged

### Issue #232 -- View platform state on artist detail page
- [ ] Add `GetArtistDetail(ctx, artistID) (*ArtistDetail, error)` to Emby client
- [ ] Add same method to Jellyfin client
- [ ] New API endpoint for fetching platform state
- [ ] New section on `artist_detail.templ` with side-by-side comparison
- [ ] Visual indicators for mismatched fields
- [ ] Push/pull action buttons per field
- [ ] Tests
- [ ] PR opened (#?)
- [ ] CI passing
- [ ] PR merged

### Issue #233 -- Delete images from Emby/Jellyfin
- [ ] Add `delete()` HTTP helper to Emby client (alongside `get()` and `post()`)
- [ ] Add `DeleteImage(ctx, artistID, imageType) error` to Emby client
- [ ] Add same methods to Jellyfin client
- [ ] Add `ImageDeleter` interface to `connection/push.go`
- [ ] Add `DELETE /api/v1/artists/{id}/push/images/{type}` endpoint
- [ ] Add delete button to platform state view from #232
- [ ] Tests
- [ ] PR opened (#?)
- [ ] CI passing
- [ ] PR merged

## UAT / Merge Order

Session 1 (import):
1. PR for #230 (base: main) -- metadata import
2. PR for #231 (base: main) -- image import

Session 2 (platform state + delete):
3. PR for #232 (base: main, after #230 and #231 merge)
4. PR for #233 (base: main or stacked on #232)

## Notes

- Emby API: `GET /Items/{id}/Images/{type}` returns raw image bytes
- Jellyfin API uses the same endpoint pattern
- Import policy: never overwrite existing local images (user changes take priority)
- `delete()` HTTP helper needed because only `get()` and `post()` exist in current clients
