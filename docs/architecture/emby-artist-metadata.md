# Emby -- Artist Metadata Schema

Reference document for Stillwater's Emby integration. Covers API endpoints,
metadata fields, image types, NFO handling, and how Stillwater maps to each.

Sources: [dev.emby.media](https://dev.emby.media/), Emby community forums,
[swagger.emby.media](https://swagger.emby.media/?staticview=true),
[Emby Music Naming Guide](https://emby.media/support/articles/Music-Naming.html).

---

## 1. API Endpoints

### Artist Discovery

| Method | Path | Purpose |
|--------|------|---------|
| GET | `/Artists` | List all artists in a library |
| GET | `/Artists/AlbumArtists` | List album artists only |
| GET | `/Artists/{Name}` | Get artist by name |

**Key query parameters** (shared by both list endpoints):

- `ParentId` -- scope to a specific library
- `StartIndex`, `Limit` -- pagination
- `Recursive` -- include nested folders
- `Fields` -- additional fields to include (comma-separated):
  `Budget`, `Chapters`, `DateCreated`, `Genres`, `HomePageUrl`, `IndexOptions`,
  `MediaStreams`, `Overview`, `ParentId`, `Path`, `People`, `ProviderIds`,
  `PrimaryImageAspectRatio`, `Revenue`, `SortName`, `Studios`, `Taglines`
- `SortBy` -- e.g. `SortName`, `DateCreated`, `PremiereDate`, `Random`
- `SortOrder` -- `Ascending` or `Descending`
- `AnyProviderIdEquals` -- filter by provider ID (format: `ProviderName.value`)
- `EnableImages`, `EnableImageTypes`, `ImageTypeLimit` -- image control

### Item Detail and Update

| Method | Path | Purpose | Auth |
|--------|------|---------|------|
| GET | `/Users/{UserId}/Items/{ItemId}` | Full item detail (user-scoped) | User |
| GET | `/Items/{ItemId}/MetadataEditor` | Metadata editor info | Admin |
| POST | `/Items/{ItemId}` | Update item metadata | Admin |

### Image Management

| Method | Path | Purpose |
|--------|------|---------|
| GET | `/Items/{Id}/Images` | List all images for an item |
| GET | `/Items/{Id}/Images/{Type}` | Download image by type |
| GET | `/Items/{Id}/Images/{Type}/{Index}` | Download image by type and index |
| POST | `/Items/{Id}/Images/{Type}` | Upload image (base64-encoded body) |
| POST | `/Items/{Id}/Images/{Type}/{Index}` | Upload image at specific index |
| DELETE | `/Items/{Id}/Images/{Type}` | Delete image |
| DELETE | `/Items/{Id}/Images/{Type}/{Index}` | Delete image at index |

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
| `Id` | string | Internal Emby item ID |
| `Name` | string | Display name |
| `OriginalTitle` | string | Original title if different from Name |
| `SortName` | string | Auto-generated sort name |
| `ForcedSortName` | string | Manually-set sort name (overrides SortName) |
| `Type` | string | `"MusicArtist"` for artists |
| `Path` | string | Filesystem path to artist directory |

### Content Metadata

| Field | Type | Notes |
|-------|------|-------|
| `Overview` | string | Biography/description text |
| `Genres` | []string | Music genres |
| `Tags` | []string | User tags (Emby uses these for styles too) |
| `Taglines` | []string | Taglines |
| `ProductionYear` | int | Formation/birth year |
| `ProductionLocations` | []string | Locations |
| `CommunityRating` | float | Community rating |
| `CriticRating` | float | Critic rating |
| `OfficialRating` | string | Content rating (MPAA etc.) |
| `CustomRating` | string | Custom rating |

### Dates

| Field | Type | Format | Notes |
|-------|------|--------|-------|
| `PremiereDate` | string | ISO 8601 (`yyyy-MM-ddTHH:mm:ss.fffffffZ`) | Formation date (groups) or birth date (persons) |
| `EndDate` | string | ISO 8601 | Disbanded date (groups) or death date (persons) |
| `DateCreated` | string | ISO 8601 | When item was created in Emby |

**Date format requirements for POST /Items/{id}:**

| Format | Result |
|--------|--------|
| `2006-06-15T00:00:00.0000000Z` | Stored (full ISO 8601) |
| `2006-06-15` | Stored (normalized to ISO 8601) |
| `2006` | Silently discarded |
| `2006-06` | Silently discarded |
| `2006 in Cardiff, CA` | Silently discarded |
| `October 14, 1946` | Silently discarded |

### External Provider IDs

The `ProviderIds` field is a `map[string]string`. For music artists, Emby
supports these built-in providers:

| ProviderIds Key | NFO Element | Source |
|-----------------|-------------|--------|
| `MusicBrainzArtist` | `<musicbrainzartistid>` | MusicBrainz |
| `AudioDbArtist` | `<audiodbartistid>` | TheAudioDB |

Emby does **not** have built-in provider IDs for Discogs, Wikidata, Deezer,
or Spotify. Custom provider IDs can be stored in the `ProviderIds` dictionary,
but they will not be displayed in the Emby UI or used by any Emby metadata
agent.

### Images

| Field | Type | Notes |
|-------|------|-------|
| `ImageTags` | map[string]string | Image type to cache-bust hash |
| `BackdropImageTags` | []string | Array of hashes for backdrop images |
| `PrimaryImageAspectRatio` | float | Aspect ratio of primary image |

### Metadata Lock

| Field | Type | Notes |
|-------|------|-------|
| `LockData` | bool | Whether metadata is locked |
| `LockedFields` | []string | Specific fields locked from refresh |

**Lockable field names** (MetadataFields enum):
`AlbumArtists`, `Artists`, `Cast`, `ChannelNumber`, `Collections`,
`CommunityRating`, `Composers`, `CriticRating`, `Genres`, `Name`,
`OfficialRating`, `OriginalTitle`, `Overview`, `ProductionLocations`,
`Runtime`, `SortIndexNumber`, `SortName`, `SortParentIndexNumber`,
`Studios`, `Tagline`, `Tags`

---

## 3. Image Types

### ImageType Enum (Artist-Relevant)

| Enum Value | Artist-Relevant | Notes |
|------------|-----------------|-------|
| `Primary` | Yes | Main cover/poster image |
| `Art` | Yes | Clearart-style image |
| `Backdrop` | Yes | Background/fanart (supports multiple via index) |
| `Banner` | Yes | Banner image |
| `Logo` | Yes | Logo overlay image |
| `Thumb` | Yes | Thumbnail/landscape image |
| `Disc` | No | Album disc art |
| `Box` | No | Box art |
| `BoxRear` | No | Rear box art |
| `Chapter` | No | Chapter image |
| `Screenshot` | No | Screenshot |
| `Menu` | No | Menu image |

### Image Filenames on Disk

Emby looks for these files in the artist directory. Supported extensions:
`.jpg`, `.jpeg`, `.png`, `.tbn`

| Image Type | Filenames (checked in order) |
|------------|------------------------------|
| Primary | `folder`, `poster`, `cover`, `default` |
| Art | `clearart` |
| Backdrop | `backdrop`, `fanart`, `background`, `art` (numbered variants: `fanart-1`, `backdrop2`, etc.) |
| Banner | `banner` |
| Logo | `logo` |
| Thumb | `thumb`, `landscape` |

**Multiple images:** Only Backdrop supports multiple images per artist. Numbered
variants use the pattern `{name}{N}` or `{name}-{N}` (e.g., `fanart-1.jpg`,
`backdrop2.jpg`). An `extrafanart/` subdirectory is also scanned.

### Stillwater Image Type Mapping

| Stillwater Type | Emby ImageType | Emby Disk Filename |
|-----------------|----------------|---------------------|
| `thumb` | `Primary` | `folder.jpg` |
| `fanart` | `Backdrop` | `backdrop.jpg` |
| `logo` | `Logo` | `logo.png` |
| `banner` | `Banner` | `banner.jpg` |

---

## 4. NFO File Handling

### Critical: Emby Does NOT Read Artist NFOs

Per the Emby team (confirmed in community forums): **artist.nfo files are only
written by Emby, not read back in.** There is no `ArtistNfoParser` in Emby's
NfoMetadata plugin. The base NFO parser exists but is not wired up for the
MusicArtist item type.

This means:
- Emby **writes** artist.nfo (via ArtistNfoSaver and BaseNfoSaver)
- Emby **does NOT read** artist.nfo from disk
- Local metadata for artists is accessed only through the Emby API
- Stillwater's NFO files are authoritative for local metadata; Emby ignores them

### NFO Output Format (What Emby Writes)

Root element: `<artist>`

**Elements from BaseNfoSaver (common):**

| XML Element | BaseItemDto Field | Notes |
|-------------|-------------------|-------|
| `<biography>` | Overview | Used instead of `<plot>` for MusicArtist |
| `<title>` | Name | |
| `<originaltitle>` | OriginalTitle | |
| `<sorttitle>` | ForcedSortName | |
| `<formed>` | PremiereDate | Used instead of `<premiered>` for MusicArtist |
| `<enddate>` | EndDate | |
| `<genre>` | Genres | One element per genre |
| `<style>` | Tags | Used instead of `<tag>` for MusicArtist |
| `<year>` | ProductionYear | |
| `<rating>` | CommunityRating | |
| `<criticrating>` | CriticRating | |
| `<mpaa>` | OfficialRating | |
| `<customrating>` | CustomRating | |
| `<tagline>` | Taglines[0] | First tagline only |
| `<studio>` | Studios | One per studio |
| `<lockdata>` | LockData | Boolean |
| `<lockedfields>` | LockedFields | Pipe-delimited |
| `<dateadded>` | DateCreated | |
| `<language>` | PreferredMetadataLanguage | |
| `<countrycode>` | PreferredMetadataCountryCode | |
| `<{provider}id>` | ProviderIds[provider] | Dynamic, e.g., `<musicbrainzartistid>` |
| `<uniqueid type="...">` | ProviderIds | Alternative provider ID format |

**Elements from ArtistNfoSaver (artist-specific):**

| XML Element | Source | Notes |
|-------------|--------|-------|
| `<disbanded>` | EndDate | Artist disbanded date |
| `<album>` | Discography | Container with `<title>` and `<year>` children |

### NFO Element Comparison: Emby vs Kodi vs Stillwater

| NFO Element | Kodi Reads | Emby Writes | Emby Reads | Stillwater Writes |
|-------------|------------|-------------|------------|-------------------|
| `<name>` | Yes | No (uses `<title>`) | No | Yes |
| `<title>` | Yes | Yes | No | No |
| `<sortname>` | Yes | No (uses `<sorttitle>`) | No | Yes |
| `<sorttitle>` | Yes | Yes | No | No |
| `<type>` | Yes | No | No | Yes |
| `<gender>` | Yes | No | No | Yes |
| `<disambiguation>` | Yes | No | No | Yes |
| `<musicbrainzartistid>` | Yes | Yes | No | Yes |
| `<audiodbartistid>` | No | Yes | No | Yes |
| `<discogsartistid>` | No | No | No | Yes |
| `<wikidataid>` | No | No | No | Yes |
| `<deezerartistid>` | No | No | No | Yes |
| `<spotifyartistid>` | No | No | No | Yes |
| `<genre>` | Yes | Yes | No | Yes |
| `<style>` | Yes | Yes (from Tags) | No | Yes |
| `<mood>` | Yes | No | No | Yes |
| `<yearsactive>` | Yes | No | No | Yes |
| `<born>` | Yes | No | No | Yes |
| `<formed>` | Yes | Yes (from PremiereDate) | No | Yes |
| `<died>` | Yes | No | No | Yes |
| `<disbanded>` | Yes | Yes (from EndDate) | No | Yes |
| `<biography>` | Yes | Yes (from Overview) | No | Yes |
| `<thumb>` | Yes | No | No | No |
| `<fanart>` | Yes | No | No | No |
| `<album>` (discography) | Yes | Yes | No | No |

---

## 5. Metadata Push Mapping (POST /Items/{id})

### Current Stillwater Implementation

| ArtistPushData Field | Emby JSON Field | Notes |
|----------------------|-----------------|-------|
| Name | `Name` | Always sent |
| SortName | `ForcedSortName` | Sent if non-empty |
| Biography | `Overview` | |
| Genres | `Genres` | |
| Styles + Moods | `Tags` | Combined into single array |
| MusicBrainzID | `ProviderIds["MusicBrainzArtist"]` | |
| Born or Formed | `PremiereDate` | Born preferred over Formed |
| Died or Disbanded | `EndDate` | Died preferred over Disbanded |

### Fields NOT Pushed (No Emby Equivalent)

| ArtistPushData Field | Reason |
|----------------------|--------|
| Disambiguation | No corresponding Emby field |
| YearsActive | No corresponding Emby field |

### Known Issues and Gaps

**1. Date format silently dropped (Issue #355)**

Free-text dates (e.g., "2006 in Cardiff, CA") are silently discarded by Emby.
Only ISO 8601 dates (`yyyy-MM-dd` or full precision) are accepted.

**2. Partial update overwrites**

Emby's POST /Items/{id} does **not** support partial updates. Omitted fields
are overwritten with null/blank. Stillwater currently sends a partial body
(fields are `omitempty`), which means any field Stillwater does not send will
be cleared on the Emby side.

**3. ProviderIds is required**

Sending POST /Items/{id} without `ProviderIds` causes a 400 error. Even an
empty `{}` must be present. Stillwater only sends ProviderIds when
MusicBrainzID is non-empty, which may cause errors for artists without an MBID.

**4. SortName not locked**

Setting `ForcedSortName` via the API requires also including `"SortName"` in
the `LockedFields` array, otherwise Emby resets the sort name on the next
metadata refresh. Stillwater does not currently set LockedFields.

**5. Genres need GenreItems**

When updating genres via the API, both `Genres` (string array) and
`GenreItems` (NameIdPair array) should be sent. Same for Tags/TagItems.
Stillwater currently only sends the string arrays.

**6. AudioDB provider ID not pushed**

Stillwater stores AudioDBID in its database and writes `<audiodbartistid>` to
NFO, but does not include it in the ProviderIds map when pushing to Emby.

**7. Art/Clearart image type not mapped**

Emby supports an `Art` (clearart) image type. Stillwater does not currently
handle this type.

**8. Thumb image type not mapped**

Emby distinguishes `Primary` (poster/folder) from `Thumb` (landscape
thumbnail). Stillwater maps its `thumb` to `Primary`, which is correct for the
primary artist image, but Emby's `Thumb` type has no Stillwater equivalent.

---

## 6. Platform Profile: Image Filenames

### Seeded Emby Profile

```json
{
  "thumb": ["folder.jpg"],
  "fanart": ["backdrop.jpg"],
  "logo": ["logo.png"],
  "banner": ["banner.jpg"]
}
```

### What Emby Actually Expects on Disk

| Stillwater Type | Emby Primary Filename | Emby Also Accepts | Profile Correct? |
|-----------------|----------------------|-------------------|------------------|
| thumb (Primary) | `folder.*` | `poster.*`, `cover.*`, `default.*` | Yes |
| fanart (Backdrop) | `backdrop.*` | `fanart.*`, `background.*`, `art.*` | Yes |
| logo (Logo) | `logo.*` | -- | Yes |
| banner (Banner) | `banner.*` | -- | Yes |

The seeded Emby profile filenames are correct. Emby's preferred primary
filename for Backdrop is `backdrop.*`, which matches the profile. Note that
Emby also accepts `fanart.*` as a Backdrop filename, so artists migrating from
Kodi naming will still work.

---

## 7. Active Metadata Providers

Emby uses these providers to fetch music artist metadata:

| Provider | Data Supplied | Notes |
|----------|--------------|-------|
| MusicBrainz | Artist identification, basic metadata | Emby runs a mirror at `musicbrainz.emby.tv` |
| TheAudioDB | Biographies, images, genres | Optional Patreon API key for higher limits |
| Fanart.tv | Logos, clearart, banners, backgrounds | Personal API key recommended |
