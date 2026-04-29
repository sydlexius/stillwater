---
description: Self-hosted artist and composer metadata management for Emby, Jellyfin, and Kodi
hide:
  - navigation
  - toc
---

# Stillwater

Self-hosted artist and composer metadata management for Emby, Jellyfin, and Kodi.

Stillwater reads and writes NFO files, fetches and crops artist artwork, and pulls
metadata from MusicBrainz, Fanart.tv, and other providers. One web UI keeps your
music library metadata clean and consistent across every media server you run.

## Get started

- [Install Stillwater](getting-started/index.md) -- binary, Docker, or Docker Compose.
- [Connect your media server](getting-started/index.md) -- Emby and Jellyfin.
- [How-to guides](how-to/index.md) -- common tasks, step by step.
- [API reference](api/index.html) -- the full REST API at `/api/v1/`.

## Why Stillwater

- **API-first.** Every feature is reachable via REST. The web UI consumes the same
  API as your scripts and integrations.
- **Self-hosted, no cloud.** Runs as a single binary or container. Your data stays
  on your hardware.
- **Plays nicely with what you already have.** Drops in alongside Emby, Jellyfin,
  Lidarr, and Kodi.

## Project status

Stillwater shipped v1.0.0 on 2026-04-28. Active development continues; see the
[GitHub repository](https://github.com/sydlexius/stillwater) for releases and the
issue tracker.
