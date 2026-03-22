# Platform NFO and Image Write Behavior

**Date:** 2026-03-22
**Test artist:** 38th Parallel (MBID: e00c9fbd-c18b-4d0c-ad4b-ce661607aa97)
**Purpose:** Determine how Emby and Jellyfin write NFO files and images to shared
filesystems, informing the evidence-based shared filesystem detection design.

## Test Environment

- Emby: localhost:8096, Music library at /music and /classical
- Jellyfin: localhost:8097, Music library at /music and /classical
- Lidarr: not yet tested (pending container setup)
- Both platforms: MetadataSavers=['Nfo'], SaveLocalMetadata=True
- Both platforms: MetadataFetchers=[] and ImageFetchers=[] for MusicArtist
  (no internet metadata/image providers configured)

## Summary of Findings

| Behavior | Emby | Jellyfin |
|----------|------|----------|
| **Rewrites NFO on refresh** | Yes (always) | Yes (always) |
| **Preserves `<stillwater>` element** | Yes | Yes |
| **Preserves `<formed>`** | No -- stripped | No -- stripped |
| **Preserves `<disbanded>`** | No -- stripped | No -- stripped |
| **Preserves `<biography>`** | No -- blanked to `<biography />` | Yes -- preserved |
| **Preserves `<genre>`** | No -- changed to "Alternative Rock" | No -- changed to "Alternative Rock" |
| **Preserves `<audiodbartistid>`** | No -- stripped | Yes -- preserved |
| **Adds own elements** | `<lockdata>`, `<dateadded>`, `<runtime>`, `<outline>`, `<album>`, `<uniqueid>` | `<lockdata>`, `<dateadded>`, `<runtime>`, `<outline>`, `<studio>`, `<art>`, `<album>` |
| **Changes encoding** | Adds BOM, changes to `utf-8 standalone="yes"` | Adds BOM, changes to `utf-8 standalone="yes"` |
| **Downloads images to filesystem** | No (with no image fetchers configured) | No (with no image fetchers configured) |
| **Modifies existing images** | No | No |
| **Strips image EXIF** | No -- EXIF preserved | No -- EXIF preserved |
| **Behavior with ReplaceAllMetadata=true** | Same as above (no additional damage) | Same as above |

## Detailed Findings

### NFO Behavior

Both platforms **clobber** the NFO on every metadata refresh, even with
`ReplaceAllMetadata=false`. The platforms read the existing NFO, merge their own
internal state, and write a completely new file. Key observations:

1. **Element ordering changes.** Both platforms reorder elements according to
   their own internal schema. Stillwater writes `<name>` first; both platforms
   write `<biography>` or other fields first.

2. **Kodi-specific fields are stripped.** `<formed>` and `<disbanded>` are not
   recognized by either platform and are silently dropped during the rewrite.
   This is despite both platforms preserving the `<stillwater>` custom element.
   The difference: `<stillwater>` uses XML attribute syntax
   (`<stillwater version="1" ... />`), while `<formed>` uses text content
   (`<formed>2000</formed>`). Both should be preserved as "unknown elements,"
   but both platforms strip `<formed>` and `<disbanded>` specifically. This may
   be because they have partial recognition of these fields (Kodi uses them)
   but don't populate them.

3. **Metadata values are overwritten.** Genre changed from "Contemporary
   Christian" (from the NFO) to "Alternative Rock" (from the platform's internal
   data, likely derived from MusicBrainz tags). This happens even with
   `ReplaceAllMetadata=false`.

4. **Emby blanks unknown fields.** Biography was replaced with `<biography />`
   (empty), even though the original NFO had a full biography. Emby's internal
   DB likely had no biography (no metadata fetcher configured), so it wrote
   its empty value.

5. **Jellyfin preserves some fields.** Biography was preserved (Jellyfin likely
   read it from the NFO and stored it internally). `<audiodbartistid>` was also
   preserved. Jellyfin is less destructive than Emby on fields it recognizes.

6. **Both add platform-specific elements.** `<lockdata>`, `<dateadded>`,
   `<runtime>`, `<outline>`, `<album>` are added by both platforms.

### Image Behavior

Neither platform downloaded images to the filesystem in this test. This is
expected because neither has image fetchers configured for MusicArtist.

With `DownloadImagesInAdvance=True` on Emby but no image fetcher plugins enabled,
Emby has nothing to download. The setting is a prerequisite, not a trigger -- it
controls whether images are proactively cached to disk once a fetcher provides
them.

**EXIF provenance in existing images was fully preserved** by both platforms.
Neither platform touched or re-encoded the image files. The `stillwater:v1` EXIF
tag in folder.jpg survived both normal and aggressive refreshes.

### Custom Element Preservation

The `<stillwater version="1" written="2026-03-22T00:00:00Z" />` element was
**preserved by both platforms** across all test scenarios (normal refresh,
ReplaceAllMetadata=true). Both platforms treat it as an unrecognized element and
round-trip it through their NFO writer.

This confirms that a `<stillwater>` provenance marker in NFOs is viable for
shared-FS canary detection. However, both platforms will still clobber the
surrounding metadata fields, making coexistence destructive regardless.

## Implications for Shared-FS Detection Design

1. **NFO canary works.** The `<stillwater>` element survives platform rewrites.
   It can serve as evidence that Stillwater wrote the NFO.

2. **mtime detection works.** Both platforms change the NFO mtime on every
   refresh. If Stillwater records when it last wrote the NFO, any mtime
   drift proves an external writer touched it.

3. **NFO savers must be disabled.** Both platforms destroy Stillwater's metadata
   on refresh. `<formed>`, `<disbanded>`, genre, and (on Emby) biography are
   clobbered. This is not "amend missing data" -- it is "overwrite with
   platform's version." The NFO saver must be disabled for coexistence.

4. **Image savers are safe when unconfigured.** With no image fetchers enabled,
   neither platform writes images to disk. However, if a user enables an image
   fetcher plugin (e.g., Fanart.tv in Emby), this would change. The spike
   should re-test with image fetchers enabled.

5. **EXIF survives.** Neither platform strips EXIF from existing images, even
   during aggressive refresh. The provenance tag is safe.

## Remaining Tests

- [ ] Emby with image fetcher plugins enabled (Fanart.tv, TheAudioDB)
- [ ] Jellyfin with MusicBrainz cover art fetcher enabled
- [ ] Emby/Jellyfin with `SaveLocalMetadata=false` (does it stop NFO writes?)
- [ ] Lidarr metadata agent behavior (pending container setup)
- [ ] Full library scan (not just per-artist refresh) behavior
- [ ] What happens when platform has `lockdata=true` set on an artist
