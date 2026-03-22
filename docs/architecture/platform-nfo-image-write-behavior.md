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

**Jellyfin-specific concern:** There appears to be no reliable API-configurable way
to prevent Jellyfin from writing NFO files. Even with all visible settings disabled,
it still writes. The only reliable prevention may be filesystem permissions (make
the directory read-only to the Jellyfin process) or using the `<lockdata>true</lockdata>`
element in existing NFOs. The lockdata approach needs separate testing.

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
| Emby | **Block** (clobbers) | **Block** (writes to FS) | Disable MetadataSavers=[] and ImageFetchers=[] |
| Jellyfin | **Block** (clobbers, cannot be reliably disabled via API) | **Block** (can replace + strip EXIF) | Warn user to disable via Jellyfin UI; API may be insufficient |
| Lidarr | **Coexist** (doesn't rewrite existing) | **Warn** (adds missing, doesn't replace) | Lidarr is safe for existing data, but may add unwanted images |

### Blocking Messages

**Emby:**
> "Emby's NFO saver is writing metadata to your music directory, which overwrites
> Stillwater's metadata. Disable the NFO saver: [Emby Dashboard > Library > Music >
> NFO](http://localhost:8096/web/#!/library.html)"
>
> API fix: Set `MetadataSavers: []` and `ImageFetchers: []` for MusicArtist type

**Jellyfin:**
> "Jellyfin writes NFO files to your music directory regardless of the NFO saver
> setting. This overwrites Stillwater's metadata. You may need to disable the NFO
> plugin entirely or set artist directories to read-only for the Jellyfin process."
>
> Note: No reliable API-only fix exists. This needs further investigation
> (lockdata element, NFO plugin removal, filesystem permissions).

**Lidarr:**
> "Lidarr's Kodi metadata consumer is enabled and may download images to your music
> directory. While Lidarr does not overwrite existing files, it may add images that
> conflict with Stillwater's dedup rules. Consider disabling artist images in Lidarr's
> metadata consumer settings."
>
> API fix: Set `artistImages: false` in metadata consumer ID 1

### Key Takeaways

1. **mtime detection is the most reliable signal.** All three platforms change the
   NFO mtime when they write, and Emby/Lidarr change image mtimes on download.
2. **The `<stillwater>` NFO canary works for Emby but NOT reliably for Jellyfin.**
   Jellyfin strips unknown elements when writing with savers disabled. Emby
   preserves them in all tested scenarios.
3. **EXIF provenance on images is preserved** by all three platforms during normal
   operations. Only Jellyfin with ReplaceAllImages=true strips EXIF.
4. **Lidarr is the safest coexistence partner.** It only writes on initial import,
   doesn't rewrite existing files, and respects what's already on disk.
5. **Jellyfin is the most problematic.** Its NFO writes cannot be reliably
   disabled via API, and ReplaceAllImages mode is destructive.
6. **Emby is controllable.** MetadataSavers=[] reliably stops NFO writes. Image
   fetcher removal stops image downloads.

## Remaining Tests

- [ ] Jellyfin: Does `<lockdata>true</lockdata>` prevent rewrites?
- [ ] Jellyfin: Does removing the NFO plugin entirely stop writes?
- [ ] Lidarr: NFO creation behavior on fresh artist import (no existing directory)
- [ ] Emby/Jellyfin: Full library scan behavior (vs per-artist refresh)
- [ ] All platforms: Behavior when music directory is mounted read-only
