---
description: How Stillwater queries metadata providers, the per-field priority chain, and how results merge.
---

<!-- code: internal/provider/provider.go (AllProviderNames, ProviderCapabilities, ProviderName constants), internal/provider/orchestrator.go (FetchMetadata, FetchImages, EnrichProviderIDs, fieldProviderExclusions, isAggregatedField, image-attempt gating), internal/provider/registry.go -->
<!-- displaced developer detail: per-fetch caching mechanism (one request per provider per fetch even when many fields ask for it), id-propagation extracting Discogs ID from MusicBrainz URL responses, image-error transient vs terminal distinction (preserves existing artwork on 5xx/timeout), fieldProviderExclusions map. These belong in godoc on internal/provider or a developer guide, not in this page. -->

# Providers

A **provider** is a metadata source: an external service Stillwater queries for an artist's biography, genres, images, or external IDs. Today Stillwater queries ten first-party metadata providers:

MusicBrainz, Wikipedia, Fanart.tv, AudioDB, Discogs, Last.fm, Wikidata, Deezer, Genius, Spotify.

DuckDuckGo supplements the chain as a web image search adapter (on-demand, opt-in).

The detailed capability matrix -- which fields each provider returns, rate limits, sign-up links -- lives in the [provider reference](../reference/providers.md). This page covers the *behavior*: how Stillwater chooses among them, in what order, and what happens when their answers disagree.

## Per-field priority

Provider priority is set **per field**. For each field (name, biography, genres, thumb, fanart, ...) you order the providers from most-trusted to least, and Stillwater walks that list when it needs that field.

- For text fields like biography, name, or born date, the **first provider with a non-empty answer wins** -- the rest aren't asked.
- For tag fields (genres, styles, moods) and image fields (thumb, fanart, logo, banner), **every provider contributes** and Stillwater merges, preserving the priority order so the highest-trusted source's contribution is listed first.

This split lets you pick "definitive" sources for some fields while pooling the diversity of others -- e.g., MusicBrainz for sort name, but genres pooled from MusicBrainz + Last.fm + Discogs.

## Per-library overrides

Priority lists can be set globally (the default) or overridden per library. A classical-music library might prefer Discogs and AudioDB; a regular library might prefer MusicBrainz and Last.fm. Stillwater uses the right list for the artist's library on each fetch.

<!-- SCREENSHOT: Settings > Providers > Priorities | state: global priorities + one library override | annotation: per-field priority chains -->

## Provider IDs feed each other

Most providers want their own ID, not a MusicBrainz ID. Discogs wants a Discogs ID; Spotify wants a Spotify ID. Stillwater stores every ID it knows on the artist record and feeds them down the chain so each provider gets the ID it can actually use. As IDs are discovered (a MusicBrainz response often contains a link to Discogs, for example), Stillwater grabs them and the next provider benefits. You don't have to back-fill IDs by hand.

## Web image search

DuckDuckGo is special: it's not a metadata provider, it's an image search adapter. It surfaces in the comparison view as an "I'll go look on the web" option for cases where curated providers don't have what you need. It runs only on demand, never as part of an automatic refresh.

## Auth tiers

Providers fall into four tiers:

- **Free** -- no key required. MusicBrainz, Wikipedia, Wikidata, Deezer, DuckDuckGo.
- **Free key** -- you sign up, you get a key, no charge. Fanart.tv, Discogs, Last.fm, Genius.
- **Freemium** -- a free tier with rate limits, paid tiers with more headroom. AudioDB.
- **Paid** -- credentials required, account is paid. Spotify.

Any keys you provide are encrypted at rest, scrubbed from logs, and only visible to you in the editor.

## What you don't need to think about

- **Repeated calls.** If many fields share a provider, that provider is asked once per refresh, not once per field.
- **Provider-specific IDs.** As Stillwater discovers them, they're stored and reused on the next refresh.
- **Transient outages.** A provider timeout doesn't blank your existing artwork; the field stays as-is until the provider is back.
- **Fields a provider can't supply.** Stillwater knows MusicBrainz won't return biography text and skips the call.

What you do think about: which providers you trust for which fields, whether to override priorities for a particular library, and whether to register API keys for the providers that need them.
