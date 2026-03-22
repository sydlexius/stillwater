# Platform NFO and Image Write Behavior

**Date:** 2026-03-22
**Test artist:** 38th Parallel (MBID: e00c9fbd-c18b-4d0c-ad4b-ce661607aa97)
**Purpose:** Determine how Emby, Jellyfin, and Lidarr write NFO files and images to
shared filesystems, informing the evidence-based shared filesystem detection design.

## Test Environment

- Emby: localhost:8096, Music library at /music and /classical
- Jellyfin: localhost:8097, Music library at /music and /classical
- Lidarr: localhost:8686, root folder at /music, Kodi/Emby metadata consumer enabled
- All three platforms share the same /music bind mount

## Summary Matrix

### NFO Write Behavior

| Scenario | Emby | Jellyfin | Lidarr |
|----------|------|----------|--------|
| Rewrites NFO on refresh | Yes | Yes | No |
| Preserves `<stillwater>` element | Yes | Yes (with savers on) | n/a (doesn't rewrite) |
| Preserves `<formed>` / `<disbanded>` | No | No | n/a |
| Preserves `<biography>` | No (blanked) | Yes | n/a |
| Preserves `<genre>` | No (changed) | No (changed) | n/a |
| Creates NFO if missing | Not tested | Not tested | No |
| MetadataSavers=[] stops writes | Yes | **No** | n/a |
| SaveLocalMetadata=false stops writes | No | **No** | n/a |
| Combined (both disabled) stops writes | Yes (MetadataSavers=[]) | **No (still writes!)** | n/a |
| `<lockdata>true</lockdata>` in NFO stops writes | **Yes** | **Yes** | n/a (doesn't rewrite) |

### Image Write Behavior

| Scenario | Emby | Jellyfin | Lidarr |
|----------|------|----------|--------|
| Downloads images (no fetchers) | No | No | n/a |
| Downloads images (fetchers enabled) | Yes (missing only) | Yes (ReplaceAllImages) | Yes (missing only) |
| Replaces existing images | No | Yes (with ReplaceAllImages) | No |
| Strips EXIF from existing images | No | No | No |
| Strips EXIF from downloaded images | n/a (own images) | n/a | n/a |
| Uses platform naming conventions | fanart.jpg | backdrop1.jpg | fanart.jpg |

### Setting Effectiveness

| Setting | Emby | Jellyfin |
|---------|------|----------|
| `MetadataSavers: ['Nfo']` | Writes NFO on every refresh | Writes NFO on every refresh |
| `MetadataSavers: []` | **Stops NFO writes** | Does NOT stop NFO writes |
| `SaveLocalMetadata: false` | Does NOT stop NFO writes alone | Does NOT stop NFO writes |
| Both disabled | **Stops NFO writes** | **Does NOT stop NFO writes** |
| `ImageFetchers: []` | Stops image downloads | Stops image downloads |
| `ImageFetchers: ['TheAudioDB']` | Downloads missing images | Downloads images (can replace all) |

## Detailed Findings

### Emby

**Plugins tested:** MusicBrainz, TheAudioDb, Fanart.tv, Discogs (all installed)

**NFO behavior:**
- Rewrites the entire NFO on every metadata refresh, even with
  `ReplaceAllMetadata=false`
- Strips `<formed>` and `<disbanded>` elements
- Blanks `<biography>` (replaces with `<biography />`)
- Changes genre to its internal value ("Alternative Rock" instead of
  "Contemporary Christian")
- Adds `<lockdata>`, `<dateadded>`, `<runtime>`, `<outline>`, `<album>`,
  `<uniqueid>` elements
- Preserves unrecognized elements including `<stillwater version="1" .../>`
- With fetchers enabled, adds provider IDs (e.g., `<discogsartistid>`)
- Changes encoding to UTF-8 with BOM
- **Disabling MetadataSavers (setting to empty array) stops NFO writes**
- SaveLocalMetadata=false alone does NOT stop writes
- **`<lockdata>true</lockdata>` in the NFO: fully respected.** When Emby reads
  the NFO and finds lockdata=true, it imports the lock into its internal DB
  and refuses to overwrite the NFO on subsequent refreshes -- even with
  ReplaceAllMetadata=true. All fields (formed, disbanded, biography, genre,
  stillwater canary) are preserved. This is the most reliable protection.

**Image behavior:**
- With no image fetchers: does not touch images
- With TheAudioDb + FanArt fetchers enabled: downloads missing images only
  (created fanart.jpg, did not replace existing folder.jpg)
- Does not re-encode or strip EXIF from existing images
- Downloaded images have no Stillwater EXIF (expected)
- Uses standard naming: fanart.jpg

**API settings keys:**
- `LibraryOptions.MetadataSavers` -- array, remove 'Nfo' to stop NFO writes
- `LibraryOptions.TypeOptions[MusicArtist].ImageFetchers` -- array of provider names
- `LibraryOptions.TypeOptions[MusicArtist].MetadataFetchers` -- array of provider names

### Jellyfin

**Stock providers:** TheAudioDB (image), MusicBrainz (metadata)

**NFO behavior:**
- Rewrites the entire NFO on every metadata refresh
- Strips `<formed>` and `<disbanded>` elements
- Preserves `<biography>` content (unlike Emby)
- Changes genre to its internal value
- Adds `<lockdata>`, `<dateadded>`, `<runtime>`, `<outline>`, `<studio>`, `<art>`,
  `<album>` elements
- Preserves `<stillwater>` element when MetadataSavers includes 'Nfo'
- **CRITICAL: With MetadataSavers=[] AND SaveLocalMetadata=false, Jellyfin
  STILL writes the NFO AND strips the `<stillwater>` element.** The resulting
  NFO is minimal (name, MBID, empty biography/outline only). This is MORE
  destructive than with savers enabled.
- Changes encoding to UTF-8 with BOM

**Image behavior:**
- With no image fetchers: does not touch images
- With TheAudioDB fetcher: downloads images
- With ReplaceAllImages=true: **replaces existing images**, strips EXIF,
  may use different naming (backdrop1.jpg instead of backdrop.jpg),
  deletes originals
- With ReplaceAllImages=false: downloads missing images only (did not create
  new images in normal refresh mode despite fetcher being enabled)
- Does not re-encode or strip EXIF from existing images during normal refresh

**API settings keys:**
- `LibraryOptions.MetadataSavers` -- array, but removing 'Nfo' does NOT stop writes
- `LibraryOptions.EnableInternetProviders` -- boolean, controls fetcher activation
- `LibraryOptions.TypeOptions[MusicArtist].ImageFetchers` -- array of provider names

**lockdata behavior:**
- `<lockdata>true</lockdata>` in the NFO is fully respected. Jellyfin reads it,
  imports the lock, and refuses to overwrite the NFO on subsequent refreshes --
  even with ReplaceAllMetadata=true and FullRefresh.
- This is the ONLY reliable way to prevent Jellyfin from overwriting NFOs.
  MetadataSavers=[] and SaveLocalMetadata=false do NOT work.
- Jellyfin does NOT rewrite NFOs during normal library scans or Default refresh
  mode -- only on explicit FullRefresh. The lockdata protection is most important
  when a user manually triggers "Refresh Metadata" on an artist.

### Lidarr

**Version:** 3.1.0.4875
**Metadata consumer:** Kodi (XBMC) / Emby (enabled with artistMetadata,
albumMetadata, artistImages, albumImages all true)

**NFO behavior:**
- Does NOT rewrite existing NFO files on refresh or rescan
- Does NOT create NFO files when missing (with existing artist directory)
- Appears to only write NFO/images during initial import, not on subsequent refreshes
- Metadata consumer settings may only apply to new imports

**Image behavior:**
- Downloads missing images on artist add (created fanart.jpg)
- Does NOT replace existing images
- Does NOT touch images on refresh/rescan (only on initial add)
- Downloaded images have their own EXIF (5158 bytes, from source) but no
  Stillwater tags
- Uses standard Kodi naming: fanart.jpg

**API settings keys:**
- `GET/PUT /api/v1/metadata/1` -- Kodi metadata consumer enable/disable
- Fields: artistMetadata, albumMetadata, artistImages, albumImages
- `GET/PUT /api/v1/config/metadataProvider` -- writeAudioTags, scrubAudioTags

## Implications for Shared-FS Detection

### Decision Matrix

| Platform | NFO Saver | Image Fetcher | Verdict |
|----------|-----------|---------------|---------|
| Emby | **Coexist with lockdata** | **Warn** (adds missing) | Stillwater writes `<lockdata>true</lockdata>` to protect NFOs; warn about image fetchers |
| Jellyfin | **Coexist with lockdata** | **Block** (can replace + strip EXIF) | Stillwater writes `<lockdata>true</lockdata>` to protect NFOs; block image fetchers with ReplaceAllImages capability |
| Lidarr | **Coexist** (doesn't rewrite) | **Warn** (adds missing) | Safe for existing data; warn about unwanted image additions |

### The lockdata Solution

**Both Emby and Jellyfin respect `<lockdata>true</lockdata>` in NFO files.**
When either platform reads an NFO containing this element, it imports the lock
into its internal database and refuses to overwrite the NFO on subsequent
refreshes -- even with ReplaceAllMetadata=true on a FullRefresh.

**Implication for Stillwater:** The NFO writer should always include
`<lockdata>true</lockdata>` in every NFO it writes. This is simpler and more
reliable than trying to disable platform savers via API:

- Works on both Emby and Jellyfin
- Requires no platform API calls to configure
- Protects individual NFOs (not a library-wide setting)
- Self-documenting (the lock is visible in the NFO)
- Survives platform reinstalls/reconfigurations

### Remaining Warnings

**Emby with image fetchers:**
> "Emby's image fetchers (TheAudioDb, FanArt) are enabled and may download
> additional images to your music directory. Stillwater's NFOs are protected
> by lockdata, but image conflicts may occur. Consider disabling image
> fetchers: [Emby Library Settings](http://localhost:8096/web/#!/library.html)"
>
> API: Set `ImageFetchers: []` for MusicArtist type

**Jellyfin with image fetchers:**
> "Jellyfin's TheAudioDB image fetcher is enabled. With ReplaceAllImages,
> Jellyfin can replace existing images and strip EXIF provenance data.
> Disable image fetchers in Jellyfin's library settings."
>
> API: Set `ImageFetchers: []` for MusicArtist type

**Lidarr with artist images:**
> "Lidarr's Kodi metadata consumer may download images to your music directory.
> While Lidarr does not overwrite existing files, it may add images that conflict
> with Stillwater's dedup rules. Consider disabling artist images."
>
> API: Set `artistImages: false` in metadata consumer ID 1

### Key Takeaways

1. **`<lockdata>true</lockdata>` is the primary defense for NFOs.** Both Emby and
   Jellyfin respect it. Stillwater should include it in every NFO it writes.
2. **mtime detection remains the most reliable shared-FS signal.** All platforms
   change file mtimes when they write.
3. **The `<stillwater>` NFO canary is preserved by both platforms** when lockdata
   is set (which it always will be when Stillwater writes). Without lockdata,
   Jellyfin strips it.
4. **EXIF provenance on images is preserved** by all platforms during normal
   operations. Only Jellyfin with ReplaceAllImages=true strips EXIF.
5. **Lidarr is the safest coexistence partner.** It only writes on initial import,
   doesn't rewrite existing files, and respects what's already on disk.
6. **Image fetchers are the remaining risk.** Both Emby and Jellyfin can download
   images when fetchers are enabled. Emby only adds missing; Jellyfin can
   replace existing (with ReplaceAllImages). Image fetcher status should be
   checked and warned about.
7. **Neither platform rewrites NFOs during normal library scans.** Clobbering
   only happens on explicit FullRefresh operations. The lockdata protection
   covers this case.

## Completed Tests

- [x] Emby: `<lockdata>true</lockdata>` in NFO prevents rewrites -- **YES**
- [x] Jellyfin: `<lockdata>true</lockdata>` in NFO prevents rewrites -- **YES**
- [x] Jellyfin: MetadataSavers=[] stops writes -- **NO** (Jellyfin ignores this)
- [x] Emby: MetadataSavers=[] stops writes -- **YES**
- [x] Jellyfin: Normal library scan rewrites NFOs -- **NO** (only FullRefresh does)
- [x] Emby: Normal library scan rewrites NFOs -- stuck at 0% with fetchers, works without
- [x] Lidarr: Fresh artist import -- creates sparse NFO (name, MBID, rating only), no images, no lockdata
- [x] Lidarr: NFO creation timing -- happens on RescanFolders, not on add or RefreshArtist
- [x] Emby: Image fetchers (TheAudioDb, FanArt) -- downloads missing images only
- [x] Jellyfin: Image fetcher (TheAudioDB) -- downloads and can replace with ReplaceAllImages
- [x] All platforms: EXIF preservation on existing images -- **YES** (normal operations)
