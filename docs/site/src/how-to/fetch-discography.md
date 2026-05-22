---
description: Pull release groups from MusicBrainz and write them into the artist's NFO discography.
---

<!-- code: internal/api/handlers_discography.go (handleFetchDiscography), internal/nfo/discography_merge.go (MergeDiscographyFromMBReleaseGroups), internal/api/router.go (POST /api/v1/artists/{id}/discography/fetch), web/templates/artist_discography.templ (ArtistDiscographyTab). -->

# Fetch discography

The **Discography** tab on an artist's page shows the `<album>` entries stored in the artist's NFO file -- the same records that Kodi, Emby, and Jellyfin read. The **Fetch discography** button pulls release groups from MusicBrainz and writes them into the NFO, so the discography stays in sync with what MusicBrainz knows about that artist.

## Before you start

- The artist must have a MusicBrainz ID. If the ID is missing, open the **Actions** menu on the artist's page and use **Identify Artist** to search providers and link the artist first. See [Refresh metadata](refresh-metadata.md) for the disambiguation step.
- The artist must have a filesystem path. Pathless artists have no NFO to write to.

## How to fetch

1. Open the artist's page.
2. Click the **Discography** tab.
3. Click **Fetch discography**.

Stillwater queries MusicBrainz for the artist's release groups, merges the results with any existing album entries in the NFO, and writes the updated file atomically.

When the fetch completes, the tab refreshes in place and shows a summary: how many albums were added and the total returned by MusicBrainz.

## What Stillwater keeps and what it adds

The merge follows a few straightforward rules:

- **Hand-added albums** (entries without a MusicBrainz release-group ID) are always kept at the top of the list, exactly as you entered them.
- **Existing albums with a MusicBrainz ID** are kept as-is regardless of whether MusicBrainz returned them in this response. A partial upstream response never silently removes albums you already have. If you refined a title or year by hand, your version is preserved.
- **New release groups** (those not yet in the NFO) are appended after the existing entries.

Running Fetch discography a second time is safe: it will not duplicate albums that are already present.

## Filtering by release type

By default, Stillwater includes Albums and EPs. To change which release types are fetched, append an `include` query parameter to the API call:

```
POST /api/v1/artists/{id}/discography/fetch?include=Album,EP,Single
```

The `include` parameter accepts any comma-separated release-type names. They are matched case-insensitively against each release group's MusicBrainz primary type, so common values are `Album`, `EP`, `Single`, `Compilation`, `Live`, and `Soundtrack`. The UI button always uses the default filter (Album and EP); the query parameter is for API or automation use.

## What happens to the NFO

When new albums are found, Stillwater:

1. Takes a snapshot of the existing NFO. The snapshot panel on the artist's page lists earlier versions; see [Discard accidental edits](edit-artist.md#discard-accidental-edits) for how to restore one.
2. Writes the updated NFO atomically using the same temp/rename pattern as all other NFO writes.
3. Stamps the NFO with the current Stillwater version and a write timestamp.

If no new albums are found, the NFO is left unchanged.

## Concurrent fetch protection

Only one fetch per artist can run at a time. If you trigger a second fetch while one is already in progress, the request is rejected with a conflict response. Wait for the first to complete and try again.

## See also

- [NFO files](../core-concepts/nfo-files.md) for how Stillwater reads and writes `artist.nfo`.
- [Refresh metadata](refresh-metadata.md) for pulling provider metadata into the artist record.
- [Edit an artist](edit-artist.md) for manually curating album entries or other fields.
