---
description: Pull updated metadata from providers for one artist, a saved view, or a whole library.
---

<!-- code: internal/api/handlers_refresh.go (handleArtistRefresh, handleRefreshSearch, handleRefreshLink), internal/api/router.go (POST /api/v1/artists/{id}/refresh + /refresh/search + /refresh/link), internal/provider/orchestrator.go (FetchMetadata), internal/api/handlers_bulk*.go (bulk refresh), web/templates/artist.templ (Refresh button + disambiguation form). -->

# Refresh metadata

A **refresh** asks providers (MusicBrainz, Fanart.tv, Last.fm, etc.) for current metadata and merges what they return into the artist's record. Useful when you've just connected a new provider, when you've changed priorities, or when you suspect upstream data has been updated.

Three scopes: a single artist, a saved view, or a whole library.

## Refresh one artist

1. Open the artist's page.
2. Click **Refresh** at the top.

If Stillwater knows the artist's MusicBrainz ID, the refresh runs immediately. If it doesn't, you'll see a disambiguation prompt.

<!-- SCREENSHOT: Artist detail | state: refresh button + sources panel after a successful refresh | annotation: which providers contributed which fields -->

### When you get the disambiguation prompt

If the artist has no MusicBrainz ID yet, Stillwater needs to pick the right one before it can run a full refresh -- many providers won't accept name-only queries reliably.

1. Stillwater shows a search field with the artist's name pre-filled.
2. Adjust the query if needed (add the country, add a year, etc.).
3. Pick the matching entry from the search results. Stillwater shows MusicBrainz and Discogs candidates side by side, with album lists when available so you can confirm by discography.
4. Click **Link**. The MBID is saved to the artist record and the refresh runs.

This only happens once per artist -- after the link, future refreshes go straight through.

## Refresh many artists

To run refreshes across a saved view or a whole library:

1. Open the artist list (or filter to the scope you want).
2. Click **Bulk actions** > **Refresh metadata**.
3. Confirm the scope. Stillwater queues the refresh; the first artists complete within seconds, the rest as the queue drains.

The bulk path runs one artist at a time per provider (so a slow provider doesn't fan out to its rate limit). Progress shows in the event banner; you can keep using Stillwater while it runs.

## What a refresh does

For each artist:

1. Stillwater walks your **per-field provider priority list** (Settings > Providers > Priorities).
2. For each field that needs a value, it asks providers in order. First match wins for text fields; aggregated fields (genres, styles, moods, images) collect from every provider in the list.
3. **Locked fields are skipped** entirely. If you've pinned the biography, no provider can overwrite it on a refresh.
4. As IDs are discovered (a Discogs URL in MusicBrainz's response, for example), Stillwater learns them and feeds them to subsequent providers in the same refresh.
5. The artist record is updated. Source attributions appear in the "Sources" panel so you can see which provider supplied which field.
6. Rules re-evaluate against the new state, so the artist's health score reflects the refresh immediately.

## What a refresh doesn't do

- It doesn't write the NFO file. The artist's database record updates; the NFO is rewritten on save (manual or via fixer).
- It doesn't touch images that providers didn't return. A transient provider error during an image fetch leaves your existing artwork alone.
- It doesn't run on locked artists. A whole-artist lock blocks the refresh entirely.

## Why a field didn't update

If a refresh ran but a field you expected to change didn't, check three things:

- **Field lock.** Is the field pinned for that artist? Pinned fields are skipped.
- **Priority list.** Does any enabled provider supply that field? If MusicBrainz is the only provider listed for biography, biography won't update -- MusicBrainz doesn't return biography text.
- **Provider availability.** Was the provider down? The Sources panel shows attributions; missing entries indicate the provider didn't respond.

The [providers reference](../reference/providers.md) lists which fields each provider can supply.

## Refresh + scan together

A common bootstrapping sequence on a fresh library:

1. **Scan** to discover artists (see [run scans](run-scans.md)).
2. **Refresh** the library to populate metadata. Most artists will need MBID linking on first refresh; consider using bulk refresh after a quick pass linking the obvious ones.
3. **Run rules** (or wait for the scheduled run) to surface what still needs work.
4. **Fix-all** to apply the trivial repairs in one pass.

After that, subsequent refreshes are mostly fast -- the IDs are already learned, the priority list is stable, and providers usually return the same data they did last time.
