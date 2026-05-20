---
description: Set the per-field provider priority list, globally and per-library, plus when to override.
---

<!-- code: internal/api/router.go (provider priority endpoints), internal/provider/orchestrator.go (FetchMetadata walks per-field priorities), web/templates/settings.templ providers tab (drag-reorderable priority chips). -->

# Configure provider priorities

Stillwater asks providers for metadata in **per-field priority order**. The order decides which provider's biography wins, which provider supplies the primary thumb, and how aggregated fields like genres are merged. This page covers setting that order globally and overriding it per library.

For the *behavior* (first-match wins for text, every-source contributes for tags and images), see [providers in core concepts](../core-concepts/providers.md).

## Set the global priority

1. Go to **Settings > Providers**.
2. Scroll to **Provider priorities**.
3. Pick a field from the dropdown (biography, name, sortname, genres, thumb, fanart, ...).
4. The current priority list shows as drag-reorderable chips. Drag chips up or down to change the order.
5. Changes save automatically as you drop.

The order is **most-trusted at the top**. For first-match-wins fields, only the topmost responsive provider matters. For aggregated fields, every provider's contribution shows up but the priority order decides which contribution is listed first.

<!-- SCREENSHOT: Settings > Providers > Priorities | state: priority chips for the biography field with Last.fm at top, Wikipedia second, AudioDB third | annotation: drag handles + per-field selector -->

## Per-library override

When one library deserves a different priority list from another (a classical-music library that prefers Discogs over Last.fm, say):

1. Go to **Settings > Libraries**.
2. Find the library you want to customize.
3. Open **Provider overrides** for that library.
4. The list shows the global priorities by default. Change the order for any field; saved overrides are highlighted.
5. Click **Reset to global** on a field to drop the override and inherit the global order.

Refreshes for artists in that library now use the override. Other libraries continue to use the global list.

## What to put where

A few opinionated starting points:

### For text fields

- **Biography:** Last.fm (rich) > Wikipedia (factual) > AudioDB (curated). Skip MusicBrainz -- it doesn't return biography text.
- **Sort name:** MusicBrainz > AudioDB. MusicBrainz's sort names are the closest to Kodi's expectations.
- **Disambiguation:** MusicBrainz only -- the others don't have a comparable concept.
- **Genres:** MusicBrainz first (curated tags) followed by Last.fm (popular tags) and Discogs (style-as-genre). Aggregated, so all three contribute.

### For images

- **Thumb:** Fanart.tv > AudioDB > Discogs. Fanart.tv has the strongest curated catalogue.
- **Fanart:** Fanart.tv first by a wide margin.
- **Logo:** Fanart.tv. Logos are nearly all theirs.
- **Banner:** Fanart.tv. Same.

These are starting points -- adjust as you see what your collection actually pulls.

## Disable a provider entirely

Set the provider's enable toggle to off (top of the Providers tab). Disabled providers are skipped in every priority list automatically; you don't have to remove the chips from each field. Re-enabling restores the chips' positions.

## What changes after a priority edit

Priority edits don't trigger refreshes on their own. The new order applies to the next refresh you run. To see the impact:

- For one artist: open the artist and click **Refresh**. The Sources panel shows which provider supplied each field after the refresh.
- For a library: run a bulk refresh (see [refresh metadata](refresh-metadata.md)).

If you only changed priorities for image fields, the metadata refresh won't move artwork -- run a bulk **Fetch images** instead.

## Two related knobs

### Metadata languages

Under Settings > Providers > Metadata languages: a multi-select with autocomplete. Drives the "Artist name matches preferred language" rule, influences which alias MusicBrainz promotes when refreshing names, and selects the language for genre, style, and mood tags. When the same genre arrives from two providers in different languages, Stillwater keeps the form in your first listed language instead of storing both as separate tags. Set this to your preferred locale list (e.g., `en, ja, ko` for an English-first collection that should use original-language names where they exist).

### Name similarity threshold

A slider under Advanced settings on the Providers tab. Lower = fuzzier matching when Stillwater compares an artist name to provider search results; higher = stricter. Defaults to a sensible middle. Tune higher if you've seen wrong matches; lower if Stillwater is missing valid candidates.

## See also

- [Providers concept](../core-concepts/providers.md) for the priority + aggregation model.
- [Providers reference](../reference/providers.md) for the per-provider capability matrix.
- [Refresh metadata](refresh-metadata.md) to see priority changes take effect.
