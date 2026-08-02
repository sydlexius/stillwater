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

### How Candidates Are Ordered

Candidates are ranked with the best match first, so the entry you want is usually at the top of the list. The ordering uses the album comparison where one was made -- a candidate whose releases line up with what's on disk outranks one that merely has a similar name -- and falls back to how closely the name matches your query, taking the artist's sort name into account as well as its display name. Providers are mixed together in one ranked list rather than grouped, so a Discogs result can appear above a MusicBrainz one when it is the better match.

### When Albums Weren't Compared

Each candidate in the search results can show how well its known albums line up with what's on disk, so you can confirm a match by discography rather than by name alone. Not every candidate carries that comparison, and the list tells you which case you are looking at rather than leaving you to guess.

Comparing albums means asking MusicBrainz for that candidate's releases, which is a rate-limited request. A broad search can return dozens of candidates, and comparing every one would leave you waiting on the search far longer than it is worth. So Stillwater compares the strongest candidates and marks the rest **Albums not compared**. Because ranking happens before the comparison, the candidates most likely to be right are the ones that get checked.

A candidate marked that way was not measured and found wanting -- it simply wasn't measured. Read it as "no information", never as a strike against the candidate. The same marker appears on a candidate with no MusicBrainz ID to look up (Discogs results have none) and on one whose lookup failed. If the entry you want is marked this way, refine the search so it ranks higher: adding a country, a year, or the artist's full sort name usually brings the right candidate into the compared group.

Sometimes Stillwater can't perform that comparison at all -- the artist has no folder path recorded yet, or the folder couldn't be read (a missing mount, a permissions problem, or a file where a directory was expected). That is a different situation from the one above: there, the candidate wasn't checked; here, nothing could be checked against. Every candidate is marked **Albums not checked**, and the album-comparison column shows no match count. If you're integrating against the API directly, this is the `local_albums_unavailable` field on the search response -- search for that name if you're looking for it in the wire format.

A missing album comparison in this state means "Stillwater could not look," not "this candidate has no matching albums." Don't read it as a strike against the candidate -- fall back to the artist name, MusicBrainz ID, and any other identifying details you have, and fix the underlying problem (add the artist's folder path, check the mount, check permissions) if you want album comparison back for future searches.

This is different from a candidate that genuinely has zero comparable albums, which Stillwater reports separately as "no local albums to compare" -- that case means the lookup succeeded and simply found nothing on disk, which is still useful signal.

`local_albums_unavailable` is additive: it's a new, optional field, so existing API clients are unaffected by its introduction. It's present, and true, only when the comparison couldn't be made -- it's omitted entirely otherwise, including when the lookup succeeded and simply found no albums. Absence is the normal case.

### Re-identifying an artist that is already linked

If an artist is linked to the wrong entry, use **Actions** > **Re-identify Artist**. You get the same search and the same candidate list.

Picking a match discards the MusicBrainz ID Stillwater currently holds -- you have said this artist is someone else, so that ID is wrong either way -- and stores the IDs from the entry you picked. A MusicBrainz candidate supplies a new MusicBrainz ID; a Discogs candidate supplies a Discogs ID and leaves the artist without a MusicBrainz ID until you link one. Provider IDs that the choice says nothing about (AudioDB, Wikidata, Deezer, Spotify) are kept: a wrong MusicBrainz ID does not make them wrong.

All of that happens at the moment you pick a match, not before. Until then the artist keeps every ID it had, so you can search, change the query, find nothing usable, or close the page entirely and the artist is left exactly as it was. There is no intermediate state where the artist has no identity at all.

**Re-identify also refreshes the metadata.** Picking a match runs a full refresh against the newly linked identity. The fields listed under [which values a refresh replaces](#which-values-a-refresh-replaces) are rewritten from the new entry, and unlike an ordinary refresh the Name and Sort name are updated too.

That is the point. If the artist was matched to the wrong entry, its origin, biography, and dates all came from the wrong artist and should not survive the correction.

The same lock rules apply, which is worth thinking about before relinking. A lock you set to protect a handwritten biography also keeps that biography after you have declared the artist to be someone else.

An artist-level lock skips the metadata refresh entirely. The identity is still relinked, so the artist ends up correctly identified with its old metadata intact.

## Refresh many artists

To run refreshes across a saved view or a whole library:

1. Open the artist list (or filter to the scope you want).
2. Click **Bulk actions** > **Refresh metadata**.
3. Confirm the scope. Stillwater queues the refresh; the first artists complete within seconds, the rest as the queue drains.

The bulk path runs one artist at a time per provider (so a slow provider doesn't fan out to its rate limit). Progress shows in the event banner; you can keep using Stillwater while it runs.

### Repairing values that were stored badly

Refreshing is also how you correct a field that was stored in a bad shape. Text fields such as **Origin** and **Years active** are replaced whenever a provider returns a value for them. Re-running a refresh on one artist, in bulk, or as part of re-identifying therefore overwrites the stored text rather than leaving it in place.

Two things bound this:

- **Locked fields are still skipped.** If you already corrected a field by hand and pinned it, the refresh leaves your version alone. That is usually what you want, but it also means a field you locked keeps its old value -- unlock it first if you want the provider's.
- **The provider has to return something.** A field no enabled provider supplies stays as it is; an empty response never clears a stored value.

When repairing in bulk, filter the artist list down to the affected artists first rather than refreshing the whole library, so you can check the result on a scope small enough to read.

## Which values a refresh replaces

A refresh is not additive. For most fields, a provider that returns a value **replaces** what is stored, including a value you typed yourself. Locks are what stop that, so it is worth knowing which fields behave which way before you run one across a library.

**Replaced whenever a provider returns something:** Origin, Years active, Type, Gender, Disambiguation. If any enabled provider has a value, it wins over the stored one. A provider returning nothing leaves the stored value alone.

**Replaced only when the provider was asked and answered:** Biography, Genres, Styles, Moods, Born, Formed, Died, Disbanded. These can also be *cleared* when a provider is asked for the field and authoritatively returns nothing, so they follow the provider more closely than the group above.

**Genres, Styles, and Moods are replaced, not accumulated.** This is worth stating plainly because it is easy to assume otherwise: the first provider in the field's priority order that returns any tags wins, and its list replaces whatever the artist had. Tags from lower-priority providers are not appended, and tags you added by hand are not kept alongside them. If you have curated a tag list, lock the field.

**Filled only when empty:** the provider IDs (MusicBrainz, Discogs, Deezer, and so on). Once an ID is set, a refresh will not swap it. Changing one is what re-identify is for.

**Never touched by a refresh:** Name and Sort name. Re-identify updates these deliberately, because saying "this is a different artist" is a statement about identity; an ordinary refresh does not.

## Keeping a value you curated

If you have corrected a field by hand and want it to survive, **lock it**. Locks are the only thing that stops an automated write:

- **Field lock** -- pins one field. Everything else on the artist still refreshes normally. This is the right tool when you have fixed one wrong origin or written your own biography.
- **Artist lock** -- skips the artist entirely. A refresh will not run on it at all.

Both are described in [field locks](../core-concepts/field-locks.md), and the [edit-artist how-to](edit-artist.md) shows where the padlocks live in the UI.

The corollary matters just as much: a locked field keeps its stored value even when that value is the one you wanted to be fixed. If you locked an origin and later want the provider's version, unlock it first, then refresh.

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
