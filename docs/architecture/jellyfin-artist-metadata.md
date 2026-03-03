# Jellyfin -- Artist Metadata Schema

Reference document for Stillwater's Jellyfin integration. Covers API endpoints,
metadata fields, image types, NFO handling, and how Stillwater maps to each.

Sources: [api.jellyfin.org](https://api.jellyfin.org/),
[jellyfin.org/docs](https://jellyfin.org/docs/),
Jellyfin source code on GitHub (authoritative, since Jellyfin is open source).

---

## 1. API Endpoints

### Artist Discovery

| Method | Path | Purpose |
|--------|------|---------|
| GET | `/Artists` | List all artists in a library |
| GET | `/Artists/AlbumArtists` | List album artists only |
| GET | `/Artists/{Name}` | Get artist by name |

**Key query parameters** (shared by both list endpoints):

- `parentId` -- scope to a specific library
- `startIndex`, `limit` -- pagination
- `searchTerm` -- text search
- `fields` -- additional fields to include (comma-separated):
  `Overview`, `Genres`, `SortName`, `ProviderIds`, `Path`, `DateCreated`,
  `PrimaryImageAspectRatio`, `Studios`, `Taglines`, `People`
- `sortBy` -- e.g. `SortName`, `DateCreated`, `PremiereDate`, `Random`
- `sortOrder` -- `Ascending` or `Descending`
- `enableImages`, `enableTotalRecordCount`
- `genres`, `genreIds`, `tags`, `years` -- category filters

### Item Detail and Update

| Method | Path | Purpose | Auth |
|--------|------|---------|------|
| GET | `/Users/{UserId}/Items/{ItemId}` | Full item detail (user-scoped) | User |
| POST | `/Items/{ItemId}` | Update item metadata | Admin |

### Image Management

| Method | Path | Purpose |
|--------|------|---------|
| GET | `/Items/{Id}/Images` | List all images for an item |
| GET | `/Items/{Id}/Images/{Type}` | Download image by type |
| GET | `/Items/{Id}/Images/{Type}/{Index}` | Download image by type and index |
| POST | `/Items/{Id}/Images/{Type}` | Upload image (binary body; set Content-Type, e.g. image/jpeg) |
| POST | `/Items/{Id}/Images/{Type}/{Index}` | Upload image at specific index (same binary body + Content-Type) |
| DELETE | `/Items/{Id}/Images/{Type}` | Delete image |
| DELETE | `/Items/{Id}/Images/{Type}/{Index}` | Delete image at index |
| POST | `/Items/{Id}/Images/{Type}/{Index}/Index?newIndex={N}` | Reorder images |

### Library Management

| Method | Path | Purpose |
|--------|------|---------|
| GET | `/Library/VirtualFolders` | Discover music libraries |
| POST | `/Library/Refresh` | Trigger full library scan |
| POST | `/Items/{Id}/Refresh` | Refresh specific artist |

---

## 2. BaseItemDto Fields (Artist-Relevant)

### Core Identity

| Field | Type | Notes |
|-------|------|-------|
| `Id` | GUID | Internal Jellyfin item ID |
| `Name` | string | Display name |
| `OriginalTitle` | string | Original title if different from Name |
| `SortName` | string | Auto-generated sort name |
| `ForcedSortName` | string | Manually-set sort name (overrides SortName) |
| `Type` | BaseItemKind | `MusicArtist` for artists |
| `Path` | string | Filesystem path to artist directory |

### Content Metadata

| Field | Type | Notes |
|-------|------|-------|
| `Overview` | string | Biography/description text |
| `Genres` | []string | Music genres |
| `GenreItems` | []NameGuidPair | Genres with IDs |
| `Tags` | []string | User tags |
| `Taglines` | []string | Taglines |
| `ProductionYear` | int | Formation/birth year |
| `ProductionLocations` | []string | Locations |
| `CommunityRating` | float | Community rating |
| `CriticRating` | float | Critic rating |
| `OfficialRating` | string | Content rating |
| `CustomRating` | string | Custom rating |
| `Studios` | []NameGuidPair | Studios/labels |

### Dates

| Field | Type | Format | Notes |
|-------|------|--------|-------|
| `PremiereDate` | DateTime? | ISO 8601 | Formation date (groups) or birth date (persons) |
| `EndDate` | DateTime? | ISO 8601 | Disbanded date (groups) or death date (persons) |
| `DateCreated` | DateTime? | ISO 8601 | When item was created in Jellyfin |
| `ProductionYear` | int? | Year integer | Year formed/born |

**Date format requirements for POST /Items/{id}:**

Jellyfin uses .NET `DateTimeOffset` for date parsing. The `UpdateItem` method
calls `NormalizeDateTime()` to convert all dates to UTC. Behavior is the same
as Emby:

| Format | Result |
|--------|--------|
| `2006-06-15T00:00:00.0000000Z` | Stored |
| `2006-06-15` | Stored (normalized to UTC) |
| `2006` | Silently discarded |
| `2006-06` | Silently discarded |
| Free-text strings | Silently discarded |

### External Provider IDs

The `ProviderIds` field is a `Dictionary<string, string>`. For music artists,
Jellyfin supports these built-in providers:

| ProviderIds Key | NFO Element | Source |
|-----------------|-------------|--------|
| `MusicBrainzArtist` | `<musicbrainzartistid>` | MusicBrainz |
| `AudioDbArtist` | `<audiodbartistid>` | TheAudioDB |

Like Emby, Jellyfin does **not** have built-in provider IDs for Discogs,
Wikidata, Deezer, or Spotify. Custom keys can be stored in ProviderIds but
will not be recognized by any Jellyfin metadata agent.

### Images

| Field | Type | Notes |
|-------|------|-------|
| `ImageTags` | Dictionary<ImageType, string> | Image type to cache-bust hash |
| `BackdropImageTags` | []string | Hashes for backdrop images |
| `ImageBlurHashes` | Dictionary<ImageType, Dictionary<string, string>> | LQIP blur hashes |
| `PrimaryImageAspectRatio` | double? | Aspect ratio |

### Metadata Lock

| Field | Type | Notes |
|-------|------|-------|
| `IsLocked` | bool | Whether metadata is locked (called `LockData` in Emby) |
| `LockedFields` | []MetadataField | Specific fields locked from refresh |

### Writable Fields via POST /Items/{id}

Source: `Jellyfin.Api/Controllers/ItemUpdateController.cs`

The update endpoint explicitly copies these fields from the request body:

`Name`, `ForcedSortName`, `OriginalTitle`, `CriticRating`, `CommunityRating`,
`IndexNumber`, `ParentIndexNumber`, `Overview`, `Genres`, `Studios`,
`Taglines` (first entry only), `DateCreated`, `EndDate`, `PremiereDate`,
`ProductionYear`, `OfficialRating`, `CustomRating`, `Tags`,
`ProductionLocations`, `PreferredMetadataCountryCode`,
`PreferredMetadataLanguage`, `AspectRatio`, `DisplayOrder`, `Video3DFormat`,
`IsLocked`, `LockedFields`, `ProviderIds`

---

## 3. Image Types

### ImageType Enum (Artist-Relevant)

| Enum Value | Int | Artist-Relevant | Notes |
|------------|-----|-----------------|-------|
| `Primary` | 0 | Yes | Main artist photo |
| `Art` | 1 | Yes | Clearart from Fanart.tv |
| `Backdrop` | 2 | Yes | Background/fanart (multiple via index) |
| `Banner` | 3 | Yes | Banner image |
| `Logo` | 4 | Yes | Logo overlay |
| `Thumb` | 5 | Yes | Thumbnail/landscape |
| `Disc` | 6 | No | Album disc art |
| `Box` | 7 | No | Box art |
| `Screenshot` | 8 | No | Obsolete |
| `Menu` | 9 | No | Menu image |
| `Chapter` | 10 | No | Chapter image |
| `BoxRear` | 11 | No | Rear box art |
| `Profile` | 12 | No | User profile image |

### Image Filenames on Disk

Jellyfin looks for these files in the artist directory. Supported extensions:
`.png`, `.jpg`, `.jpeg`, `.webp`, `.tbn`, `.gif`, `.svg`

Source: `LocalImageProvider.cs`

| Image Type | Filenames (checked in order) |
|------------|------------------------------|
| Primary | `folder`, `poster`, `cover`, `jacket`, `default`, `albumart` |
| Art | `clearart` |
| Backdrop | `backdrop`, `fanart`, `background`, `art` (numbered: `fanart-1`, `backdrop2`, etc., up to 20) |
| Banner | `banner` |
| Logo | `logo`, `clearlogo` |
| Thumb | `landscape`, `thumb` |
| Disc | `cdart`, `disc`, `discart` |

**Multiple images:** Only Backdrop supports multiple images. Jellyfin checks
numbered variants up to 20 images, stopping after 3 consecutive missing files.

### Stillwater Image Type Mapping

| Stillwater Type | Jellyfin ImageType | Jellyfin Disk Filename |
|-----------------|---------------------|------------------------|
| `thumb` | `Primary` | `folder.jpg` |
| `fanart` | `Backdrop` | `backdrop.jpg` |
| `logo` | `Logo` | `logo.png` |
| `banner` | `Banner` | `banner.jpg` |

---

## 4. NFO File Handling

### Jellyfin DOES Read Artist NFOs

Unlike Emby, Jellyfin reads artist.nfo files via `BaseNfoParser`. The NFO
metadata plugin (`MediaBrowser.XbmcMetadata`) provides both reading and writing
for artists.

### NFO Read: Elements Parsed by Jellyfin

Source: `BaseNfoParser.cs` -- the switch statement that processes XML elements.

| XML Element | Maps To | Notes |
|-------------|---------|-------|
| `<name>`, `<title>`, `<localtitle>` | `item.Name` | |
| `<sortname>` | `item.SortName` | |
| `<sorttitle>` | `item.ForcedSortName` | |
| `<biography>`, `<plot>`, `<review>` | `item.Overview` | All three map to Overview |
| `<formed>`, `<aired>`, `<premiered>`, `<releasedate>` | `item.PremiereDate` | All four map to PremiereDate |
| `<enddate>` | `item.EndDate` | |
| `<year>` | `item.ProductionYear` | Must be > 1850 |
| `<genre>` | `item.AddGenre()` | Multiple allowed, split on `/` |
| `<style>`, `<tag>` | `item.AddTag()` | Both map to Tags |
| `<rating>` | `item.CommunityRating` | Float |
| `<criticrating>` | `item.CriticRating` | Float |
| `<studio>` | `item.AddStudio()` | |
| `<country>` | `item.ProductionLocations` | Split on `/` |
| `<tagline>` | `item.Tagline` | |
| `<mpaa>` | `item.OfficialRating` | |
| `<customrating>` | `item.CustomRating` | |
| `<language>` | `item.PreferredMetadataLanguage` | |
| `<countrycode>` | `item.PreferredMetadataCountryCode` | |
| `<originaltitle>` | `item.OriginalTitle` | |
| `<lockdata>` | `item.IsLocked` | Boolean |
| `<lockedfields>` | `item.LockedFields` | Pipe-delimited |
| `<dateadded>` | `item.DateCreated` | Format: `yyyy-MM-dd HH:mm:ss` |
| `<uniqueid type="...">` | `item.ProviderIds[type]` | Provider ID via type attribute |
| `<musicbrainzartistid>` | `item.ProviderIds["MusicBrainzArtist"]` | |
| `<audiodbartistid>` | `item.ProviderIds["AudioDbArtist"]` | |
| `<thumb>` | Remote images | `aspect` attribute determines type |
| `<fanart>` | Remote images | Contains child `<thumb>` elements |

### Elements Jellyfin Does NOT Parse

These Kodi/Stillwater NFO elements are **ignored** by Jellyfin:

| XML Element | Notes |
|-------------|-------|
| `<type>` | Artist type (person/group) -- Kodi-only |
| `<gender>` | Artist gender -- Kodi-only |
| `<disambiguation>` | MusicBrainz disambiguation -- Kodi-only |
| `<yearsactive>` | Years active range -- Kodi-only |
| `<instruments>` | Instruments played -- Kodi-only |
| `<born>` | Birth description -- Kodi-only (use `<formed>` or `<premiered>`) |
| `<died>` | Death description -- Kodi-only (use `<enddate>`) |
| `<mood>` | Moods -- Kodi-only |
| `<discogsartistid>` | No Jellyfin provider for Discogs |
| `<wikidataid>` | No Jellyfin provider for Wikidata |
| `<deezerartistid>` | No Jellyfin provider for Deezer |
| `<spotifyartistid>` | No Jellyfin provider for Spotify |

### NFO Write: Elements Jellyfin Produces

**From BaseNfoSaver (common):**

| XML Element | Source Field | MusicArtist-Specific |
|-------------|-------------|----------------------|
| `<biography>` | Overview | Yes -- uses `<biography>` instead of `<plot>` |
| `<title>` | Name | |
| `<originaltitle>` | OriginalTitle | |
| `<sorttitle>` | ForcedSortName | |
| `<formed>` | PremiereDate | Yes -- uses `<formed>` instead of `<premiered>` |
| `<enddate>` | EndDate | |
| `<genre>` | Genres | One per genre |
| `<style>` | Tags | Yes -- uses `<style>` instead of `<tag>` for MusicArtist |
| `<year>` | ProductionYear | |
| `<rating>` | CommunityRating | |
| `<criticrating>` | CriticRating | |
| `<mpaa>` | OfficialRating | |
| `<customrating>` | CustomRating | |
| `<tagline>` | Taglines[0] | |
| `<studio>` | Studios | |
| `<country>` | ProductionLocations | |
| `<lockdata>` | IsLocked | |
| `<lockedfields>` | LockedFields | Pipe-delimited |
| `<dateadded>` | DateCreated | Format: `yyyy-MM-dd HH:mm:ss` |
| `<language>` | PreferredMetadataLanguage | |
| `<countrycode>` | PreferredMetadataCountryCode | |
| `<musicbrainzartistid>` | ProviderIds["MusicBrainzArtist"] | |
| `<audiodbartistid>` | ProviderIds["AudioDbArtist"] | |

**From ArtistNfoSaver (artist-specific):**

| XML Element | Source | Notes |
|-------------|--------|-------|
| `<disbanded>` | EndDate | Duplicate of `<enddate>` from base saver |
| `<album>` | Discography | Container with `<title>` and `<year>` children |

### Key NFO Behavioral Notes

- **Tags vs Styles**: When writing, `item.Tags` are saved as `<style>` for
  MusicArtist. When reading, both `<style>` and `<tag>` are parsed into
  `item.Tags`. This means styles and tags are merged in Jellyfin.
- **Overview vs Biography**: For MusicArtist, `item.Overview` is written as
  `<biography>`. When reading, `<biography>`, `<plot>`, and `<review>` all map
  to `item.Overview`.
- **Formed vs PremiereDate**: For MusicArtist, `item.PremiereDate` is written
  as `<formed>`. When reading, `<formed>`, `<aired>`, `<premiered>`, and
  `<releasedate>` all map to `item.PremiereDate`.
- **Date format**: Configurable via `XbmcMetadataOptions.ReleaseDateFormat`,
  default `"yyyy-MM-dd"`. DateAdded uses fixed format `"yyyy-MM-dd HH:mm:ss"`.
- **Genre caveat**: Jellyfin docs state "genre, tag, and style tags will be
  ignored for music albums and music artists" but the parser code does process
  them. This appears to be a documentation error.

---

## 5. NFO Element Comparison: Jellyfin vs Kodi vs Stillwater

| NFO Element | Kodi Reads | Jellyfin Reads | Jellyfin Writes | Stillwater Writes |
|-------------|------------|----------------|-----------------|-------------------|
| `<name>` | Yes | Yes | No (uses `<title>`) | Yes |
| `<title>` | Yes | Yes | Yes | No |
| `<sortname>` | Yes | Yes | No (uses `<sorttitle>`) | Yes |
| `<sorttitle>` | Yes | Yes | Yes | No |
| `<type>` | Yes | No | No | Yes |
| `<gender>` | Yes | No | No | Yes |
| `<disambiguation>` | Yes | No | No | Yes |
| `<musicbrainzartistid>` | Yes | Yes | Yes | Yes |
| `<audiodbartistid>` | No | Yes | Yes | Yes |
| `<discogsartistid>` | No | No | No | Yes |
| `<wikidataid>` | No | No | No | Yes |
| `<deezerartistid>` | No | No | No | Yes |
| `<spotifyartistid>` | No | No | No | Yes |
| `<genre>` | Yes | Yes | Yes | Yes |
| `<style>` | Yes | Yes (as Tag) | Yes (from Tags) | Yes |
| `<mood>` | Yes | No | No | Yes |
| `<yearsactive>` | Yes | No | No | Yes |
| `<born>` | Yes | No | No | Yes |
| `<formed>` | Yes | Yes (as PremiereDate) | Yes | Yes |
| `<died>` | Yes | No | No | Yes |
| `<disbanded>` | Yes | No (reads `<enddate>`) | Yes | Yes |
| `<enddate>` | No | Yes | Yes | No |
| `<biography>` | Yes | Yes (as Overview) | Yes | Yes |
| `<plot>` | No | Yes (as Overview) | No | No |
| `<thumb>` | Yes | Yes (remote images) | No | No |
| `<fanart>` | Yes | Yes (remote images) | No | No |
| `<album>` (discography) | Yes | No | Yes | No |
| `<dateadded>` | No | Yes | Yes | No |
| `<lockdata>` | No | Yes | Yes | No |
| `<lockedfields>` | No | Yes | Yes | No |
| `<rating>` | Yes | Yes | Yes | No |
| `<criticrating>` | No | Yes | Yes | No |

---

## 6. Metadata Push Mapping (POST /Items/{id})

### Current Stillwater Implementation

| ArtistPushData Field | Jellyfin JSON Field | Notes |
|----------------------|---------------------|-------|
| Name | `Name` | Always sent |
| SortName | `ForcedSortName` | Sent if non-empty |
| Biography | `Overview` | |
| Genres | `Genres` | |
| Styles + Moods | `Tags` | Combined into single array |
| MusicBrainzID | `ProviderIds["MusicBrainzArtist"]` | |
| Born or Formed | `PremiereDate` | Born preferred over Formed |
| Died or Disbanded | `EndDate` | Died preferred over Disbanded |

### Fields NOT Pushed (No Jellyfin Equivalent)

| ArtistPushData Field | Reason |
|----------------------|--------|
| Disambiguation | No corresponding Jellyfin field |
| YearsActive | No corresponding Jellyfin field |

### Known Issues and Gaps

**1. Date format silently dropped (Issue #355)**

Same behavior as Emby. Free-text dates are silently discarded. Only ISO 8601
dates (`yyyy-MM-dd` or full precision) are stored.

**2. ProviderIds should always be sent**

Jellyfin's update controller filters out empty/null provider IDs, so sending
an empty map is safe. But Stillwater only sends ProviderIds when MusicBrainzID
is non-empty, potentially clearing provider IDs for artists without MBID.

**3. AudioDB provider ID not pushed**

Stillwater stores AudioDBID but does not include `"AudioDbArtist"` in
ProviderIds when pushing to Jellyfin.

**4. Art/Clearart image type not mapped**

Jellyfin supports `Art` (clearart) as an image type, available from Fanart.tv.
Stillwater does not handle this type.

**5. Thumb image type not mapped**

Jellyfin distinguishes `Primary` from `Thumb` (landscape). Stillwater maps
`thumb` to `Primary`, which is correct, but Jellyfin's `Thumb` type has no
Stillwater equivalent.

**6. NFO elements Stillwater writes but Jellyfin ignores**

Stillwater writes `<born>`, `<died>`, `<type>`, `<gender>`, `<disambiguation>`,
`<yearsactive>`, `<mood>` to NFO. Jellyfin ignores all of these. This is not
a bug (the data is still useful for Kodi and Stillwater's own database) but
means these fields do not round-trip through Jellyfin.

**7. Stillwater uses `<name>` and `<sortname>`, Jellyfin prefers `<title>` and `<sorttitle>`**

Jellyfin reads both, so this works. But Jellyfin writes `<title>` and
`<sorttitle>`. If both Stillwater and Jellyfin write the same NFO, the file
will contain duplicate name/sort fields under different element names.

**8. Stillwater does not write `<enddate>`, Jellyfin does not read `<disbanded>`**

Stillwater writes `<disbanded>` (Kodi convention). Jellyfin reads `<enddate>`
but not `<disbanded>`. Jellyfin writes both `<enddate>` and `<disbanded>`. If
Jellyfin's NFO saver is disabled and only Stillwater writes NFO, the EndDate
will not round-trip through Jellyfin's NFO reader.

---

## 7. Platform Profile: Image Filenames

### Seeded Jellyfin Profile

```json
{
  "thumb": ["folder.jpg"],
  "fanart": ["backdrop.jpg"],
  "logo": ["logo.png"],
  "banner": ["banner.jpg"]
}
```

### What Jellyfin Actually Expects on Disk

| Stillwater Type | Jellyfin Primary Filename | Jellyfin Also Accepts | Profile Correct? |
|-----------------|--------------------------|----------------------|------------------|
| thumb (Primary) | `folder.*` | `poster.*`, `cover.*`, `jacket.*`, `default.*`, `albumart.*` | Yes |
| fanart (Backdrop) | `backdrop.*` | `fanart.*`, `background.*`, `art.*` | Yes |
| logo (Logo) | `logo.*` | `clearlogo.*` | Yes |
| banner (Banner) | `banner.*` | -- | Yes |

The seeded Jellyfin profile filenames are correct.

---

## 8. Differences from Emby

| Aspect | Emby | Jellyfin |
|--------|------|----------|
| NFO read (artist) | Does NOT read artist.nfo | Reads artist.nfo |
| Item ID type | String | GUID |
| Lock field name | `LockData` (bool) | `IsLocked` (bool) |
| Image extensions | `.jpg`, `.jpeg`, `.png`, `.tbn` | `.png`, `.jpg`, `.jpeg`, `.webp`, `.tbn`, `.gif`, `.svg` |
| Primary filenames | `folder`, `poster`, `cover`, `default` | `folder`, `poster`, `cover`, `jacket`, `default`, `albumart` |
| Logo filenames | `logo` | `logo`, `clearlogo` |
| Backdrop limit | Configurable (default ~10) | Up to 20 (stops after 3 consecutive missing) |
| Blur hashes | Not available | `ImageBlurHashes` field on BaseItemDto |
| MusicBrainz mirror | `musicbrainz.emby.tv` (Emby-hosted) | Default MusicBrainz API |
| Open source | No | Yes (authoritative source code) |

---

## 9. Active Metadata Providers

Jellyfin uses these providers to fetch music artist metadata:

| Provider | Data Supplied | Notes |
|----------|--------------|-------|
| MusicBrainz | Name, ProductionYear, PremiereDate | Direct MBID lookup, name search, diacritical fallback |
| TheAudioDB | Overview (localized), Genres, Provider IDs, Images (Primary, Logo, Banner, up to 3 Backdrops) | Requires MBID; Patreon API key for higher limits |
| Fanart.tv (plugin) | Art, Logo, Banner, Backdrop, Primary | HD and SD variants; requires API key |
