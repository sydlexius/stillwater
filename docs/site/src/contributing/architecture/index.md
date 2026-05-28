---
description: System topology, package map, and end-to-end scan flow for Stillwater contributors.
---

# Architecture overview

Stillwater is a containerized, self-hosted Go application. Its design is
API-first: every feature the web UI exposes is backed by a REST endpoint at
`/api/v1/`, and the browser talks to those endpoints via HTMX rather than a
custom JavaScript framework.

## System topology

```mermaid
flowchart TD
    Browser["Browser (HTMX)"]
    API["HTTP API (internal/api)"]
    DB["SQLite database (modernc.org/sqlite)"]
    FS["Local filesystem (music library)"]
    MediaServers["Emby / Jellyfin (media servers)"]
    Lidarr["Lidarr (library manager)"]
    Providers["MusicBrainz / Fanart.tv / etc."]

    Browser -->|REST + SSE| API
    API -->|read/write| DB
    API -->|NFO + image writes| FS
    API -->|metadata, image, and lock push| MediaServers
    API -->|folder-rename path push + write-back control| Lidarr
    Lidarr -->|artist roster + events (webhooks)| API
    API -->|fetch metadata + images| Providers
    FS -->|fsnotify events| API
```

The application runs as a single binary with no external process dependencies
beyond SQLite (embedded via pure-Go `modernc.org/sqlite`). Configuration is
loaded from environment variables and an optional YAML file at startup.

The external platform connections (`internal/connection`) are not all the same
shape. Emby and Jellyfin are media servers that Stillwater treats as push
targets: it sends curated artist metadata, images, and lock state to them.
Lidarr is different: it is an upstream source of truth for the library. Artists
originate in Lidarr, and its inbound webhooks (`internal/webhook`, handled in
`internal/api/handlers_inbound_webhook.go`) seed and re-evaluate them in
Stillwater on events like ArtistAdded and AlbumImport. The flow is not one-way,
though. When Stillwater renames an artist directory on disk, it pushes the new
path back to Lidarr so the source-of-truth record stays consistent with the
filesystem: `internal/publish.SyncRename` fans out to every connected platform
and, for a Lidarr connection, calls `lidarr.Client.UpdateArtistPath`
(`PUT /api/v1/artist/{id}`) with an optional verify-after-PUT round-trip.
Stillwater can also toggle Lidarr's own NFO and image write-back consumers so
they do not overwrite Stillwater's files.

## Package map

| Package | Role |
|---|---|
| `cmd/stillwater` | Main entry point; wires all services together and starts the server |
| `internal/api` | HTTP handlers, middleware, and the SSE event hub |
| `internal/artist` | Artist domain model, service, and repository interfaces |
| `internal/auth` | Session-based authentication |
| `internal/backup` | Scheduled database backup service |
| `internal/config` | Configuration loading from env and YAML |
| `internal/conflict` | Conflict detection and write-gate enforcement (coalesce, ledger) |
| `internal/connection` | External platform connections: Emby, Jellyfin, Lidarr |
| `internal/database` | SQLite setup and schema migrations |
| `internal/dbutil` | Shared database helpers (type conversions, nullable handling) |
| `internal/encryption` | AES-256-GCM encryption for stored API keys |
| `internal/event` | Channel-based in-process event bus |
| `internal/filesystem` | Atomic file writes using a tmp/bak/rename pattern |
| `internal/foreign` | Foreign artist file scanner and model |
| `internal/i18n` | Internationalization support |
| `internal/image` | Image fetch, crop, compare, and EXIF metadata |
| `internal/imagebridge` | Resolves artist IDs to platform-specific image URLs |
| `internal/langpref` | Per-user and per-locale language preferences |
| `internal/library` | Music library management |
| `internal/logging` | Log manager: levels, rotation, in-memory ring buffer |
| `internal/maintenance` | Scheduled maintenance tasks |
| `internal/nfo` | NFO file parser and writer |
| `internal/platform` | Platform naming profiles |
| `internal/provider` | Metadata source adapters: MusicBrainz, Fanart.tv, etc. |
| `internal/publish` | Unified publisher for NFO and platform writes |
| `internal/rule` | Rule engine (Bliss-inspired violation detection and fixing) |
| `internal/scanner` | Filesystem and API library scanners |
| `internal/scraper` | Configurable web scraping |
| `internal/server` | HTTPS / HTTP/3 listeners, ACME cert manager, BYO TLS |
| `internal/settingsio` | Application settings persistence |
| `internal/updater` | Self-updater with channel and semver gating |
| `internal/version` | Build version injected at link time via ldflags |
| `internal/watcher` | Filesystem watcher for library directories |
| `internal/webhook` | Webhook dispatcher |
| `web/components` | Reusable Templ components: badges, modals, toasts, icons |
| `web/templates` | Page-level Templ templates |
| `web/static` | Compiled CSS and vendored JS (HTMX, Cropper.js, Chart.js) |

## How a typical scan flows end-to-end

Understanding one complete flow ties the packages together. Here is what
happens when a user clicks "Run scan":

1. **HTTP handler** (`internal/api`) receives `POST /api/v1/scanner/run`. It
   delegates immediately to `internal/scanner.Service.Run`, which spawns a
   background goroutine and returns a 202 with the scan ID.

2. **Scanner** walks every configured library path. For each artist
   directory it finds, it reads the directory listing, probes for known image
   filenames (`folder.jpg`, `fanart.jpg`, `logo.png`, etc.), and parses
   `artist.nfo` if present. It uses a mtime fast-path to skip directories
   that have not changed since the last scan.

3. **Artist upsert**: for each discovered directory the scanner calls
   `internal/artist.Service` to create a new artist record or update an
   existing one. The `ThumbExists`, `FanartExists`, `LogoExists`, and
   `NFOExists` flags on the artist model are set here.

4. **Rule evaluation**: after upsert the scanner calls
   `internal/rule.Engine.Evaluate` for the artist. The engine runs every
   enabled rule's checker function against the artist. Violations are
   written back to the database by the rule service.

5. **Event bus**: once the full walk is complete the scanner publishes a
   `scan.completed` event to `internal/event.Bus`. The SSE hub
   (`internal/api`) fans this event out to all connected browsers, which then
   refresh the artist list without a full-page reload.

6. **Filesystem watcher** (`internal/watcher`) runs in parallel with the
   scanner. It uses `fsnotify` to watch library roots and triggers a new
   partial scan whenever a directory is created or removed, keeping the
   database up to date between explicit user-initiated scans.

For deeper dives into individual subsystems see the pages in this section:

- [Rule engine](rule-engine.md) -- how violations are detected and fixed
- [Scanner pipeline](scanner-pipeline.md) -- watcher, scanner, event bus, and publisher topology
- [Conflict gate](conflict-gate.md) -- how write-back conflicts are detected and enforced
