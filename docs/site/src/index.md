---
description: Curate artist metadata and images for your self-hosted music library. NFO writeback or direct API push.
hide:
  - navigation
---

# Stillwater

## One place to manage artist metadata, for every server you run.

Metadata gets clobbered between server refreshes. Image fetches come up empty for half your library. Edits on one server drift on another.

Stillwater curates the artist metadata for your music library and gets it where it needs to go: to the NFO file your library reads from, to Emby or Jellyfin via their APIs, or both. Per-library locking keeps your manual edits from being overwritten when servers refresh.

[Get started](getting-started/index.md){ .md-button .md-button--primary }
[See it on GitHub](https://github.com/sydlexius/stillwater){ .md-button }

---

## What it does

Stillwater is a self-hosted web app that manages artist metadata and images for music libraries. It runs alongside Emby, Jellyfin, Kodi, and Lidarr; it doesn't replace them.

**Two ways to deliver:**

- **NFO writeback (filesystem).** Mount your music library on the Stillwater host. Stillwater writes `artist.nfo` files your servers read. Highest fidelity. Captures fields Emby and Jellyfin don't even surface (Discography and others) but that Kodi reads natively.
- **Direct API push.** Connect Stillwater to Emby or Jellyfin and it pushes edits to them via their APIs. No mounted library required. Limited to fields each platform's API exposes, but atomic and good for the common case.

Use either, or both. Stillwater never touches your audio files; only metadata and artwork.

**Where the data comes from:**

Ten metadata providers with per-field fallback (MusicBrainz, Fanart.tv, Discogs, AudioDB, Spotify, and others), plus a web image search adapter (DuckDuckGo) for cases the metadata chain doesn't cover. [See the full provider matrix](reference/index.md) for what each source contributes.

---

## Why Stillwater

### Mainstream artists are the easy part.

Indie acts, regional artists, classical performers, names with non-Latin characters: that's where most tools come up blank. Stillwater pulls from ten different metadata providers, so when one doesn't have your artist, the others usually do.

### Run more than one media server? Stillwater handles all of them.

Emby, Jellyfin, and Kodi all read NFO files differently, and each one quietly rewrites the file in its own dialect on refresh. Stillwater writes one file all three read cleanly. If filesystem access isn't an option for some servers, Stillwater talks to each one's API directly.

### Built to run on your hardware.

Single binary or container. SQLite. No cloud, no telemetry, no account. Your library data stays where you put it.

---

## Get started

- [Install Stillwater](getting-started/index.md): binary, Docker, or Docker Compose.
- [Set up NFO writeback (preferred)](getting-started/first-run-oobe.md): point Stillwater at your music library.
- [Connect a media server](getting-started/index.md#3-pick-a-delivery-mode): the API-push alternative for Emby or Jellyfin.
- [Browse the API](api/index.html): every feature, scriptable.

---

## Project status

Stillwater shipped v1.0.0 on 2026-04-28. Active development continues; see the [issue tracker](https://github.com/sydlexius/stillwater/issues) for the roadmap.

[GPL-3.0](https://github.com/sydlexius/stillwater/blob/main/LICENSE) ·
[Releases](https://github.com/sydlexius/stillwater/releases) ·
[Contributing](https://github.com/sydlexius/stillwater/blob/main/CONTRIBUTING.md)
